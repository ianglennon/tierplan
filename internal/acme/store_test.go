// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// newTestStore is a Store over a migrated temporary database and a vault with a generated key. The
// store and vault are returned too, for tests that read raw columns or re-wrap.
func newTestStore(t *testing.T) (*Store, *store.Store, *vault.Vault) {
	t.Helper()
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.New(t)
	return NewStore(st, v), st, v
}

const testDirectory = "https://acme-staging-v02.api.letsencrypt.org/directory"

func TestACMEAccountKeyRoundTripsSealed(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	acct := Account{
		DirectoryURL: testDirectory, URL: "https://acme.example/acct/1", Contact: "mailto:admin@example.org",
		TermsOfService: "https://acme.example/tos", CreatedAt: time.Unix(1_700_000_000, 123),
	}

	if err := s.SaveAccount(ctx, acct, key); err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	got, gotKey, found, err := s.Account(ctx, testDirectory)
	if err != nil || !found {
		t.Fatalf("Account = found %v, %v; want found", found, err)
	}
	if !gotKey.Equal(key) {
		t.Error("Account returned a different key than was saved")
	}
	if !got.CreatedAt.Equal(acct.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, acct.CreatedAt)
	}
	if got.DirectoryURL != acct.DirectoryURL || got.URL != acct.URL || got.Contact != acct.Contact || got.TermsOfService != acct.TermsOfService {
		t.Errorf("Account = %+v, want %+v", got, acct)
	}

	var raw string
	if err := st.Read.QueryRowContext(ctx, `SELECT key_sealed FROM acme_account WHERE directory_url = ?`, testDirectory).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") || !strings.HasPrefix(raw, "v1.") {
		t.Errorf("the key_sealed column is not a vault envelope: %.20q", raw)
	}

	// Another directory has no account, and that is not an error.
	if _, _, found, err := s.Account(ctx, "https://acme-v02.api.letsencrypt.org/directory"); found || err != nil {
		t.Errorf("an unknown directory = found %v, %v; want not found and no error", found, err)
	}

	// An envelope that does not open is an error and never "not found".
	if _, err := st.Write.ExecContext(ctx, `UPDATE acme_account SET key_sealed = 'v1.k1-nope.AAAA' WHERE directory_url = ?`, testDirectory); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := s.Account(ctx, testDirectory); err == nil || found {
		t.Errorf("a key that does not open = found %v, %v; want an error", found, err)
	}

	if err := s.ForgetAccount(ctx, testDirectory); err != nil {
		t.Fatalf("ForgetAccount: %v", err)
	}
	if _, _, found, err := s.Account(ctx, testDirectory); found || err != nil {
		t.Errorf("after ForgetAccount = found %v, %v; want not found", found, err)
	}
}
