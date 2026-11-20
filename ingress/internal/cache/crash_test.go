// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/ingress/internal/fsx/fsxtest"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

const (
	opCreate = "create tmp/.part-*"
	opWrite  = "write tmp/.part-*" //nolint:gosec // G101: the name of a recorded filesystem operation, not a credential
	opChmod  = "chmod tmp/.part-*"
	opSync   = "sync tmp/.part-*"
	opClose  = "close tmp/.part-*"

	opRenameDynamic = "rename tmp/.part-* -> dynamic/tierplan.yml"
	opRenameCommit  = "rename state/pending.json -> state/applied.json"
)

// tempWrite is the five operations that write one temporary file, then the rename into place.
func tempWrite(dst string) []string {
	return []string{opCreate, opWrite, opChmod, opSync, opClose, "rename tmp/.part-* -> " + dst}
}

// certPaths lists a bundle's certs/ paths in manifest path order.
func certPaths(b ingressbundle.Bundle) []string {
	var out []string
	for _, e := range b.Manifest.Files {
		if e.Step == ingressbundle.StepCerts {
			out = append(out, e.Path)
		}
	}
	return out
}

// seeded makes a real volume with the bundles applied in order, and returns its root.
func seeded(t *testing.T, bundles ...ingressbundle.Bundle) string {
	t.Helper()
	s, root := newPrepared(t)
	for _, b := range bundles {
		mustAccept(t, s, b)
	}
	return root
}

// recorded is a Store over root whose operations are recorded and can be failed, already loaded.
func recorded(t *testing.T, root string) (*Store, *fsxtest.Recorder) {
	t.Helper()
	rec := fsxtest.New(fsx.OS(), root)
	s := NewStore(rec, root)
	if _, ok, err := s.Load(); err != nil || !ok {
		t.Fatalf("Load before the apply: ok %v, err %v", ok, err)
	}
	return s, rec
}

// threeBundles is A, B and C over three different certificates.
func threeBundles(t *testing.T) (a, b, c ingressbundle.Bundle) {
	t.Helper()
	la, lb, lc := newLeafFiles(t, "a.test.local"), newLeafFiles(t, "b.test.local"), newLeafFiles(t, "c.test.local")
	return bundleOf(t, 1, la), bundleOf(t, 2, lb), bundleOf(t, 3, lc)
}

// TestApplyOrder pins the D-31 sequence of operations exactly. B over A keeps A's certificates (the previous
// bundle's) and removes nothing; C over B then removes A's, and only after the commit rename.
func TestApplyOrder(t *testing.T) {
	a, b, c := threeBundles(t)
	root := t.TempDir()
	rec := fsxtest.New(fsx.OS(), root)
	s := NewStore(rec, root)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	mustAccept(t, s, a)

	expected := func(newCerts, removed []string) []string {
		var want []string
		for _, p := range newCerts {
			want = append(want, tempWrite(p)...)
		}
		if len(newCerts) > 0 {
			want = append(want, "syncdir certs")
		}
		want = append(want, tempWrite("state/pending.json")...)
		want = append(want, "syncdir state")
		want = append(want, tempWrite("dynamic/tierplan.yml")...)
		want = append(want, "syncdir dynamic")
		want = append(want, opRenameCommit, "syncdir state")
		for _, p := range removed {
			want = append(want, "remove "+p)
		}
		if len(removed) > 0 {
			want = append(want, "syncdir certs")
		}
		return want
	}

	mark := len(rec.Ops())
	mustAccept(t, s, b)
	if got, want := rec.Ops()[mark:], expected(certPaths(b), nil); !slices.Equal(got, want) {
		t.Fatalf("B over A:\n got  %s\n want %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}

	mark = len(rec.Ops())
	mustAccept(t, s, c)
	removed := certPaths(a)
	slices.Sort(removed)
	if got, want := rec.Ops()[mark:], expected(certPaths(c), removed); !slices.Equal(got, want) {
		t.Fatalf("C over B:\n got  %s\n want %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}

	for _, op := range rec.Ops() {
		if strings.Contains(op, "dynamic/") && op != opRenameDynamic {
			t.Errorf("operation %q writes into dynamic/ other than by the one rename", op)
		}
		if strings.HasSuffix(op, ".yml") || strings.HasSuffix(op, ".yaml") || strings.HasSuffix(op, ".toml") {
			if !strings.Contains(op, "-> dynamic/tierplan.yml") {
				t.Errorf("operation %q puts a YAML name anywhere but the final rename", op)
			}
		}
	}
}

// TestCrashAfterEachStep stops the filesystem after every operation of an apply in turn, as a kill would,
// and has a new process load the volume: it must serve a verified bundle, the old one until the dynamic
// rename and the new one from then on.
func TestCrashAfterEachStep(t *testing.T) {
	a, old, next := threeBundles(t)

	// The dry run: the whole apply of next over old, to learn the operations and the two landmarks.
	root := seeded(t, a, old)
	s, rec := recorded(t, root)
	mark := len(rec.Ops())
	mustAccept(t, s, next)
	ops := rec.Ops()[mark:]
	dynRename := slices.Index(ops, opRenameDynamic)
	commitRename := slices.Index(ops, opRenameCommit)
	if dynRename < 0 || commitRename < 0 || len(ops) < 8 {
		t.Fatalf("the dry run did not show the apply: %v", ops)
	}
	commitSync := commitRename + 1 // the flush of state/ that follows the commit rename

	oldDynamic, newDynamic := dynamicOf(t, old), dynamicOf(t, next)
	for n := range ops {
		t.Run(fmt.Sprintf("kill-at-op-%02d", n), func(t *testing.T) {
			root := seeded(t, a, old)
			s, rec := recorded(t, root)
			rec.FailFrom(n)
			_, reason := s.Accept(frameOf(t, next), testInstance)
			if n <= commitSync && reason != ingressbundle.ReasonApplyFailed {
				t.Fatalf("killed before op %d (%s): Accept reason %q, want apply_failed", n, ops[n], reason)
			}
			if n > commitSync {
				if reason != "" {
					t.Fatalf("killed in the cleanup, op %d (%s): Accept reason %q, want none: the bundle was live", n, ops[n], reason)
				}
				if s.GCErr() == nil {
					t.Fatalf("killed in the cleanup, op %d (%s): no GCErr", n, ops[n])
				}
			}

			got, ok, err := NewStore(fsx.OS(), root).Load()
			if err != nil || !ok {
				t.Fatalf("a new process loads no bundle after a kill before op %d (%s): ok %v, err %v", n, ops[n], ok, err)
			}
			if err := Check(got); err != nil {
				t.Fatalf("the loaded bundle fails Check after a kill before op %d (%s): %v", n, ops[n], err)
			}
			want, wantDynamic := old.ID, oldDynamic
			if n > dynRename {
				want, wantDynamic = next.ID, newDynamic
			}
			if got.ID != want {
				t.Fatalf("a kill before op %d (%s) leaves bundle %s, want %s", n, ops[n], got.ID, want)
			}
			dyn, err := os.ReadDir(filepath.Join(root, "dynamic"))
			if err != nil || len(dyn) != 1 || dyn[0].Name() != "tierplan.yml" {
				t.Fatalf("dynamic/ = %v (%v), want exactly tierplan.yml", dyn, err)
			}
			raw, err := os.ReadFile(filepath.Join(root, "dynamic", "tierplan.yml")) //nolint:gosec // G304: a file in this test's own temporary volume
			if err != nil || string(raw) != wantDynamic {
				t.Fatalf("dynamic/tierplan.yml is neither whole old nor whole new content (%v)", err)
			}
		})
	}
}

// TestRenameEXDEV: a rename across devices fails the apply and nothing copies the file instead.
func TestRenameEXDEV(t *testing.T) {
	a, b, _ := threeBundles(t)
	root := seeded(t, a)
	s, rec := recorded(t, root)
	rec.RenameErr("dynamic/tierplan.yml", syscall.EXDEV)

	err := s.Apply(b)
	var ae *ApplyError
	if !errors.As(err, &ae) || ae.Step != "dynamic" || !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("Apply = %v, want an *ApplyError at step dynamic wrapping EXDEV", err)
	}
	if _, reason := s.Accept(frameOf(t, b), testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("Accept reason = %q, want apply_failed", reason)
	}
	for _, op := range rec.Ops() {
		if strings.Contains(op, "dynamic/") {
			t.Errorf("operation %q writes into dynamic/ although its only door, the rename, failed", op)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "dynamic", "tierplan.yml")) //nolint:gosec // G304: a file in this test's own temporary volume
	if err != nil || string(raw) != dynamicOf(t, a) {
		t.Fatalf("dynamic/tierplan.yml changed after a failed rename (%v)", err)
	}
	got, ok, err := NewStore(fsx.OS(), root).Load()
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("Load = (%s, %v, %v), want the previous bundle %s", got.ID, ok, err, a.ID)
	}
}
