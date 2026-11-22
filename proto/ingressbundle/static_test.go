// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites the golden files from the current output: go test ./ingressbundle -update.
var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// TestStaticGolden pins both forms of the static Traefik file byte for byte (D-10, D-31): the file the
// supervisor writes always matches the binary, and a change to either form is a reviewed diff.
func TestStaticGolden(t *testing.T) {
	for _, tc := range []struct {
		name      string
		accessLog bool
		golden    string
	}{
		{"plain", false, "static-plain.golden"},
		{"access log", true, "static-accesslog.golden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := StaticTraefikWith(tc.accessLog)
			path := filepath.Join("testdata", tc.golden)
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil { //nolint:gosec // a golden file in the source tree
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // a fixed testdata path
			if err != nil {
				t.Fatalf("read golden: %v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("StaticTraefikWith(%t) differs from %s (run with -update to accept):\n--- got\n%s\n--- want\n%s", tc.accessLog, tc.golden, got, want)
			}
		})
	}
}

func TestStaticWithFalseIsStatic(t *testing.T) {
	if !bytes.Equal(StaticTraefikWith(false), StaticTraefik()) {
		t.Fatal("StaticTraefikWith(false) is not StaticTraefik()")
	}
	for _, accessLog := range []bool{false, true} {
		a := StaticTraefikWith(accessLog)
		a[0] = '!'
		if b := StaticTraefikWith(accessLog); b[0] == '!' {
			t.Fatalf("StaticTraefikWith(%t) returned a shared slice: a caller's change reached the next caller", accessLog)
		}
	}
	if b := StaticTraefik(); b[0] == '!' {
		t.Fatal("StaticTraefik returned the embedded slice")
	}
}
