// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto/ecdsa"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// accountStore is the acme.AccountStore of the per-hostname manager: its accounts live in ingress_acme_account,
// one per directory URL, and never in the own certificate's acme_account (D-19, D-36). Both can be registered
// with the same directory.
type accountStore struct {
	s *Store
}

var _ acme.AccountStore = accountStore{}

// AccountStore returns the account store acme.Client runs against for the per-hostname certificates.
func (s *Store) AccountStore() acme.AccountStore {
	return accountStore{s: s}
}

// SaveAccount stores the account and seals its key under ingress-acme-account/v1/<directory url>. The key is
// durable when SaveAccount returns.
func (a accountStore) SaveAccount(ctx context.Context, acct acme.Account, key *ecdsa.PrivateKey) error {
	keyPEM, err := encodeECKey(key)
	if err != nil {
		return err
	}
	sealed, err := a.s.v.Seal(vault.ContextIngressACMEAccountKey+acct.DirectoryURL, keyPEM)
	if err != nil {
		return fmt.Errorf("ingresscert: seal the ACME account key %q: %w", acct.DirectoryURL, err)
	}
	return a.s.writeDurable(ctx, nil, "the ACME account key", func(_ *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.UpsertIngressACMEAccount(ctx, sqlc.UpsertIngressACMEAccountParams{
			DirectoryUrl: acct.DirectoryURL, AccountUrl: acct.URL, KeySealed: sealed, Contact: acct.Contact,
			TosUrl: acct.TermsOfService, CreatedAt: toNanos(acct.CreatedAt),
		}); err != nil {
			return false, fmt.Errorf("ingresscert: save the ACME account %q: %w", acct.DirectoryURL, err)
		}
		return false, nil
	})
}

// Account returns the account registered with directoryURL and its key. found is false, with a nil error, only
// when there is no such row: an envelope that does not open is an error, so a caller can never mistake an
// unreadable key for a missing account and register a second one.
func (a accountStore) Account(ctx context.Context, directoryURL string) (acme.Account, *ecdsa.PrivateKey, bool, error) {
	row, err := a.s.st.RQ.GetIngressACMEAccount(ctx, directoryURL)
	if errors.Is(err, sql.ErrNoRows) {
		return acme.Account{}, nil, false, nil
	}
	if err != nil {
		return acme.Account{}, nil, false, fmt.Errorf("ingresscert: read the ACME account %q: %w", directoryURL, err)
	}
	keyPEM, err := a.s.v.Open(vault.ContextIngressACMEAccountKey+directoryURL, row.KeySealed)
	if err != nil {
		return acme.Account{}, nil, false, fmt.Errorf("ingresscert: open the ACME account key %q: %w", directoryURL, err)
	}
	key, err := decodeECKey(keyPEM)
	if err != nil {
		return acme.Account{}, nil, false, fmt.Errorf("ingresscert: decode the ACME account key %q: %w", directoryURL, err)
	}
	return accountOf(row), key, true, nil
}

// ForgetAccount removes the account registered with directoryURL, if there is one. The own account is not
// touched.
func (a accountStore) ForgetAccount(ctx context.Context, directoryURL string) error {
	if err := a.s.st.Q.DeleteIngressACMEAccount(ctx, directoryURL); err != nil {
		return fmt.Errorf("ingresscert: delete the ACME account %q: %w", directoryURL, err)
	}
	return nil
}

// accountOf converts a row, without its key.
func accountOf(row sqlc.IngressAcmeAccount) acme.Account {
	return acme.Account{
		DirectoryURL: row.DirectoryUrl, URL: row.AccountUrl, Contact: row.Contact,
		TermsOfService: row.TosUrl, CreatedAt: fromNanos(row.CreatedAt),
	}
}

// AccountURL returns the URL of the ingress account registered with directoryURL without opening its key, for a
// reader that only needs to know whether and under which account certificates are ordered. found is false, with
// a nil error, when there is no account.
func (s *Store) AccountURL(ctx context.Context, directoryURL string) (string, bool, error) {
	row, err := s.st.RQ.GetIngressACMEAccount(ctx, directoryURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("ingresscert: read the ACME account %q: %w", directoryURL, err)
	}
	return row.AccountUrl, true, nil
}
