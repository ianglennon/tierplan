// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"reflect"
	"strings"
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

// TestRenderNeverHoldsTheKey pins A-03 and PROH-26-03: the key appears in the text as one file reference and
// Config has no field that could carry key bytes.
func TestRenderNeverHoldsTheKey(t *testing.T) {
	cfg := goldenConfig()
	out := string(Render(cfg, "eth0"))

	var keyLines []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && f[0] == "key" {
			keyLines = append(keyLines, line)
		}
	}
	if len(keyLines) != 1 {
		t.Fatalf("want exactly one key line, got %d: %q", len(keyLines), keyLines)
	}
	f := strings.Fields(keyLines[0])
	if len(f) != 3 || f[2] != "file:"+cfg.KeyFile {
		t.Errorf("key line %q is not a file reference to %s", keyLines[0], cfg.KeyFile)
	}
	if strings.Contains(out, "hex:") {
		t.Error("the text holds an inline hex key")
	}

	typ := reflect.TypeOf(cfg)
	for i := 0; i < typ.NumField(); i++ {
		switch fld := typ.Field(i); fld.Type.Kind() {
		case reflect.String, reflect.Int, reflect.Struct:
		default:
			t.Errorf("Config.%s is a %s: only strings, ints and the address prefix are allowed, never bytes", fld.Name, fld.Type)
		}
	}
}
