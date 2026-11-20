// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
)

// errFlush is the error the injected directory flush returns.
var errFlush = errors.New("input/output error")

// syncFS is the real filesystem that counts the flushes of each directory (by base name) and fails the
// flush of the directories named in failing.
type syncFS struct {
	fsx.FS
	mu      sync.Mutex
	failing map[string]bool
	synced  map[string]int
}

func newSyncFS(failing ...string) *syncFS {
	f := &syncFS{FS: fsx.OS(), failing: map[string]bool{}, synced: map[string]int{}}
	for _, d := range failing {
		f.failing[d] = true
	}
	return f
}

func (f *syncFS) fail(dir string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[dir] = on
}

func (f *syncFS) flushes(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced[dir]
}

func (f *syncFS) SyncDir(dir string) error {
	f.mu.Lock()
	name := filepath.Base(dir)
	f.synced[name]++
	fail := f.failing[name]
	f.mu.Unlock()
	if fail {
		return errFlush
	}
	return f.FS.SyncDir(dir)
}

// TestFailedDynamicFlushStillRecordsTheBundleInForce is WR-A-04: when the flush of dynamic/ fails after the
// rename, the new bundle is already what Traefik reads, so the store's state is the new bundle (and the
// error still says the apply was not clean). Before the fix the state stayed on the previous bundle while
// the volume served the new one.
func TestFailedDynamicFlushStillRecordsTheBundleInForce(t *testing.T) {
	a, b, _ := threeBundles(t)
	root := seeded(t, a)
	fsys := newSyncFS("dynamic")
	s := NewStore(fsys, root)
	if _, ok, err := s.Load(); err != nil || !ok {
		t.Fatalf("Load: ok %v, err %v", ok, err)
	}

	err := s.Apply(b)
	var ae *ApplyError
	if !errors.As(err, &ae) || ae.Step != stepDynamic || !errors.Is(err, errFlush) {
		t.Fatalf("Apply = %v, want an *ApplyError at step dynamic wrapping the flush error", err)
	}
	raw, rerr := os.ReadFile(filepath.Join(root, "dynamic", "tierplan.yml")) //nolint:gosec // G304: a file in this test's own temporary volume
	if rerr != nil || string(raw) != dynamicOf(t, b) {
		t.Fatalf("dynamic/tierplan.yml is not the new bundle's (%v): the rename should have happened", rerr)
	}
	if got := s.AppliedID(); got != b.ID {
		t.Fatalf("AppliedID = %q after the rename, want the new bundle %q the volume serves", got, b.ID)
	}
	// The same bundle again is answered from memory, so the node and the volume agree.
	if id, reason, _ := s.Accept(frameOf(t, b), testInstance); id != b.ID || reason != "" {
		t.Fatalf("Accept of the bundle in force = (%q, %q), want (%q, \"\")", id, reason, b.ID)
	}
}

// TestCertsDirIsFlushedWhenTheFilesAreAlreadyThere is WR-A-06: a certificate that is already on disk with the
// same content is not written again, but certs/ is still flushed, because "already on disk" is what the page
// cache says and not what a power loss keeps. Before the fix the retry of an apply whose first flush failed
// skipped the flush.
func TestCertsDirIsFlushedWhenTheFilesAreAlreadyThere(t *testing.T) {
	a, _, _ := threeBundles(t)
	root := t.TempDir()
	fsys := newSyncFS("certs")
	s := NewStore(fsys, root)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// First attempt: the certificates are written, the flush of certs/ fails, the apply fails.
	var ae *ApplyError
	if err := s.Apply(a); !errors.As(err, &ae) || ae.Step != stepCerts || !errors.Is(err, errFlush) {
		t.Fatalf("first Apply = %v, want an *ApplyError at step certs wrapping the flush error", err)
	}
	failed := fsys.flushes("certs")

	// The resend: every file is present and equal, so nothing is written, and certs/ must be flushed anyway.
	fsys.fail("certs", false)
	if err := s.Apply(a); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := fsys.flushes("certs"); got <= failed {
		t.Fatalf("certs/ was flushed %d times in total, %d by the failed attempt: the retry skipped the flush", got, failed)
	}
}
