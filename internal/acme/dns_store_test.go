// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store"
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

func ledgerRow(id, ownerKind, ownerID, token string, created time.Time) DNSRecord {
	return DNSRecord{
		ID: id, Provider: "cloudflare", ZoneID: "zone-1", ZoneName: "example.org", AccountID: "account-1", RecordID: "rec-" + id,
		Name: "_acme-challenge.tierplan.example.org", Value: "value-" + id,
		Comment:   "tierplan:install-1:" + ownerID,
		OwnerKind: ownerKind, OwnerID: ownerID, Token: token, CreatedAt: created,
	}
}

func ledgerIDs(rows []DNSRecord) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func TestDNSLedgerInsertIsDurable(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 987_654_321)
	want := ledgerRow("row-1", OwnerOrder, "order-1", "tok-1", t0)
	if err := s.InsertDNSRecord(ctx, want); err != nil {
		t.Fatalf("InsertDNSRecord: %v", err)
	}

	// Durable when InsertDNSRecord returns: nothing is left in the write-ahead log to lose.
	var busy, logFrames, checkpointed int
	if err := st.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if busy != 0 || logFrames != checkpointed {
		t.Errorf("after InsertDNSRecord the log holds %d frames and %d are checkpointed (busy %d); want all checkpointed", logFrames, checkpointed, busy)
	}

	// A fresh Store over the same database reads the row back exactly.
	rows, err := NewStore(st, v).DNSRecords(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("DNSRecords = %d rows, %v; want 1", len(rows), err)
	}
	got := rows[0]
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	got.CreatedAt, want.CreatedAt = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("row = %+v, want %+v", got, want)
	}
}

func TestDNSLedgerRecordsForAnOwner(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	for _, r := range []DNSRecord{
		ledgerRow("a", OwnerOrder, "order-1", "tok-1", t0),
		ledgerRow("b", OwnerOrder, "order-1", "tok-2", t0.Add(time.Second)),
		ledgerRow("c", OwnerOrder, "order-2", "tok-1", t0.Add(2*time.Second)),
		ledgerRow("d", OwnerTest, "order-1", "tok-1", t0.Add(3*time.Second)),
		ledgerRow("e", OwnerOrder, "order-1", "tok-1", t0.Add(4*time.Second)),
	} {
		if err := s.InsertDNSRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.DNSRecordsFor(ctx, OwnerOrder, "order-1", "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgerIDs(rows); !slices.Equal(got, []string{"a", "e"}) {
		t.Errorf("DNSRecordsFor(order, order-1, tok-1) = %v, want [a e]", got)
	}
	rows, err = s.DNSRecordsFor(ctx, OwnerTest, "nobody", "tok-1")
	if err != nil || len(rows) != 0 {
		t.Errorf("DNSRecordsFor an owner with no rows = %v, %v; want none", ledgerIDs(rows), err)
	}
}

func TestDNSLedgerPendingRecordIsReusedByRestamping(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	r := ledgerRow("a", OwnerOrder, "order-1", "tok-1", t0)
	if err := s.InsertDNSRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDNSRecordFailed(ctx, "a", 2, t0.Add(time.Minute), t0.Add(time.Hour), "dns_unavailable"); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.PendingDNSRecord(ctx, r.Provider, r.ZoneID, r.Name, r.Value)
	if err != nil || !found || got.ID != "a" {
		t.Fatalf("PendingDNSRecord = %q, found %v, %v; want row a", got.ID, found, err)
	}
	if _, found, err := s.PendingDNSRecord(ctx, r.Provider, r.ZoneID, r.Name, "another value"); found || err != nil {
		t.Errorf("PendingDNSRecord for a different value = found %v, %v; want not found", found, err)
	}

	if err := s.RestampDNSRecord(ctx, "a", OwnerOrder, "order-2", "tok-9"); err != nil {
		t.Fatalf("RestampDNSRecord: %v", err)
	}
	rows, err := s.DNSRecordsFor(ctx, OwnerOrder, "order-2", "tok-9")
	if err != nil || len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("after RestampDNSRecord the new owner holds %v, %v; want [a]", ledgerIDs(rows), err)
	}
	if rows[0].Attempts != 0 || !rows[0].NextTryAt.IsZero() || rows[0].LastErrorClass != "" {
		t.Errorf("a re-stamped row keeps its retry state: %+v", rows[0])
	}
	if old, _ := s.DNSRecordsFor(ctx, OwnerOrder, "order-1", "tok-1"); len(old) != 0 {
		t.Errorf("the old owner still holds %v", ledgerIDs(old))
	}
}

func TestDNSLedgerFailedDeleteIsStampedAndDueLater(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	for i, id := range []string{"a", "b", "c"} {
		if err := s.InsertDNSRecord(ctx, ledgerRow(id, OwnerOrder, "order-1", "tok-"+id, t0.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	// Every fresh row is due at once (next_try_at is 0).
	due, err := s.DueDNSRecords(ctx, t0)
	if err != nil || !slices.Equal(ledgerIDs(due), []string{"a", "b", "c"}) {
		t.Fatalf("DueDNSRecords for fresh rows = %v, %v; want [a b c]", ledgerIDs(due), err)
	}

	retryAt := t0.Add(time.Hour)
	if err := s.MarkDNSRecordFailed(ctx, "a", 3, t0.Add(time.Minute), retryAt, "dns_token_invalid"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDNSRecordFailed(ctx, "c", 1, t0.Add(time.Minute), t0.Add(2*time.Hour), "dns_unavailable"); err != nil {
		t.Fatal(err)
	}

	due, err = s.DueDNSRecords(ctx, t0.Add(time.Minute))
	if err != nil || !slices.Equal(ledgerIDs(due), []string{"b"}) {
		t.Errorf("DueDNSRecords before any retry is due = %v, %v; want [b]", ledgerIDs(due), err)
	}
	due, err = s.DueDNSRecords(ctx, retryAt)
	if err != nil || !slices.Equal(ledgerIDs(due), []string{"a", "b"}) {
		t.Errorf("DueDNSRecords at a's retry time = %v, %v; want [a b]", ledgerIDs(due), err)
	}

	all, err := s.DNSRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := all[0]
	if a.ID != "a" || a.Attempts != 3 || a.LastErrorClass != "dns_token_invalid" ||
		!a.LastTryAt.Equal(t0.Add(time.Minute)) || !a.NextTryAt.Equal(retryAt) {
		t.Errorf("row a = %+v, want attempts 3, class dns_token_invalid, last try +1m, next try +1h", a)
	}
}

func TestDNSLedgerOrdersOldestFirst(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	// Inserted out of order; two share an instant and are ordered by id.
	for _, r := range []DNSRecord{
		ledgerRow("z", OwnerOrder, "o", "t", t0.Add(time.Second)),
		ledgerRow("c", OwnerOrder, "o", "t", t0),
		ledgerRow("a", OwnerOrder, "o", "t", t0),
		ledgerRow("m", OwnerTest, "x", "t", t0.Add(2*time.Second)),
	} {
		if err := s.InsertDNSRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"a", "c", "z", "m"}
	all, err := s.DNSRecords(ctx)
	if err != nil || !slices.Equal(ledgerIDs(all), want) {
		t.Errorf("DNSRecords = %v, %v; want %v", ledgerIDs(all), err, want)
	}
	due, err := s.DueDNSRecords(ctx, t0)
	if err != nil || !slices.Equal(ledgerIDs(due), want) {
		t.Errorf("DueDNSRecords = %v, %v; want %v", ledgerIDs(due), err, want)
	}
}

func TestDNSLedgerDeleteRemovesExactlyOneRow(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	// The same record content in two rows: a delete is by TierPlan's row id, never by name or value.
	a, b := ledgerRow("a", OwnerOrder, "o", "t", t0), ledgerRow("b", OwnerOrder, "o", "t", t0.Add(time.Second))
	b.Name, b.Value, b.RecordID = a.Name, a.Value, a.RecordID
	for _, r := range []DNSRecord{a, b} {
		if err := s.InsertDNSRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteDNSRecord(ctx, "a"); err != nil {
		t.Fatalf("DeleteDNSRecord: %v", err)
	}
	all, err := s.DNSRecords(ctx)
	if err != nil || !slices.Equal(ledgerIDs(all), []string{"b"}) {
		t.Errorf("after deleting a, DNSRecords = %v, %v; want [b]", ledgerIDs(all), err)
	}
	// Deleting a row that is not there is not an error.
	if err := s.DeleteDNSRecord(ctx, "a"); err != nil {
		t.Errorf("DeleteDNSRecord of a missing row = %v, want nil", err)
	}
}

func strPtr(s string) *string { return &s }

func TestStateFullUpdateNeverWritesTheChallenge(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	now := time.Unix(1_700_000_000, 0)

	fresh, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ChallengeType != ChallengeHTTP01 || fresh.DNSProvider != "cloudflare" || fresh.ValidationUntested ||
		fresh.DNSResolvers == nil || len(fresh.DNSResolvers) != 0 {
		t.Fatalf("fresh state = challenge %q, provider %q, untested %v, resolvers %#v; want http-01, cloudflare, false, an empty list",
			fresh.ChallengeType, fresh.DNSProvider, fresh.ValidationUntested, fresh.DNSResolvers)
	}

	stale, err := s.State(ctx) // read before the validation save
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveValidation(ctx, ValidationWrite{
		ChallengeType: ChallengeDNS01, Provider: "cloudflare", Resolvers: []string{"192.0.2.53:5353", "ns.example.org"},
		Token: strPtr("cf-token"), Untested: true, Now: now,
	}); err != nil {
		t.Fatalf("SaveValidation: %v", err)
	}
	stale.ConsecutiveFailures = 2
	if err := s.SaveState(ctx, stale); err != nil { // the full update with the stale read
		t.Fatal(err)
	}

	got, err := s.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChallengeType != ChallengeDNS01 || got.DNSProvider != "cloudflare" || !got.ValidationUntested ||
		!slices.Equal(got.DNSResolvers, []string{"192.0.2.53:5353", "ns.example.org"}) {
		t.Errorf("after a stale SaveState: challenge %q, provider %q, untested %v, resolvers %v; want the saved dns-01 settings",
			got.ChallengeType, got.DNSProvider, got.ValidationUntested, got.DNSResolvers)
	}
	if got.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2: SaveState still writes the ledger", got.ConsecutiveFailures)
	}
	if token, found, err := s.DNSToken(ctx, "cloudflare"); err != nil || !found || token != "cf-token" {
		t.Errorf("DNSToken = %q, found %v, %v; want the token saved with the validation", token, found, err)
	}

	// A save without a token keeps the saved one; SetValidationUntested clears just the flag.
	if err := s.SaveValidation(ctx, ValidationWrite{ChallengeType: ChallengeHTTP01, Provider: "cloudflare", Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetValidationUntested(ctx, false, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.State(ctx)
	if err != nil || got.ChallengeType != ChallengeHTTP01 || got.ValidationUntested || len(got.DNSResolvers) != 0 {
		t.Errorf("after switching back: %+v, %v; want http-01, not untested, no resolvers", got, err)
	}
	if saved, err := s.DNSTokenSaved(ctx, "cloudflare"); err != nil || !saved {
		t.Errorf("DNSTokenSaved after switching back = %v, %v; want the token kept", saved, err)
	}
}

func TestSaveValidationIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	write := ValidationWrite{
		ChallengeType: ChallengeDNS01, Provider: "cloudflare", Resolvers: []string{"192.0.2.53"},
		Token: strPtr("cf-token"), Untested: true, CloseOpenOrder: true, Now: now,
	}
	assertUntouched := func(t *testing.T, s *Store) {
		t.Helper()
		st, err := s.State(ctx)
		if err != nil || st.ChallengeType != ChallengeHTTP01 || st.ValidationUntested || len(st.DNSResolvers) != 0 {
			t.Errorf("state after a failed save = %+v, %v; want the previous settings", st, err)
		}
		if saved, err := s.DNSTokenSaved(ctx, "cloudflare"); err != nil || saved {
			t.Errorf("DNSTokenSaved after a failed save = %v, %v; want false", saved, err)
		}
		if id, open, err := s.OpenOrderID(ctx); err != nil || !open || id != "order-1" {
			t.Errorf("OpenOrderID after a failed save = %q, %v, %v; want order-1 still open", id, open, err)
		}
	}
	insertOrder := func(t *testing.T, s *Store) {
		t.Helper()
		key, err := newKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertOrder(ctx, newTestOrder("order-1", now), key); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the token cannot be sealed", func(t *testing.T) {
		_, st, _ := newTestStore(t)
		s := NewStore(st, &vault.Vault{}) // a vault holding no key
		insertOrder(t, newTestStoreWithVault(t, st))
		if err := s.SaveValidation(ctx, write); err == nil {
			t.Fatal("SaveValidation succeeded with a vault that holds no key")
		}
		assertUntouched(t, NewStore(st, mustVault(t)))
	})

	t.Run("a write fails after the settings were written", func(t *testing.T) {
		s, st, _ := newTestStore(t)
		insertOrder(t, s)
		if _, err := st.Write.ExecContext(ctx, `CREATE TRIGGER force_failure BEFORE INSERT ON acme_dns_credentials BEGIN SELECT RAISE(ABORT, 'forced'); END`); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveValidation(ctx, write); err == nil {
			t.Fatal("SaveValidation succeeded although the token write failed")
		}
		assertUntouched(t, s)
	})
}

func mustVault(t *testing.T) *vault.Vault {
	t.Helper()
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func newTestStoreWithVault(t *testing.T, st *store.Store) *Store { return NewStore(st, mustVault(t)) }

func TestSaveValidationClosesTheOpenOrderAndResetsOnlyDNSFailures(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	failed := func(class string) State {
		return State{
			Mode: ModeActive, ChallengeType: ChallengeHTTP01, ConsecutiveFailures: 4, LastErrorClass: class,
			NextAttemptAt: now.Add(time.Hour), RetryAfterUntil: now.Add(2 * time.Hour),
		}
	}
	for _, tc := range []struct {
		class     string
		wantReset bool
	}{
		{"dns_token_invalid", true},
		{"dns_not_visible", true},
		{ClassValidationTXT, false},
		{ClassValidationDNS, false},
		{ClassRateLimited, false},
	} {
		t.Run(tc.class, func(t *testing.T) {
			s, _, _ := newTestStore(t)
			key, err := newKey()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.InsertOrder(ctx, newTestOrder("order-1", now), key); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveState(ctx, failed(tc.class)); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveValidation(ctx, ValidationWrite{
				ChallengeType: ChallengeDNS01, Provider: "cloudflare", CloseOpenOrder: true, ResetDNSFailures: true, Now: now,
			}); err != nil {
				t.Fatalf("SaveValidation: %v", err)
			}
			if _, open, err := s.OpenOrderID(ctx); err != nil || open {
				t.Errorf("OpenOrderID after the save = open %v, %v; want the order closed", open, err)
			}
			st, err := s.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reset := st.ConsecutiveFailures == 0 && st.NextAttemptAt.IsZero() && st.RetryAfterUntil.IsZero()
			if reset != tc.wantReset {
				t.Errorf("class %s: backoff reset = %v (%d failures, next %v, retry-after %v), want %v",
					tc.class, reset, st.ConsecutiveFailures, st.NextAttemptAt, st.RetryAfterUntil, tc.wantReset)
			}
		})
	}
}

func TestOpenOrderID(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	t0 := time.Unix(1_700_000_000, 0)
	if id, found, err := s.OpenOrderID(ctx); found || err != nil {
		t.Fatalf("OpenOrderID with no order = %q, %v, %v; want not found", id, found, err)
	}
	for i, id := range []string{"order-1", "order-2"} {
		key, err := newKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.InsertOrder(ctx, newTestOrder(id, t0.Add(time.Duration(i)*time.Second)), key); err != nil {
			t.Fatal(err)
		}
	}
	if id, found, err := s.OpenOrderID(ctx); err != nil || !found || id != "order-2" {
		t.Errorf("OpenOrderID = %q, %v, %v; want the newest open order, order-2", id, found, err)
	}
	if err := s.CloseOrder(ctx, "order-2", StageFailed, t0); err != nil {
		t.Fatal(err)
	}
	if id, found, err := s.OpenOrderID(ctx); err != nil || !found || id != "order-1" {
		t.Errorf("OpenOrderID after closing order-2 = %q, %v, %v; want order-1", id, found, err)
	}
}

func TestFailureGuardDNSIgnoresDNSFailures(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	classes := []string{"dns_token_invalid", "dns_not_visible", "dns_zone_not_found", "dns_rate_limited", "validation_txt", "validation_dns", "internal"}
	for i, class := range classes {
		id := "run-" + class
		if err := s.InsertRun(ctx, Run{
			ID: id, Kind: "issue", Trigger: "admin", StartedAt: base.Add(time.Duration(i) * time.Minute),
			Outcome: OutcomeRunning, ChallengeType: ChallengeDNS01,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRun(ctx, id, "failed", class, base.Add(time.Hour), State{Mode: ModeActive}); err != nil {
			t.Fatal(err)
		}
	}
	starts, err := s.FailedRunsSince(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	// Only validation_txt, validation_dns and internal count: the four dns_ classes happened before the CA was asked.
	want := []time.Time{base.Add(4 * time.Minute), base.Add(5 * time.Minute), base.Add(6 * time.Minute)}
	if len(starts) != len(want) {
		t.Fatalf("FailedRunsSince = %v, want %v", starts, want)
	}
	for i := range want {
		if !starts[i].Equal(want[i]) {
			t.Errorf("FailedRunsSince[%d] = %v, want %v", i, starts[i], want[i])
		}
	}
}

func TestRunsCarryTheirChallenge(t *testing.T) {
	ctx := context.Background()
	s, st, _ := newTestStore(t)
	base := time.Unix(1_800_000_000, 0)
	if err := s.InsertRun(ctx, Run{ID: "run-dns", Kind: "issue", Trigger: "admin", StartedAt: base.Add(time.Minute), Outcome: OutcomeRunning, ChallengeType: ChallengeDNS01}); err != nil {
		t.Fatal(err)
	}
	// A row from before the column existed reads http-01.
	if _, err := st.Write.ExecContext(ctx,
		`INSERT INTO acme_runs (id, kind, trigger, actor, names, started_at, finished_at, outcome, error_class) VALUES ('run-old', 'issue', 'admin', '', '[]', ?, 0, 'ok', '')`,
		base.UnixNano()); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("Runs = %d, %v; want 2", len(runs), err)
	}
	if runs[0].ID != "run-dns" || runs[0].ChallengeType != ChallengeDNS01 {
		t.Errorf("newest run = %q with challenge %q; want run-dns with dns-01", runs[0].ID, runs[0].ChallengeType)
	}
	if runs[1].ID != "run-old" || runs[1].ChallengeType != ChallengeHTTP01 {
		t.Errorf("old run = %q with challenge %q; want run-old with http-01", runs[1].ID, runs[1].ChallengeType)
	}
}

func TestInstallIDIsMintedOnce(t *testing.T) {
	ctx := context.Background()
	s, st, v := newTestStore(t)
	first, err := s.InstallID(ctx)
	if err != nil {
		t.Fatalf("InstallID: %v", err)
	}
	if id, perr := uuid.Parse(first); perr != nil || id.Version() != 7 {
		t.Errorf("InstallID = %q (%v); want a UUIDv7", first, perr)
	}
	if again, err := s.InstallID(ctx); err != nil || again != first {
		t.Errorf("a second InstallID = %q, %v; want %q", again, err, first)
	}
	if fresh, err := NewStore(st, v).InstallID(ctx); err != nil || fresh != first {
		t.Errorf("InstallID from a new Store over the same file = %q, %v; want %q", fresh, err, first)
	}
}

func TestInstallIDConcurrentFirstCallsAgree(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newTestStore(t)
	const callers = 8
	ids := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = s.InstallID(ctx)
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] || ids[i] == "" {
			t.Errorf("caller %d got %q, caller 0 got %q; want one ID", i, ids[i], ids[0])
		}
	}
}
