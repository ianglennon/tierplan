// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/vault"
)

func TestDNSTokenRoundTripsSealed(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	const token = "cf-token-0123456789abcdef"
	now := time.Unix(1_700_000_000, 42)

	if saved, err := s.DNSTokenSaved(ctx, "cloudflare"); err != nil || saved {
		t.Fatalf("DNSTokenSaved on a fresh database = %v, %v; want false", saved, err)
	}
	if _, found, err := s.DNSToken(ctx, "cloudflare"); found || err != nil {
		t.Fatalf("DNSToken on a fresh database = found %v, %v; want not found", found, err)
	}
	if err := s.SaveDNSToken(ctx, "cloudflare", token, now); err != nil {
		t.Fatalf("SaveDNSToken: %v", err)
	}

	var raw string
	if err := st.Read.QueryRowContext(ctx, `SELECT token_sealed FROM acme_dns_credentials WHERE provider = 'cloudflare'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, token) || !strings.HasPrefix(raw, "v1.") {
		t.Errorf("the token_sealed column is not a vault envelope that hides the token: %.20q", raw)
	}
	got, found, err := s.DNSToken(ctx, "cloudflare")
	if err != nil || !found || got != token {
		t.Fatalf("DNSToken = %q, found %v, %v; want the saved token", got, found, err)
	}
	if saved, err := s.DNSTokenSaved(ctx, "cloudflare"); err != nil || !saved {
		t.Fatalf("DNSTokenSaved = %v, %v; want true", saved, err)
	}

	// Key rotation re-wraps the token, and it opens under the new vault.
	w, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.RewrapAll(ctx, tx, v, w); err != nil {
		t.Fatalf("RewrapAll: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, found, err := NewStore(st, w).DNSToken(ctx, "cloudflare"); err != nil || !found || got != token {
		t.Errorf("after RewrapAll the token under the new vault = %q, found %v, %v; want the saved token", got, found, err)
	}

	// Saving again replaces the token.
	if err := s.SaveDNSToken(ctx, "cloudflare", "another-token", now); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.DNSToken(ctx, "cloudflare"); err != nil || got != "another-token" {
		t.Errorf("after a second save DNSToken = %q, %v; want the new token", got, err)
	}

	if err := s.DeleteDNSToken(ctx, "cloudflare"); err != nil {
		t.Fatal(err)
	}
	if saved, err := s.DNSTokenSaved(ctx, "cloudflare"); err != nil || saved {
		t.Errorf("after DeleteDNSToken DNSTokenSaved = %v, %v; want false", saved, err)
	}
}
