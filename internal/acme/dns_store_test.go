// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"log/slog"
	"slices"
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

func ledgerRow(id, ownerKind, ownerID, token string, created time.Time) DNSRecord {
	return DNSRecord{
		ID: id, Provider: "cloudflare", ZoneID: "zone-1", RecordID: "rec-" + id,
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
