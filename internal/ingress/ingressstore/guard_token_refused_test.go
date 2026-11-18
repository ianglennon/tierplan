// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/store"
)

// seedFailedLedgerRow puts a ledger row of domainID's token whose last delete failed with class, with attempts made
// and a next try far ahead.
func seedFailedLedgerRow(t *testing.T, st *store.Store, id, domainID, class string) {
	t.Helper()
	if _, err := st.Write.Exec(
		`INSERT INTO acme_dns_records (id, provider, zone_id, record_id, name, value, owner_kind, owner_id, created_at, credential_ref,
		                               attempts, last_try_at, next_try_at, last_error_class)
		 VALUES (?, 'cloudflare', 'zone-1', ?, '_acme-challenge.app.example.com', 'v', 'order', 'order-1', 1, ?, 4, 10, 9000000000000000000, ?)`,
		id, "rec-"+id, domainID, class); err != nil {
		t.Fatalf("seed ledger row %s: %v", id, err)
	}
}

// Phase 20 review WR-03: a record the DNS provider refuses the domain's token for cannot be removed with that token,
// so it does not hold the token's replacement back. After the replacement the record is due again with its backoff
// started over, so the next sweep tries it with the new token. A record the token may still remove holds the
// replacement back as before, and the delete stays refused while any record is left, saying how many the provider
// refuses the token for.
func TestDomainTokenReplaceIsNotHeldBackByRecordsTheTokenCannotRemove(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	d, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "example.com", Issuer: "staging", Token: []byte("old-token"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	seedFailedLedgerRow(t, st, "invalid", d.ID, "dns_token_invalid")
	seedFailedLedgerRow(t, st, "scope", d.ID, "dns_token_scope")
	seedFailedLedgerRow(t, st, "busy", d.ID, "dns_rate_limited")

	var pending *CleanupPendingError
	err = s.DeleteDomain(ctx, "env-1", d.ID, nil)
	if !errors.As(err, &pending) || pending.Records != 3 || pending.TokenRefused != 2 {
		t.Fatalf("DeleteDomain = %v (%+v), want *CleanupPendingError with 3 records, 2 refused for the token", err, pending)
	}

	// A record the token may still remove (a rate limit passes) holds the replacement back.
	_, err = s.SetDomainToken(ctx, "env-1", d.ID, []byte("new-token"), "admin", nil)
	if !errors.As(err, &pending) || pending.Records != 1 || pending.TokenRefused != 0 {
		t.Fatalf("SetDomainToken = %v (%+v), want *CleanupPendingError with the 1 record the token may still remove", err, pending)
	}
	if got, err := s.DomainToken(ctx, "env-1", d.ID); err != nil || string(got) != "old-token" {
		t.Fatalf("DomainToken = %q, %v, want the old token kept", got, err)
	}

	// Once only records the token cannot remove are left, the replacement goes through and makes them due.
	if _, err := st.Write.Exec(`DELETE FROM acme_dns_records WHERE id = 'busy'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDomainToken(ctx, "env-1", d.ID, []byte("new-token"), "admin", nil); err != nil {
		t.Fatalf("SetDomainToken with only token-refused records = %v, want success", err)
	}
	if got, err := s.DomainToken(ctx, "env-1", d.ID); err != nil || string(got) != "new-token" {
		t.Fatalf("DomainToken = %q, %v, want the new token", got, err)
	}
	var stale int
	if err := st.Read.QueryRow(`SELECT COUNT(*) FROM acme_dns_records WHERE credential_ref = ? AND (attempts <> 0 OR next_try_at <> 0)`, d.ID).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("%d records still wait on their old backoff after the replacement, want none", stale)
	}

	// The delete still waits for the records to go.
	if err := s.DeleteDomain(ctx, "env-1", d.ID, nil); !errors.As(err, &pending) || pending.Records != 2 {
		t.Fatalf("DeleteDomain after the replacement = %v, want *CleanupPendingError with 2 records", err)
	}
}

// The reset after a replacement touches only the replaced domain's records: another domain's backoff and TierPlan's
// own records (an empty credential_ref) are left as they were.
func TestDomainTokenReplaceRetriesOnlyItsOwnRecords(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	seedEnv(t, st, "env-1", "one")
	a, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "a.example", Issuer: "staging", Token: []byte("tok-a"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain a: %v", err)
	}
	b, err := s.CreateDomain(ctx, CreateDomainInput{EnvID: "env-1", Suffix: "b.example", Issuer: "staging", Token: []byte("tok-b"), Actor: "admin"}, nil)
	if err != nil {
		t.Fatalf("CreateDomain b: %v", err)
	}
	seedFailedLedgerRow(t, st, "a-row", a.ID, "dns_token_invalid")
	seedFailedLedgerRow(t, st, "b-row", b.ID, "dns_token_invalid")
	seedFailedLedgerRow(t, st, "own-row", "", "dns_token_invalid")

	if _, err := s.SetDomainToken(ctx, "env-1", a.ID, []byte("tok-a2"), "admin", nil); err != nil {
		t.Fatalf("SetDomainToken(a): %v", err)
	}
	for id, want := range map[string]int64{"a-row": 0, "b-row": 4, "own-row": 4} {
		var attempts int64
		if err := st.Read.QueryRow(`SELECT attempts FROM acme_dns_records WHERE id = ?`, id).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != want {
			t.Errorf("%s attempts = %d, want %d", id, attempts, want)
		}
	}
}
