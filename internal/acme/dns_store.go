// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

var errNotImplemented = errors.New("not implemented")

// InsertDNSRecord stores a ledger row and does not return before it is durable.
func (s *Store) InsertDNSRecord(ctx context.Context, r DNSRecord) error { return errNotImplemented }

// DNSRecordsFor returns the rows of one owner for one ACME challenge token, oldest first.
func (s *Store) DNSRecordsFor(ctx context.Context, ownerKind, ownerID, token string) ([]DNSRecord, error) {
	return nil, errNotImplemented
}

// PendingDNSRecord finds a row holding exactly this record at the provider.
func (s *Store) PendingDNSRecord(ctx context.Context, provider, zoneID, name, value string) (DNSRecord, bool, error) {
	return DNSRecord{}, false, errNotImplemented
}

// RestampDNSRecord moves a row to a new owner and token.
func (s *Store) RestampDNSRecord(ctx context.Context, id, ownerKind, ownerID, token string) error {
	return errNotImplemented
}

// DeleteDNSRecord removes exactly one row by TierPlan's row ID.
func (s *Store) DeleteDNSRecord(ctx context.Context, id string) error { return errNotImplemented }

// MarkDNSRecordFailed stamps a failed delete on a row.
func (s *Store) MarkDNSRecordFailed(ctx context.Context, id string, attempts int, lastTry, nextTry time.Time, class string) error {
	return errNotImplemented
}

// DueDNSRecords returns the rows whose next try has come, oldest first.
func (s *Store) DueDNSRecords(ctx context.Context, now time.Time) ([]DNSRecord, error) {
	return nil, errNotImplemented
}

// DNSRecords returns every row, oldest first.
func (s *Store) DNSRecords(ctx context.Context) ([]DNSRecord, error) { return nil, errNotImplemented }
