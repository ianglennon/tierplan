// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// tlsCertStore keeps the server's TLS certificates and their private keys.
type tlsCertStore interface {
	// Load returns the certificate and key stored under id. found is false, with a nil error,
	// only when no row has that id: a row that cannot be read is an error, so a caller can never
	// mistake it for a first start and replace a key that is only unreadable (D-11).
	Load(ctx context.Context, id string) (certPEM, keyPEM []byte, found bool, err error)
	// Insert stores a new certificate and key under id. The key is durable when Insert returns.
	Insert(ctx context.Context, id string, certPEM, keyPEM []byte, now time.Time) error
	// UpdateCert replaces the certificate under id and leaves the key as it is.
	UpdateCert(ctx context.Context, id string, certPEM []byte, now time.Time) error
}

// sealedCertStore is the production tlsCertStore: the key lives in the tls_certs table as a vault
// envelope sealed under vault.ContextTLSKey and the row's ID, and is never written anywhere else.
type sealedCertStore struct {
	st *store.Store
	v  *vault.Vault
}

var _ tlsCertStore = sealedCertStore{}

func (s sealedCertStore) Load(ctx context.Context, id string) (certPEM, keyPEM []byte, found bool, err error) {
	row, err := s.st.RQ.GetTLSCert(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("read the TLS certificate %q: %w", id, err)
	}
	key, err := s.v.Open(vault.ContextTLSKey+id, row.KeySealed)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open the TLS key %q: %w", id, err)
	}
	return []byte(row.CertPem), key, true, nil
}

func (s sealedCertStore) Insert(ctx context.Context, id string, certPEM, keyPEM []byte, now time.Time) error {
	sealed, err := s.v.Seal(vault.ContextTLSKey+id, keyPEM)
	if err != nil {
		return fmt.Errorf("seal the TLS key %q: %w", id, err)
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	ts := now.UnixNano()
	if err := s.st.Q.WithTx(tx).InsertTLSCert(ctx, sqlc.InsertTLSCertParams{
		ID: id, CertPem: string(certPEM), KeySealed: sealed, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		return fmt.Errorf("insert the TLS certificate %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx)
}

// checkpoint folds the write-ahead log into the database file. The store runs with
// synchronous(NORMAL), under which a committed transaction can still be lost to a power cut until
// the log is checkpointed; a key that was generated, handed to a client or pinned by an agent must
// not be, so Insert does not return before this has finished.
func (s sealedCertStore) checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.st.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("make the TLS key durable: %w", err)
	}
	if busy != 0 {
		return errors.New("make the TLS key durable: the checkpoint was blocked by another connection")
	}
	return nil
}

func (s sealedCertStore) UpdateCert(ctx context.Context, id string, certPEM []byte, now time.Time) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.st.Q.WithTx(tx).UpdateTLSCert(ctx, sqlc.UpdateTLSCertParams{
		CertPem: string(certPEM), UpdatedAt: now.UnixNano(), ID: id,
	}); err != nil {
		return fmt.Errorf("update the TLS certificate %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// mirrorCertFile is the public certificate's file name under <DataDir>/tls.
const mirrorCertFile = "server.crt"

// writeMirror is not written yet.
func writeMirror(string, []byte) error { return errors.New("not implemented") }
