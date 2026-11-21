// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// snapshot maps the path of every file under the given directories of root to its bytes.
func snapshot(t *testing.T, root string, dirs ...string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, d := range dirs {
		base := filepath.Join(root, d)
		err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(p) //nolint:gosec // G304: a file in this test's own temporary volume
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = raw
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot %s: %v", d, err)
		}
	}
	return out
}

func sameSnapshot(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, func(x, y []byte) bool { return bytes.Equal(x, y) })
}

// certNames lists the files in certs/, as bundle paths, sorted.
func certNames(t *testing.T, root string) []string {
	t.Helper()
	list, err := os.ReadDir(filepath.Join(root, "certs"))
	if err != nil {
		t.Fatalf("read certs/: %v", err)
	}
	var out []string
	for _, e := range list {
		out = append(out, "certs/"+e.Name())
	}
	slices.Sort(out)
	return out
}

// pathsOf lists the certs/ paths of leaves, sorted.
func pathsOf(leaves ...leaf) []string {
	var out []string
	for _, l := range leaves {
		out = append(out, l.certPath, l.keyPath)
	}
	slices.Sort(out)
	return out
}

// TestRejectLeavesVolumeUnchanged: every way a frame can be refused leaves certs/ and dynamic/ byte for
// byte as they were, the bundle in force is still the one applied before, and the code is the stable one.
func TestRejectLeavesVolumeUnchanged(t *testing.T) {
	s, root := newPrepared(t)
	applied := testBundle(t, "applied.test.local")
	mustAccept(t, s, applied)
	before := snapshot(t, root, "certs", "dynamic")

	type row struct {
		name   string
		want   string
		frame  func(t *testing.T) []byte
		wantID func() string // the ID the node names in rejected{id}; "" when the frame names none
	}
	var rows []row
	for _, r := range rejectRows() {
		b := r.build
		var built ingressbundle.Bundle
		rows = append(rows, row{
			name: r.name,
			want: r.want,
			frame: func(t *testing.T) []byte {
				built = b(t)
				return frameOf(t, built)
			},
			wantID: func() string { return built.ID },
		})
	}
	rows = append(rows,
		row{"instance_mismatch", ingressbundle.ReasonInstanceMismatch, func(t *testing.T) []byte {
			return frameFor(t, "some-other-instance", testBundle(t, "x.test.local"))
		}, nil},
		row{"too_large", ingressbundle.ReasonTooLarge, func(*testing.T) []byte {
			return make([]byte, ingresswire.MaxBundleMessageBytes+1)
		}, func() string { return "" }},
		row{"malformed", ingressbundle.ReasonMalformed, func(*testing.T) []byte { return []byte("{") },
			func() string { return "" }},
	)

	ran := map[string]bool{}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			frame := r.frame(t)
			id, reason, _ := s.Accept(frame, testInstance)
			if reason != r.want {
				t.Fatalf("Accept refused with %q, want %q", reason, r.want)
			}
			if r.wantID != nil && id != r.wantID() {
				t.Fatalf("Accept named %q in the rejection, want %q", id, r.wantID())
			}
			if after := snapshot(t, root, "certs", "dynamic"); !sameSnapshot(before, after) {
				t.Fatalf("certs/ or dynamic/ changed after a %s rejection", r.want)
			}
			if got := s.AppliedID(); got != applied.ID {
				t.Fatalf("AppliedID = %q after a rejection, want %q", got, applied.ID)
			}
			ran[r.want] = true
		})
	}

	// A code added to RejectReasons without a row here fails this test. apply_failed is the one a frame
	// cannot cause: it is TestApplyFailedKeepsLastGood.
	covered := 0
	for _, code := range ingressbundle.RejectReasons() {
		if code == ingressbundle.ReasonApplyFailed {
			continue
		}
		covered++
		if !ran[code] {
			t.Errorf("no subtest of TestRejectLeavesVolumeUnchanged produces the reason %q", code)
		}
	}
	if covered != 19 {
		t.Errorf("RejectReasons has %d codes besides apply_failed, the table was written for 19", covered)
	}

	got, ok, err := NewStore(fsx.OS(), root).Load()
	if err != nil || !ok || got.ID != applied.ID {
		t.Fatalf("after every rejection Load = (%s, %v, %v), want %s", got.ID, ok, err, applied.ID)
	}
}

func TestApplyFailedKeepsLastGood(t *testing.T) {
	a, b, _ := threeBundles(t)
	root := seeded(t, a)
	dynamicBefore := snapshot(t, root, "dynamic")
	s, rec := recorded(t, root)
	rec.RenameErr("dynamic/tierplan.yml", errors.New("the disk refused"))

	id, reason, _ := s.Accept(frameOf(t, b), testInstance)
	if reason != ingressbundle.ReasonApplyFailed || id != b.ID {
		t.Fatalf("Accept = (%q, %q), want (%q, apply_failed)", id, reason, b.ID)
	}
	if after := snapshot(t, root, "dynamic"); !sameSnapshot(dynamicBefore, after) {
		t.Fatal("dynamic/ changed although the rename failed")
	}
	if s.AppliedID() != a.ID {
		t.Fatalf("AppliedID = %q, want %q", s.AppliedID(), a.ID)
	}
	got, ok, err := NewStore(fsx.OS(), root).Load()
	if err != nil || !ok || got.ID != a.ID {
		t.Fatalf("Load = (%s, %v, %v), want the previous bundle %s", got.ID, ok, err, a.ID)
	}
}

func TestIdenticalBundleIsNoOp(t *testing.T) {
	a, _, _ := threeBundles(t)
	root := seeded(t, a)
	s, rec := recorded(t, root)

	again := a
	again.Version = ingressbundle.Version{Config: 99, Certs: 7} // metadata is not part of the ID
	for _, b := range []ingressbundle.Bundle{a, again} {
		id, reason, _ := s.Accept(frameOf(t, b), testInstance)
		if reason != "" || id != a.ID {
			t.Fatalf("Accept = (%q, %q), want (%q, applied again)", id, reason, a.ID)
		}
	}
	if ops := rec.Ops(); len(ops) != 0 {
		t.Fatalf("an identical bundle caused operations: %v", ops)
	}
}

func TestLowerVersionApplies(t *testing.T) {
	la, lb := newLeafFiles(t, "a.test.local"), newLeafFiles(t, "b.test.local")
	a := bundleOf(t, 5, la)
	lower := bundleOf(t, 2, lb)
	s, root := newPrepared(t)
	mustAccept(t, s, a)
	mustAccept(t, s, lower)
	if s.AppliedID() != lower.ID {
		t.Fatalf("AppliedID = %q, want the lower-version bundle %q", s.AppliedID(), lower.ID)
	}
	raw, err := os.ReadFile(filepath.Join(root, "dynamic", "tierplan.yml")) //nolint:gosec // G304: a file in this test's own temporary volume
	if err != nil || string(raw) != dynamicOf(t, lower) {
		t.Fatalf("dynamic/tierplan.yml is not the lower-version bundle's (%v)", err)
	}
}

// TestRetainPrevious: a reload may still be reading the previous YAML, so the previous bundle's
// certificates stay; one certificate shared by two bundles is never removed.
func TestRetainPrevious(t *testing.T) {
	x, y, z, w := newLeafFiles(t, "x.test.local"), newLeafFiles(t, "y.test.local"),
		newLeafFiles(t, "z.test.local"), newLeafFiles(t, "w.test.local")
	s, root := newPrepared(t)

	steps := []struct {
		name   string
		bundle ingressbundle.Bundle
		want   []string
	}{
		{"A", bundleOf(t, 1, x), pathsOf(x)},
		{"B", bundleOf(t, 2, y), pathsOf(x, y)},
		{"C", bundleOf(t, 3, x, z), pathsOf(x, y, z)},
		{"D", bundleOf(t, 4, w), pathsOf(x, z, w)},
	}
	for _, step := range steps {
		mustAccept(t, s, step.bundle)
		if got := certNames(t, root); !slices.Equal(got, step.want) {
			t.Fatalf("after %s certs/ = %v, want %v", step.name, got, step.want)
		}
	}
	if s.GCErr() != nil {
		t.Fatalf("GCErr = %v", s.GCErr())
	}
}

// TestLoadWithNoVerifiableRecordChangesNothing: a cache that fails under both records serves nothing and
// is left exactly as found, so it can be inspected and nothing is deleted on a guess (D-13).
func TestLoadWithNoVerifiableRecordChangesNothing(t *testing.T) {
	a, b, _ := threeBundles(t)
	s, root := newPrepared(t)
	mustAccept(t, s, a)
	mustAccept(t, s, b)

	tamper := filepath.Join(root, "dynamic", "tierplan.yml")
	raw, err := os.ReadFile(tamper) //nolint:gosec // G304: a file in this test's own temporary volume
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(tamper, append(raw, []byte("# edited by hand\n")...), 0o644); err != nil { //nolint:gosec // a test file
		t.Fatalf("tamper: %v", err)
	}
	before := snapshot(t, root, "certs", "dynamic", "state")

	got, ok, err := NewStore(fsx.OS(), root).Load()
	if ok || err != nil {
		t.Fatalf("Load = (%s, %v, %v), want no bundle and no error", got.ID, ok, err)
	}
	if after := snapshot(t, root, "certs", "dynamic", "state"); !sameSnapshot(before, after) {
		t.Fatal("Load changed the volume although nothing verified")
	}
}

// TestLoadCompletesAVerifiedPending: a kill between the dynamic rename and the commit leaves a pending
// record that verifies; Load adopts it, renames it to applied.json and removes the certificates neither
// the new nor the previous bundle needs.
func TestLoadCompletesAVerifiedPending(t *testing.T) {
	a, b, c := threeBundles(t)
	root := seeded(t, a, b)

	// Find the commit rename in a dry run on a copy of the same shape, then kill the real apply there.
	dry := seeded(t, a, b)
	ds, drec := recorded(t, dry)
	mark := len(drec.Ops())
	mustAccept(t, ds, c)
	commit := slices.Index(drec.Ops()[mark:], opRenameCommit)
	if commit < 0 {
		t.Fatal("the dry run has no commit rename")
	}

	s, rec := recorded(t, root)
	rec.FailFrom(commit)
	if _, reason, _ := s.Accept(frameOf(t, c), testInstance); reason != ingressbundle.ReasonApplyFailed {
		t.Fatalf("Accept reason = %q, want apply_failed from the killed commit", reason)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "pending.json")); err != nil {
		t.Fatalf("pending.json is not there after a kill before the commit: %v", err)
	}

	fresh := NewStore(fsx.OS(), root)
	got, ok, err := fresh.Load()
	if err != nil || !ok || got.ID != c.ID {
		t.Fatalf("Load = (%s, %v, %v), want the pending bundle %s", got.ID, ok, err, c.ID)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "pending.json")); !os.IsNotExist(err) {
		t.Fatalf("pending.json survived a Load that adopted it: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "state", "applied.json")) //nolint:gosec // G304: a file in this test's own temporary volume
	if err != nil || !strings.Contains(string(raw), c.ID) {
		t.Fatalf("applied.json does not name the completed bundle (%v)", err)
	}
	want := slices.Concat(certPaths(b), certPaths(c))
	slices.Sort(want)
	if names := certNames(t, root); !slices.Equal(names, want) {
		t.Fatalf("certs/ = %v after completing, want the new and the previous bundle's %v", names, want)
	}
	if fresh.AppliedID() != c.ID {
		t.Fatalf("AppliedID = %q, want %q", fresh.AppliedID(), c.ID)
	}
}
