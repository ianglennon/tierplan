// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// update rewrites the golden files from the current output: go test ./internal/ingressrender -update.
// Read the diff before committing it; a golden is the byte-for-byte contract of what a node is given.
var update = flag.Bool("update", false, "rewrite the golden files")

const goldenDir = "testdata/golden"

// goldenScenarios is one fixed Input per golden directory. Later plans add scenarios here.
func goldenScenarios() map[string]Input {
	return map[string]Input{
		// An environment with ingress on and nothing claimed: the dynamic file still holds strict SNI (D-23).
		"empty": {
			Snapshot: ingress.Snapshot{Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 1}},
		},
		// One hostname, the root and one path under it, one certificate: two routers, one shared
		// service, one certificate pair.
		"one-host": {
			Snapshot: ingress.Snapshot{
				Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 4},
				Claims:   []ingress.SnapshotClaim{claimFor("app.example.com")},
				Routes: []ingress.SnapshotRoute{
					swarmRoute("app.example.com", "/", "shop", "web", 8080),
					swarmRoute("app.example.com", "/shop", "shop", "web", 8080),
				},
			},
			Certs:        []HostCert{{Hostname: "app.example.com", ChainPEM: chainA, KeyPEM: keyA}},
			CertsVersion: 2,
		},
	}
}

// goldenBundle is the JSON a bundle.json holds: everything of a Result but file data.
type goldenBundle struct {
	ID       string                 `json:"id"`
	Schema   int                    `json:"schema"`
	Version  ingressbundle.Version  `json:"version"`
	Manifest ingressbundle.Manifest `json:"manifest"`
	Skipped  []goldenSkipped        `json:"skipped"`
}

type goldenSkipped struct {
	Hostname   string `json:"hostname"`
	PathPrefix string `json:"pathPrefix"`
	Stack      string `json:"stack"`
	Service    string `json:"service"`
	Reason     string `json:"reason"`
}

func goldenJSON(t *testing.T, res Result) []byte {
	t.Helper()
	skipped := make([]goldenSkipped, 0, len(res.Skipped))
	for _, s := range res.Skipped {
		skipped = append(skipped, goldenSkipped{s.Hostname, s.PathPrefix, s.Stack, s.Service, string(s.Reason)})
	}
	out, err := json.MarshalIndent(goldenBundle{
		ID: res.Bundle.ID, Schema: res.Bundle.Schema, Version: res.Bundle.Version,
		Manifest: res.Bundle.Manifest, Skipped: skipped,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func dynamicBytes(t *testing.T, res Result) []byte {
	t.Helper()
	for _, f := range res.Bundle.Files {
		if f.Path == ingressbundle.DynamicFile {
			return f.Data
		}
	}
	t.Fatalf("bundle has no %s", ingressbundle.DynamicFile)
	return nil
}

// TestGolden pins the dynamic file and the manifest of every scenario byte for byte (D-07).
func TestGolden(t *testing.T) {
	scenarios := goldenScenarios()

	// A golden directory with no scenario is a stale file that nothing checks.
	dirs, err := os.ReadDir(goldenDir)
	if err != nil && !*update {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, ok := scenarios[d.Name()]; !ok {
			t.Errorf("%s/%s has no scenario in goldenScenarios", goldenDir, d.Name())
		}
	}

	for _, name := range slices.Sorted(maps.Keys(scenarios)) {
		t.Run(name, func(t *testing.T) {
			res, err := Render(scenarios[name])
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			files := map[string][]byte{
				"tierplan.yml": dynamicBytes(t, res),
				"bundle.json":  goldenJSON(t, res),
			}
			dir := filepath.Join(goldenDir, name)
			if *update {
				if err := os.MkdirAll(dir, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range slices.Sorted(maps.Keys(files)) {
				path := filepath.Join(dir, file)
				if *update {
					if err := os.WriteFile(path, files[file], 0o600); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata
				if err != nil {
					t.Fatalf("read golden: %v (run with -update to create it)", err)
				}
				if !bytes.Equal(files[file], want) {
					t.Errorf("%s differs from the golden file.\n--- got ---\n%s\n--- want ---\n%s", path, files[file], want)
				}
			}
		})
	}
}
