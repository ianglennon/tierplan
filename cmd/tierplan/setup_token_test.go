// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSetupTokenPrintsTheActiveToken(t *testing.T) {
	dir := t.TempDir()
	const token = "0123abcd-4567ef01-89abcdef-01234567"
	if err := os.WriteFile(filepath.Join(dir, "setup-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, dir, "setup-token")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != token || !regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{8}){3}$`).MatchString(got) {
		t.Fatalf("output = %q, want %q", out, token)
	}
}

func TestSetupTokenExitsOneWithAnExplanationWhenThereIsNone(t *testing.T) {
	dir := t.TempDir()
	out, errOut, err := runCLI(t, dir, "setup-token")
	if err == nil {
		t.Fatal("want a non-zero exit when there is no token")
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("stdout = %q, want empty", out)
	}
	if !strings.Contains(errOut, "no active setup token: setup is already complete, or the TierPlan server has not started yet") {
		t.Fatalf("stderr = %q, want the explanation", errOut)
	}
}

func TestSetupTokenNeverOpensTheDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runCLI(t, dir, "setup-token"); err == nil {
		t.Fatal("want an error with no token")
	}
	if _, err := os.Stat(filepath.Join(dir, "tierplan.db")); err == nil {
		t.Fatal("setup-token created or opened the database")
	}
}
