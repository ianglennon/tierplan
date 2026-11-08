// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// mirrorCertFile is the public certificate's file name under <DataDir>/tls. It is one neutral name
// for the default certificate and for any later source, so the healthcheck reads one path.
const mirrorCertFile = "server.crt"

// writeMirror puts the public certificate where the healthcheck can read it without opening the
// database: <dataDir>/tls/server.crt, mode 0644, replaced atomically. Only the certificate goes
// here; the key stays in the database as a vault envelope.
func writeMirror(dataDir string, certPEM []byte) error {
	dir := filepath.Join(dataDir, selfSignedDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return writeAtomic(filepath.Join(dir, mirrorCertFile), certPEM, 0o644)
}

// selfSignedID is the tls_certs row that holds the certificate TierPlan made for itself.
const selfSignedID = "selfsigned"

// prepareSelfSigned returns the self-signed certificate to serve and its key, creating them on the
// first start.
//
// The key is what agents pin, so it is created once and then only read. A stored certificate that
// still carries every wanted name and is inside its validity is served as it is; one that does not is
// signed again with the stored key, which leaves the SPKI pin alone. A key is generated only when no
// row exists. A row that cannot be opened or read is an error that stops startup: a replacement key
// would silently change the pin every agent holds, so the person must decide (T-13.1-03). Either way
// the public certificate is mirrored for the healthcheck before this returns.
func prepareSelfSigned(ctx context.Context, store tlsCertStore, dataDir string, want names, now time.Time) (tls.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, keyPEM, found, err := store.Load(ctx, selfSignedID)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if !found {
		key, err := newKey()
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		cert, certPEM, err := signSelfSigned(key, want, now)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		keyPEM, err := encodeKey(key)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		if err := store.Insert(ctx, selfSignedID, certPEM, keyPEM, now); err != nil {
			return tls.Certificate{}, nil, err
		}
		logNewKey(false, cert.Certificate[0])
		if err := writeMirror(dataDir, certPEM); err != nil {
			return tls.Certificate{}, nil, err
		}
		return cert, key, nil
	}

	key, err := decodeKey(keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("the stored TLS key %q is unreadable and was not replaced, because a new key would change the pin every agent holds: %w", selfSignedID, err)
	}
	if block, _ := pem.Decode(certPEM); block != nil && block.Type == "CERTIFICATE" {
		if leaf, perr := x509.ParseCertificate(block.Bytes); perr == nil &&
			want.covers(leaf) && !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter) {
			if err := writeMirror(dataDir, certPEM); err != nil {
				return tls.Certificate{}, nil, err
			}
			return tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key}, key, nil
		}
	}
	// Corrupt, other names or outside its validity: sign again with the stored key.
	cert, certPEM, err := signSelfSigned(key, want, now)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := store.UpdateCert(ctx, selfSignedID, certPEM, now); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := writeMirror(dataDir, certPEM); err != nil {
		return tls.Certificate{}, nil, err
	}
	return cert, key, nil
}

// signSelfSigned signs a certificate for want with key and returns it, with its PEM encoding.
func signSelfSigned(key *ecdsa.PrivateKey, want names, now time.Time) (tls.Certificate, []byte, error) {
	der, err := signCertificate(key, want, now)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
