// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package compose_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPrivilegedServiceYieldsOneFindingWithItsLine(t *testing.T) {
	fs, err := compose.Check(fixture(t, "privileged.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Service != "web" || f.Key != "privileged" || f.Value != "true" || f.Line != 4 || f.Rule != "privileged" {
		t.Fatalf("unexpected finding %+v", f)
	}
	if f.Why == "" {
		t.Fatal("a finding must say why")
	}
	if !compose.NeedsEnvAdmin(fs) {
		t.Fatal("a finding must require env Admin")
	}
	if got, want := compose.Format(f), "services.web.privileged (line 4): true"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestCleanComposeFileHasNoFindings(t *testing.T) {
	fs, err := compose.Check(fixture(t, "clean.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("want no findings, got %+v", fs)
	}
	if compose.NeedsEnvAdmin(fs) {
		t.Fatal("no findings must not require env Admin")
	}
}

func TestEmptyInputHasNoFindingsAndNoError(t *testing.T) {
	for _, src := range []string{"", "   \n", "# only a comment\n", "services: {}", "services:\n", "name: x\n"} {
		fs, err := compose.Check([]byte(src))
		if err != nil || len(fs) != 0 {
			t.Fatalf("%q: findings %+v err %v", src, fs, err)
		}
	}
}

func TestTooLargeInputIsRefusedAndMalformedYAMLIsAnError(t *testing.T) {
	big := bytes.Repeat([]byte("#"), compose.MaxSize+1)
	if _, err := compose.Check(big); !errors.Is(err, compose.ErrTooLarge) {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
	edge := bytes.Repeat([]byte("#"), compose.MaxSize)
	if _, err := compose.Check(edge); err != nil {
		t.Fatalf("exactly 1 MiB must be accepted, got %v", err)
	}
	if _, err := compose.Check([]byte("services: [")); err == nil {
		t.Fatal("malformed YAML must be an error")
	}
}
