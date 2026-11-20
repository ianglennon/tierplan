// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// testInstance is the instance ID every test node is pinned to.
const testInstance = "inst-1"

// frameOf is the wire frame of a bundle for the pinned instance.
func frameOf(t *testing.T, b ingressbundle.Bundle) []byte {
	t.Helper()
	return frameFor(t, testInstance, b)
}

func frameFor(t *testing.T, instance string, b ingressbundle.Bundle) []byte {
	t.Helper()
	raw, err := json.Marshal(ingresswire.BundleEnvelope{T: ingresswire.TypeBundle, InstanceID: instance, Bundle: b})
	if err != nil {
		t.Fatalf("marshal the envelope: %v", err)
	}
	return raw
}

// bundleOf builds a valid bundle over the given certificates, with no routers.
func bundleOf(t *testing.T, config int64, leaves ...leaf) ingressbundle.Bundle {
	t.Helper()
	var files []ingressbundle.File
	for _, l := range leaves {
		files = append(files,
			ingressbundle.File{Path: l.certPath, Data: l.cert},
			ingressbundle.File{Path: l.keyPath, Data: l.key})
	}
	files = append(files, ingressbundle.File{Path: ingressbundle.DynamicFile, Data: dynamicFor(nil, leaves)})
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: config}, files, nil)
	if err != nil {
		t.Fatalf("build a bundle: %v", err)
	}
	return b
}

// newPrepared is a Store over a fresh temporary root on the real filesystem, with the layout made.
func newPrepared(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s := NewStore(fsx.OS(), root)
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return s, root
}

func mustAccept(t *testing.T, s *Store, b ingressbundle.Bundle) {
	t.Helper()
	id, reason := s.Accept(frameOf(t, b), testInstance)
	if reason != "" || id != b.ID {
		t.Fatalf("Accept = (%q, %q), want (%q, \"\")", id, reason, b.ID)
	}
}

func modeOf(t *testing.T, path string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Mode().Perm()
}

func TestPrepareMakesTheLayoutWithPrivateModes(t *testing.T) {
	_, root := newPrepared(t)
	list, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the root: %v", err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
		if !e.IsDir() {
			t.Errorf("%s is not a directory", e.Name())
		}
		if got := modeOf(t, filepath.Join(root, e.Name())); got != 0o700 {
			t.Errorf("%s mode = %v, want 0700", e.Name(), got)
		}
	}
	slices.Sort(names)
	want := []string{"certs", "dynamic", "state", "static", "tmp"}
	if !slices.Equal(names, want) {
		t.Fatalf("layout = %v, want %v", names, want)
	}
}

func TestPrepareClearsTemporaryFilesOfAKilledApply(t *testing.T) {
	s, root := newPrepared(t)
	stray := filepath.Join(root, "tmp", ".part-12345")
	if err := os.WriteFile(stray, []byte("half"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("the leftover temporary file is still there: %v", err)
	}
}

func TestAcceptThenLoad(t *testing.T) {
	s, root := newPrepared(t)
	b := testBundle(t, "a.test.local")
	mustAccept(t, s, b)
	if got := s.AppliedID(); got != b.ID {
		t.Fatalf("AppliedID = %q, want %q", got, b.ID)
	}

	for _, e := range b.Manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(e.Path))
		if got := modeOf(t, path); got != fs.FileMode(e.Mode) {
			t.Errorf("%s mode = %v, want %v", e.Path, got, fs.FileMode(e.Mode))
		}
		if strings.HasSuffix(e.Path, ".key") && modeOf(t, path) != 0o600 {
			t.Errorf("%s is a key and not 0600", e.Path)
		}
	}
	if got := modeOf(t, filepath.Join(root, "state", "applied.json")); got != 0o600 {
		t.Errorf("applied.json mode = %v, want 0600", got)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "pending.json")); !os.IsNotExist(err) {
		t.Errorf("pending.json is still there after the commit: %v", err)
	}
	dyn, err := os.ReadDir(filepath.Join(root, "dynamic"))
	if err != nil || len(dyn) != 1 || dyn[0].Name() != "tierplan.yml" {
		t.Errorf("dynamic/ = %v (%v), want exactly tierplan.yml", dyn, err)
	}

	fresh := NewStore(fsx.OS(), root)
	got, ok, err := fresh.Load()
	if err != nil || !ok {
		t.Fatalf("Load = ok %v, err %v", ok, err)
	}
	if got.ID != b.ID {
		t.Fatalf("Load returned bundle %s, want %s", got.ID, b.ID)
	}
	if err := Check(got); err != nil {
		t.Fatalf("the loaded bundle fails Check: %v", err)
	}
	if fresh.AppliedID() != b.ID {
		t.Fatalf("a loaded Store reports applied %q, want %q", fresh.AppliedID(), b.ID)
	}
}

func TestEmptyBundleAppliesAndLoads(t *testing.T) {
	s, root := newPrepared(t)
	b := bundleOf(t, 1)
	mustAccept(t, s, b)

	got, ok, err := NewStore(fsx.OS(), root).Load()
	if err != nil || !ok || got.ID != b.ID {
		t.Fatalf("Load = (%v, %v, %v), want the empty bundle %s", got.ID, ok, err, b.ID)
	}
	if err := Check(got); err != nil {
		t.Fatalf("Check: %v", err)
	}
	certs, err := os.ReadDir(filepath.Join(root, "certs"))
	if err != nil || len(certs) != 0 {
		t.Fatalf("certs/ = %v (%v), want empty", certs, err)
	}
}

func TestLoadOfAnEmptyVolumeReturnsNothing(t *testing.T) {
	s, _ := newPrepared(t)
	_, ok, err := s.Load()
	if ok || err != nil {
		t.Fatalf("Load on an empty volume = ok %v, err %v, want no bundle", ok, err)
	}
	if s.AppliedID() != "" {
		t.Fatalf("AppliedID = %q on an empty volume", s.AppliedID())
	}
}

func TestWriteStatic(t *testing.T) {
	s, root := newPrepared(t)
	for _, body := range []string{"first\n", "second\n"} {
		if err := s.WriteStatic([]byte(body)); err != nil {
			t.Fatalf("WriteStatic: %v", err)
		}
	}
	path := filepath.Join(root, "static", "traefik.yml")
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "second\n" {
		t.Fatalf("static/traefik.yml = %q (%v), want the second write", got, err)
	}
	if mode := modeOf(t, path); mode != 0o644 {
		t.Fatalf("static/traefik.yml mode = %v, want 0644", mode)
	}
	left, _ := os.ReadDir(filepath.Join(root, "tmp"))
	if len(left) != 0 {
		t.Fatalf("tmp/ holds %d files after WriteStatic", len(left))
	}
}
