// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// goldenDir is the renderer's own golden output. The test reads it across the module boundary; it
// imports nothing from the renderer.
const goldenDir = "../../../internal/ingressrender/testdata/golden"

func TestCheckAcceptsABundle(t *testing.T) {
	b := testBundle(t, "a.test.local")
	if err := Check(b); err != nil {
		t.Fatalf("Check refused a valid bundle: %v", err)
	}
}

func TestCheckAcceptsTheEmptyBundle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(goldenDir, "empty", "tierplan.yml"))
	if err != nil {
		t.Fatalf("read the empty golden: %v", err)
	}
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 1},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: data}}, nil)
	if err != nil {
		t.Fatalf("build the empty bundle: %v", err)
	}
	if err := Check(b); err != nil {
		t.Fatalf("Check refused the empty bundle, which is valid and fails closed at the handshake: %v", err)
	}
}

// TestCertRefsMatchRendererGoldens proves the YAML reference scan against what the renderer writes: for
// every golden, the certificate paths certRefs finds are exactly the certs/ files the golden's manifest
// lists.
func TestCertRefsMatchRendererGoldens(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(goldenDir, "*"))
	if err != nil {
		t.Fatalf("glob the goldens: %v", err)
	}
	var seen int
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		seen++
		t.Run(filepath.Base(dir), func(t *testing.T) {
			dynamic, err := os.ReadFile(filepath.Join(dir, "tierplan.yml"))
			if err != nil {
				t.Fatalf("read tierplan.yml: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			if err != nil {
				t.Fatalf("read bundle.json: %v", err)
			}
			var golden struct {
				Manifest struct {
					Files []struct {
						Path string `json:"path"`
					} `json:"files"`
				} `json:"manifest"`
			}
			if err := json.Unmarshal(raw, &golden); err != nil {
				t.Fatalf("parse bundle.json: %v", err)
			}
			want := []string{}
			for _, f := range golden.Manifest.Files {
				if strings.HasPrefix(f.Path, ingressbundle.CertsDir+"/") {
					want = append(want, f.Path)
				}
			}
			slices.Sort(want)

			refs, err := certRefs(dynamic)
			if err != nil {
				t.Fatalf("certRefs: %v", err)
			}
			got := []string{}
			for _, r := range refs {
				rel, ok := relativeCertPath(r)
				if !ok {
					t.Fatalf("a reference the renderer wrote is not under the cert root: %q", r)
				}
				got = append(got, rel)
			}
			slices.Sort(got)
			got = slices.Compact(got)
			if !slices.Equal(got, want) {
				t.Fatalf("certRefs = %v, the manifest lists %v", got, want)
			}
		})
	}
	if seen < 8 {
		t.Fatalf("found %d golden directories under %s, want at least the eight Phase 21 goldens", seen, goldenDir)
	}
}
