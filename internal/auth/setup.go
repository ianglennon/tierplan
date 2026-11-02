// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package auth holds TierPlan's sign-in machinery. This file is the first-run
// setup gate: while no user exists, claiming the instance needs a one-time
// token that only someone with access to the container log, the data volume or
// `docker exec` can read (D-01).
//
// There is deliberately no option, environment variable, flag or build tag
// that skips, prefills or weakens the gate (D-79).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/ianglennon/tierplan/internal/store"
)

// SetupTokenFile is the name, inside the data directory, of the file holding
// the active setup token for `tierplan setup-token`.
const SetupTokenFile = "setup-token"

const (
	tokenBytes    = 16 // 128 bits
	tokenFileMode = 0o600
)

// SetupGate guards the first-run claim of the instance.
type SetupGate struct {
	st      *store.Store
	dataDir string
	log     *slog.Logger

	needs atomic.Bool

	mu    sync.RWMutex
	token string // normalised: 32 lowercase hex characters, no dashes; empty when none is active
}

// NewSetupGate returns a gate that reads the user count from st and keeps its
// token file in dataDir. Call Init once at startup, after migrations.
func NewSetupGate(st *store.Store, dataDir string, log *slog.Logger) *SetupGate {
	if log == nil {
		log = slog.Default()
	}
	return &SetupGate{st: st, dataDir: dataDir, log: log}
}

func (g *SetupGate) path() string { return filepath.Join(g.dataDir, SetupTokenFile) }

// Init decides whether setup is needed. With no user it generates a fresh token
// (every start gets a new one, D-01), writes it to <dataDir>/setup-token with
// mode 0600 and logs it. With a user it removes any stale token file.
func (g *SetupGate) Init(ctx context.Context) error {
	n, err := g.st.RQ.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		g.finish()
		return g.removeFile()
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generate setup token: %w", err)
	}
	plain := hex.EncodeToString(raw)
	shown := strings.Join([]string{plain[0:8], plain[8:16], plain[16:24], plain[24:32]}, "-")

	if err := writeTokenFile(g.path(), shown); err != nil {
		return err
	}
	g.mu.Lock()
	g.token = plain
	g.mu.Unlock()
	g.needs.Store(true)

	g.log.Warn("setup required",
		"setup_token", shown,
		"hint", "open TierPlan in a browser, or run: docker exec <container> tierplan setup-token")
	return nil
}

// NeedsSetup reports whether the instance is still unclaimed, as of the last
// Init, Check or Consume.
func (g *SetupGate) NeedsSetup() bool { return g.needs.Load() }

// Check re-reads the user count, so a user created after Init (by plan 10's
// completion, or by anything else) ends setup at once. It is what the HTTP
// handlers use; the answer is never stale in the unsafe direction.
func (g *SetupGate) Check(ctx context.Context) (bool, error) {
	if !g.needs.Load() {
		return false, nil
	}
	n, err := g.st.RQ.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	if n > 0 {
		g.finish()
		if err := g.removeFile(); err != nil {
			g.log.Warn("could not remove the setup token file", "error", err)
		}
		return false, nil
	}
	return true, nil
}

// Verify reports whether candidate is the active token. Whitespace and dashes
// are ignored and case is folded, so the token can be typed or pasted either
// way. The comparison is constant-time. With no active token it is always false.
func (g *SetupGate) Verify(candidate string) bool {
	g.mu.RLock()
	token := g.token
	g.mu.RUnlock()
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(normaliseToken(candidate)), []byte(token)) == 1
}

// Consume ends setup: it deletes the token file, forgets the token and flips
// NeedsSetup to false. Plan 10 calls it after the admin is created. It is safe
// to call twice.
func (g *SetupGate) Consume() error {
	g.finish()
	return g.removeFile()
}

// finish clears the in-memory state.
func (g *SetupGate) finish() {
	g.mu.Lock()
	g.token = ""
	g.mu.Unlock()
	g.needs.Store(false)
}

func (g *SetupGate) removeFile() error {
	if err := os.Remove(g.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove setup token file: %w", err)
	}
	return nil
}

// ReadTokenFile returns the token recorded in dataDir, for `tierplan
// setup-token`. A missing file is reported as an error wrapping fs.ErrNotExist.
func ReadTokenFile(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, SetupTokenFile)) //nolint:gosec // operator data directory
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func normaliseToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// writeTokenFile replaces the token file atomically: a 0600 temporary file in
// the same directory is renamed into place, so a reader never sees a partial
// token and the final file is never briefly world-readable.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".setup-token-*")
	if err != nil {
		return fmt.Errorf("write setup token file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op after a successful rename

	if err := tmp.Chmod(tokenFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write setup token file: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write setup token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write setup token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write setup token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install setup token file %s: %w", path, err)
	}
	return nil
}
