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
