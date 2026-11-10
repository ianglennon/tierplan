// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// SaveDNSToken seals token under the provider's context and stores it, replacing any token saved for
// the provider. The token is durable when SaveDNSToken returns.
func (s *Store) SaveDNSToken(ctx context.Context, provider, token string, now time.Time) error {
	sealed, err := s.v.Seal(vault.ContextACMEDNSToken+provider, []byte(token))
	if err != nil {
		return fmt.Errorf("seal the %s DNS token: %w", provider, err)
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.st.Q.WithTx(tx).UpsertACMEDNSCredential(ctx, sqlc.UpsertACMEDNSCredentialParams{
		Provider: provider, TokenSealed: sealed, CreatedAt: toNanos(now),
	}); err != nil {
		return fmt.Errorf("store the %s DNS token: %w", provider, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the DNS token")
}

// DNSToken returns the saved token of provider. found is false, with a nil error, only when none is
// saved: an envelope that does not open is an error.
func (s *Store) DNSToken(ctx context.Context, provider string) (token string, found bool, err error) {
	row, err := s.st.RQ.GetACMEDNSCredential(ctx, provider)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the %s DNS token: %w", provider, err)
	}
	plain, err := s.v.Open(vault.ContextACMEDNSToken+provider, row.TokenSealed)
	if err != nil {
		return "", false, fmt.Errorf("open the %s DNS token: %w", provider, err)
	}
	return string(plain), true, nil
}

// DNSTokenSaved reports whether a token is saved for provider, without opening it.
func (s *Store) DNSTokenSaved(ctx context.Context, provider string) (bool, error) {
	n, err := s.st.RQ.CountACMEDNSCredential(ctx, provider)
	if err != nil {
		return false, fmt.Errorf("count the %s DNS token: %w", provider, err)
	}
	return n > 0, nil
}

// DeleteDNSToken removes the token saved for provider, if there is one.
func (s *Store) DeleteDNSToken(ctx context.Context, provider string) error {
	if err := s.st.Q.DeleteACMEDNSCredential(ctx, provider); err != nil {
		return fmt.Errorf("delete the %s DNS token: %w", provider, err)
	}
	return nil
}

// The kinds of owner a ledger row belongs to.
const (
	// OwnerOrder is a record made for an ACME order; the owner ID is the order's ID.
	OwnerOrder = "order"
	// OwnerTest is a record made by Test token; the owner ID names the test.
	OwnerTest = "test"
)

// DNSRecord is one row of the record-ID ledger: a TXT record TierPlan created at a DNS provider. The
// provider's own reference (ZoneID, RecordID) is the only thing a delete uses.
type DNSRecord struct {
	// ID is TierPlan's own row ID.
	ID                   string
	Provider             string
	ZoneID, RecordID     string
	Name, Value, Comment string
	OwnerKind, OwnerID   string
	Token                string
	CreatedAt            time.Time
	Attempts             int
	LastTryAt, NextTryAt time.Time
	LastErrorClass       string
}

// InsertDNSRecord stores a ledger row and does not return before it is durable: the provider's record
// ID is on disk before the CA is asked to validate, so a crash after this call can still delete the
// record.
func (s *Store) InsertDNSRecord(ctx context.Context, r DNSRecord) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.st.Q.WithTx(tx).InsertACMEDNSRecord(ctx, sqlc.InsertACMEDNSRecordParams{
		ID: r.ID, Provider: r.Provider, ZoneID: r.ZoneID, RecordID: r.RecordID, Name: r.Name, Value: r.Value,
		Comment: r.Comment, OwnerKind: r.OwnerKind, OwnerID: r.OwnerID, Token: r.Token,
		CreatedAt: toNanos(r.CreatedAt), Attempts: int64(r.Attempts), LastTryAt: toNanos(r.LastTryAt),
		NextTryAt: toNanos(r.NextTryAt), LastErrorClass: r.LastErrorClass,
	}); err != nil {
		return fmt.Errorf("insert the DNS record %q: %w", r.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the DNS record ID")
}

// DNSRecordsFor returns the rows of one owner for one ACME challenge token, oldest first.
func (s *Store) DNSRecordsFor(ctx context.Context, ownerKind, ownerID, token string) ([]DNSRecord, error) {
	rows, err := s.st.RQ.ListACMEDNSRecordsByOwner(ctx, sqlc.ListACMEDNSRecordsByOwnerParams{
		OwnerKind: ownerKind, OwnerID: ownerID, Token: token,
	})
	if err != nil {
		return nil, fmt.Errorf("list the DNS records of %s %q: %w", ownerKind, ownerID, err)
	}
	return dnsRecords(rows), nil
}

// PendingDNSRecord finds a row holding exactly this record at the provider (newest first), so a resumed
// order reuses the record it already made instead of creating a second one.
func (s *Store) PendingDNSRecord(ctx context.Context, provider, zoneID, name, value string) (DNSRecord, bool, error) {
	row, err := s.st.RQ.GetPendingACMEDNSRecord(ctx, sqlc.GetPendingACMEDNSRecordParams{
		Provider: provider, ZoneID: zoneID, Name: name, Value: value,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DNSRecord{}, false, nil
	}
	if err != nil {
		return DNSRecord{}, false, fmt.Errorf("find the DNS record %q: %w", name, err)
	}
	return dnsRecord(row), true, nil
}

// RestampDNSRecord hands a row to a new owner and ACME token and clears its retry state, because the
// record is in use again.
func (s *Store) RestampDNSRecord(ctx context.Context, id, ownerKind, ownerID, token string) error {
	if err := s.st.Q.RestampACMEDNSRecord(ctx, sqlc.RestampACMEDNSRecordParams{
		OwnerKind: ownerKind, OwnerID: ownerID, Token: token, ID: id,
	}); err != nil {
		return fmt.Errorf("restamp the DNS record %q: %w", id, err)
	}
	return nil
}

// DeleteDNSRecord removes exactly one row by TierPlan's row ID. A row that is not there is not an error.
func (s *Store) DeleteDNSRecord(ctx context.Context, id string) error {
	if err := s.st.Q.DeleteACMEDNSRecord(ctx, id); err != nil {
		return fmt.Errorf("delete the DNS record %q: %w", id, err)
	}
	return nil
}

// MarkDNSRecordFailed stamps a failed delete: how many tries so far, when this one was and the next is,
// and the class of its error.
func (s *Store) MarkDNSRecordFailed(ctx context.Context, id string, attempts int, lastTry, nextTry time.Time, class string) error {
	if err := s.st.Q.MarkACMEDNSRecordFailed(ctx, sqlc.MarkACMEDNSRecordFailedParams{
		Attempts: int64(attempts), LastTryAt: toNanos(lastTry), NextTryAt: toNanos(nextTry), LastErrorClass: class, ID: id,
	}); err != nil {
		return fmt.Errorf("mark the DNS record %q failed: %w", id, err)
	}
	return nil
}

// DueDNSRecords returns the rows whose next try has come (next_try_at at or before now), oldest first.
func (s *Store) DueDNSRecords(ctx context.Context, now time.Time) ([]DNSRecord, error) {
	rows, err := s.st.RQ.ListDueACMEDNSRecords(ctx, toNanos(now))
	if err != nil {
		return nil, fmt.Errorf("list the due DNS records: %w", err)
	}
	return dnsRecords(rows), nil
}

// DNSRecords returns every row, oldest first and, for the same instant, by id.
func (s *Store) DNSRecords(ctx context.Context) ([]DNSRecord, error) {
	rows, err := s.st.RQ.ListACMEDNSRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the DNS records: %w", err)
	}
	return dnsRecords(rows), nil
}

func dnsRecords(rows []sqlc.AcmeDnsRecord) []DNSRecord {
	out := make([]DNSRecord, len(rows))
	for i, r := range rows {
		out[i] = dnsRecord(r)
	}
	return out
}

func dnsRecord(r sqlc.AcmeDnsRecord) DNSRecord {
	return DNSRecord{
		ID: r.ID, Provider: r.Provider, ZoneID: r.ZoneID, RecordID: r.RecordID, Name: r.Name, Value: r.Value,
		Comment: r.Comment, OwnerKind: r.OwnerKind, OwnerID: r.OwnerID, Token: r.Token,
		CreatedAt: fromNanos(r.CreatedAt), Attempts: int(r.Attempts), LastTryAt: fromNanos(r.LastTryAt),
		NextTryAt: fromNanos(r.NextTryAt), LastErrorClass: r.LastErrorClass,
	}
}

// ValidationWrite is one save of the validation settings. Token is nil to keep the saved token.
type ValidationWrite struct {
	// ChallengeType, Provider and Resolvers are the challenge to use and, for DNS-01, the provider and
	// the normalised resolver list.
	ChallengeType, Provider string
	Resolvers               []string
	// Token is the new token to seal and store for Provider, or nil to leave the saved one alone.
	Token *string
	// Untested records that the challenge type changed and no run has proved the new one yet.
	Untested bool
	// CloseOpenOrder closes every open order as failed, so an order made under the old challenge is
	// not resumed under the new one.
	CloseOpenOrder bool
	// ResetDNSFailures clears the backoff that a DNS-side failure put in force.
	ResetDNSFailures bool
	Now              time.Time
}

// SaveValidation writes the validation settings in one transaction: the challenge type, the provider,
// the resolver list and the untested flag, the sealed token when there is one, the closing of an open
// order and the reset of a DNS-side failure. A token that cannot be sealed, or any failed write, leaves
// the previous settings whole. The result is durable when SaveValidation returns.
func (s *Store) SaveValidation(ctx context.Context, w ValidationWrite) error {
	var sealed string
	if w.Token != nil {
		var err error
		if sealed, err = s.v.Seal(vault.ContextACMEDNSToken+w.Provider, []byte(*w.Token)); err != nil {
			return fmt.Errorf("seal the %s DNS token: %w", w.Provider, err)
		}
	}
	resolvers := w.Resolvers
	if resolvers == nil {
		resolvers = []string{}
	}
	encoded, err := json.Marshal(resolvers)
	if err != nil {
		return fmt.Errorf("encode the DNS resolvers: %w", err)
	}
	ts := toNanos(w.Now)

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	var untested int64
	if w.Untested {
		untested = 1
	}
	if err := q.SetACMEValidation(ctx, sqlc.SetACMEValidationParams{
		ChallengeType: w.ChallengeType, DnsProvider: w.Provider, DnsResolvers: string(encoded),
		ValidationUntested: untested, UpdatedAt: ts,
	}); err != nil {
		return fmt.Errorf("save the validation settings: %w", err)
	}
	if w.Token != nil {
		if err := q.UpsertACMEDNSCredential(ctx, sqlc.UpsertACMEDNSCredentialParams{
			Provider: w.Provider, TokenSealed: sealed, CreatedAt: ts,
		}); err != nil {
			return fmt.Errorf("store the %s DNS token: %w", w.Provider, err)
		}
	}
	if w.CloseOpenOrder {
		if err := q.CloseOpenACMEOrders(ctx, sqlc.CloseOpenACMEOrdersParams{Stage: StageFailed, UpdatedAt: ts}); err != nil {
			return fmt.Errorf("close the open ACME order: %w", err)
		}
	}
	if w.ResetDNSFailures {
		if err := q.ResetACMEDNSFailures(ctx, ts); err != nil {
			return fmt.Errorf("reset the DNS failures: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the validation settings")
}

// SetValidationUntested sets or clears the flag that says the saved challenge type has not been proved
// by a run yet. It writes nothing else.
func (s *Store) SetValidationUntested(ctx context.Context, untested bool, now time.Time) error {
	var flag int64
	if untested {
		flag = 1
	}
	if err := s.st.Q.SetACMEValidationUntested(ctx, sqlc.SetACMEValidationUntestedParams{
		ValidationUntested: flag, UpdatedAt: toNanos(now),
	}); err != nil {
		return fmt.Errorf("save the untested flag: %w", err)
	}
	return nil
}

// InstallID returns this install's ID, minting it (a UUIDv7, kept in app_meta) on the first call. It
// goes into the comment of every DNS record TierPlan creates. A restored backup drops app_meta, so a
// restored clone mints its own and never claims the source install's records. Concurrent first calls
// all return the one ID that won.
func (s *Store) InstallID(ctx context.Context) (string, error) {
	id, err := s.st.RQ.GetACMEInstallID(ctx)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("read the install ID: %w", err)
	}
	minted, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("mint the install ID: %w", err)
	}
	if err := s.st.Q.InsertACMEInstallID(ctx, sqlc.InsertACMEInstallIDParams{
		Value: minted.String(), UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		return "", fmt.Errorf("store the install ID: %w", err)
	}
	// Read back: a concurrent first call may have inserted its own ID first, and its ID is the one.
	if id, err = s.st.Q.GetACMEInstallID(ctx); err != nil {
		return "", fmt.Errorf("read the install ID: %w", err)
	}
	return id, s.checkpoint(ctx, "the install ID")
}

// OpenOrderID returns the ID of the newest open order without opening its sealed key. found is false,
// with a nil error, when no order is open.
func (s *Store) OpenOrderID(ctx context.Context) (string, bool, error) {
	id, err := s.st.RQ.GetOpenACMEOrderID(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the open ACME order ID: %w", err)
	}
	return id, true, nil
}
