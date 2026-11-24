// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"testing"
)

// update rewrites the golden file from the current output: go test ./internal/vip -update. Read the diff
// before committing it; the golden is the byte-for-byte configuration every node runs.
var update = flag.Bool("update", false, "rewrite the golden files")

const goldenFile = "testdata/keepalived.conf.golden"

// goldenConfig is the fixed input of the golden: 192.168.1.50/24, router 77, probe port 7483.
func goldenConfig() Config {
	return Config{
		Address:   netip.MustParsePrefix("192.168.1.50/24"),
		RouterID:  77,
		ProbePort: 7483,
		KeyFile:   "/etc/tierplan-keepalived/key",
		NodeID:    "abcdefghijklmnopqrstuvwxy",
		NodeName:  "node-a",
	}
}

func TestRenderGolden(t *testing.T) {
	got := Render(goldenConfig(), "eth0")
	if *update {
		if err := os.WriteFile(goldenFile, got, 0o644); err != nil { //nolint:gosec // a test golden, not a secret
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("read the golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Render differs from %s (run with -update, read the diff):\n--- got ---\n%s--- want ---\n%s", goldenFile, got, want)
	}
}
