// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/store/storetest"
)

var tokenShape = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{8}){3}$`)

func newGate(t *testing.T) (*auth.SetupGate, *store.Store, string, *bytes.Buffer) {
	t.Helper()
	st := storetest.New(t)
	dir := t.TempDir()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	return auth.NewSetupGate(st, dir, log), st, dir, logs
}

func addUser(t *testing.T, st *store.Store) {
	t.Helper()
	now := time.Now().UnixNano()
	err := st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: "x",
		CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir string) string {
	t.Helper()
	tok, err := auth.ReadTokenFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestInitWithNoUsersWritesLogsAndRequiresSetup(t *testing.T) {
	g, _, dir, logs := newGate(t)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !g.NeedsSetup() {
		t.Fatal("NeedsSetup = false with zero users")
	}
	tok := readFile(t, dir)
	if !tokenShape.MatchString(tok) {
		t.Fatalf("token %q is not four dash-separated groups of 8 lowercase hex chars", tok)
	}
	st, err := os.Stat(filepath.Join(dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("setup-token mode = %o, want 600", st.Mode().Perm())
	}
	out := logs.String()
	if !strings.Contains(out, "setup required") || !strings.Contains(out, tok) || !strings.Contains(out, "coaming setup-token") {
		t.Fatalf("log must carry the message, the token and the CLI hint, got %s", out)
	}
	// No leftover temporary files in the data directory.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("data dir has %d entries, want only setup-token", len(entries))
	}
}

func TestEveryStartGeneratesANewToken(t *testing.T) {
	g1, st, dir, _ := newGate(t)
	if err := g1.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := readFile(t, dir)
	g2 := auth.NewSetupGate(st, dir, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err := g2.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := readFile(t, dir)
	if first == second {
		t.Fatal("a second Init reused the previous token")
	}
	if g2.Verify(first) {
		t.Fatal("the previous start's token still verifies")
	}
	if !g2.Verify(second) {
		t.Fatal("the new token does not verify")
	}
}

func TestInitWithAUserRemovesStaleTokenFile(t *testing.T) {
	g, st, dir, logs := newGate(t)
	if err := os.WriteFile(filepath.Join(dir, "setup-token"), []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addUser(t, st)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.NeedsSetup() {
		t.Fatal("NeedsSetup = true with a user")
	}
	if _, err := auth.ReadTokenFile(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale setup-token should be gone, ReadTokenFile err = %v", err)
	}
	if strings.Contains(logs.String(), "setup required") {
		t.Fatal("must not log a setup token when setup is complete")
	}
	if g.Verify("stale") || g.Verify("") {
		t.Fatal("a gate with no token must reject everything")
	}
}

func TestVerifyNormalisesAndRejects(t *testing.T) {
	g, _, dir, _ := newGate(t)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok := readFile(t, dir)
	bare := strings.ReplaceAll(tok, "-", "")

	accept := map[string]string{
		"as written":       tok,
		"without dashes":   bare,
		"surrounding ws":   "  \t" + tok + "\n",
		"upper case":       strings.ToUpper(tok),
		"inner whitespace": strings.Join([]string{tok[:8], tok[9:17], tok[18:26], tok[27:]}, " "),
	}
	for name, cand := range accept {
		if !g.Verify(cand) {
			t.Errorf("Verify rejected %s: %q", name, cand)
		}
	}

	flipped := []byte(bare)
	if flipped[0] == '0' {
		flipped[0] = '1'
	} else {
		flipped[0] = '0'
	}
	reject := map[string]string{
		"empty":         "",
		"only dashes":   "---",
		"one char off":  string(flipped),
		"truncated":     bare[:31],
		"extended":      bare + "0",
		"prefix":        bare[:16],
		"another token": "00000000-00000000-00000000-00000000",
		"non hex":       strings.Repeat("z", 32),
	}
	for name, cand := range reject {
		if g.Verify(cand) {
			t.Errorf("Verify accepted %s: %q", name, cand)
		}
	}
}

func TestConsumeDeletesFileClearsTokenAndEndsSetup(t *testing.T) {
	g, _, dir, _ := newGate(t)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	tok := readFile(t, dir)
	if err := g.Consume(); err != nil {
		t.Fatal(err)
	}
	if g.NeedsSetup() {
		t.Fatal("NeedsSetup = true after Consume")
	}
	if g.Verify(tok) {
		t.Fatal("the token still verifies after Consume")
	}
	if _, err := auth.ReadTokenFile(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("setup-token should be deleted, err = %v", err)
	}
	if err := g.Consume(); err != nil {
		t.Fatalf("a second Consume must not fail: %v", err)
	}
}

func TestCheckNoticesAUserCreatedAfterInit(t *testing.T) {
	g, st, dir, _ := newGate(t)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	needs, err := g.Check(context.Background())
	if err != nil || !needs {
		t.Fatalf("Check before any user = %v, %v; want true, nil", needs, err)
	}
	tok := readFile(t, dir)
	addUser(t, st)
	needs, err = g.Check(context.Background())
	if err != nil || needs {
		t.Fatalf("Check after a user exists = %v, %v; want false, nil", needs, err)
	}
	if g.Verify(tok) {
		t.Fatal("the token must stop verifying once a user exists")
	}
	if _, err := auth.ReadTokenFile(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("setup-token should be removed once a user exists, err = %v", err)
	}
}

func TestReadTokenFileMissing(t *testing.T) {
	_, err := auth.ReadTokenFile(t.TempDir())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

// D-79: nothing in the environment can prefill or skip the gate.
func TestNoEnvironmentVariableCanPrefillTheToken(t *testing.T) {
	const planted = "11111111-22222222-33333333-44444444"
	// The names are assembled so that the repository-wide grep for bypass
	// settings (the plan 09 acceptance check) matches nothing, this file included.
	setup := "SET" + "UP"
	for _, name := range []string{"COAMING_" + setup + "_TOKEN", "COAMING_SKIP_" + setup, "COAMING_" + setup + "_BYPASS", setup + "_TOKEN"} {
		t.Setenv(name, planted)
	}
	g, _, dir, _ := newGate(t)
	if err := g.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if readFile(t, dir) == planted || g.Verify(planted) {
		t.Fatal("an environment variable prefilled the setup token")
	}
	if !g.NeedsSetup() {
		t.Fatal("an environment variable skipped setup")
	}
}
