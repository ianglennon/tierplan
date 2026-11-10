// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// CertID is the tls_certs row that holds the Let's Encrypt certificate and its key. The self-signed
// row ("selfsigned") is a different row, and nothing in this package writes to it.
const CertID = "acme"

// The modes of the acme_state row.
const (
	ModeOff    = "off"
	ModeActive = "active"
)

// PausedRestore is the paused_reason a restore sets: the restored instance may not be the one the
// domain points at, so renewal waits for a person.
const PausedRestore = "restore"

// The stages of an order, in the order it moves through them.
const (
	StageCreated            = "created"
	StageChallengeSubmitted = "challenge_submitted"
	StageReady              = "ready"
	StageFinalizing         = "finalizing"
	StageStored             = "stored"
	StageFailed             = "failed"
)

// Account is the ACME account registered with one directory. Its private key is kept apart: it is
// only ever returned by Store.Account, and never stored in the clear.
type Account struct {
	DirectoryURL   string
	URL            string
	Contact        string
	TermsOfService string
	CreatedAt      time.Time
}

// State is the singleton acme_state row. Every instant is the zero time.Time when it is not set, and
// the zero State is the empty ledger: mode off, no pause, no failures, no backoff in force.
type State struct {
	Mode                string
	ChallengeType       string
	Profile             string
	PausedReason        string
	ConsecutiveFailures int
	LastAttemptAt       time.Time
	NextAttemptAt       time.Time
	RetryAfterUntil     time.Time
	RenewAt             time.Time
	AriWindowStart      time.Time
	AriWindowEnd        time.Time
	AriCheckedAt        time.Time
	AriNextCheckAt      time.Time
	LastErrorAt         time.Time
	LastSuccessAt       time.Time
	RenewBasis          string
	LastErrorClass      string
	LastErrorDetail     string
	LastSuccessKind     string
	UpdatedAt           time.Time
}

// Order is one ACME order: the work of getting or renewing one certificate. Its fresh certificate
// key is kept apart, like the account key.
type Order struct {
	ID           string
	DirectoryURL string
	URL          string
	Kind         string // "issue" or "renew"
	Names        []string
	Profile      string
	Replaces     string
	Stage        string
	Status       string // "open" or "closed"
	ResumeCount  int
	ExpiresAt    time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Run is one entry of the run history.
type Run struct {
	ID         string
	Kind       string
	Trigger    string
	Actor      string
	Names      []string
	StartedAt  time.Time
	FinishedAt time.Time
	Outcome    string
	ErrorClass string
}

// Store keeps everything ACME persists: the account, the state and ledger, the open order, the run
// history and the served certificate. Private keys are vault envelopes in every column that holds
// one; they are never written anywhere else.
type Store struct {
	st *store.Store
	v  *vault.Vault
}

// NewStore returns the Store over st, sealing with v.
func NewStore(st *store.Store, v *vault.Vault) *Store {
	return &Store{st: st, v: v}
}

// SaveAccount stores the account and seals its key under the account's directory URL. The key is
// durable when SaveAccount returns.
func (s *Store) SaveAccount(ctx context.Context, a Account, key *ecdsa.PrivateKey) error {
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	sealed, err := s.v.Seal(vault.ContextACMEAccountKey+a.DirectoryURL, keyPEM)
	if err != nil {
		return fmt.Errorf("seal the ACME account key %q: %w", a.DirectoryURL, err)
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.st.Q.WithTx(tx).InsertACMEAccount(ctx, sqlc.InsertACMEAccountParams{
		DirectoryUrl: a.DirectoryURL, AccountUrl: a.URL, KeySealed: sealed,
		Contact: a.Contact, TosUrl: a.TermsOfService, CreatedAt: toNanos(a.CreatedAt),
	}); err != nil {
		return fmt.Errorf("insert the ACME account %q: %w", a.DirectoryURL, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the ACME account key")
}

// Account returns the account registered with directoryURL and its key. found is false, with a nil
// error, only when there is no such row: an envelope that does not open is an error, so a caller can
// never mistake an unreadable key for a missing account and register a second one.
func (s *Store) Account(ctx context.Context, directoryURL string) (Account, *ecdsa.PrivateKey, bool, error) {
	row, err := s.st.RQ.GetACMEAccount(ctx, directoryURL)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, nil, false, nil
	}
	if err != nil {
		return Account{}, nil, false, fmt.Errorf("read the ACME account %q: %w", directoryURL, err)
	}
	keyPEM, err := s.v.Open(vault.ContextACMEAccountKey+directoryURL, row.KeySealed)
	if err != nil {
		return Account{}, nil, false, fmt.Errorf("open the ACME account key %q: %w", directoryURL, err)
	}
	key, err := decodeKey(keyPEM)
	if err != nil {
		return Account{}, nil, false, fmt.Errorf("decode the ACME account key %q: %w", directoryURL, err)
	}
	return Account{
		DirectoryURL: row.DirectoryUrl, URL: row.AccountUrl, Contact: row.Contact,
		TermsOfService: row.TosUrl, CreatedAt: fromNanos(row.CreatedAt),
	}, key, true, nil
}

// ForgetAccount removes the account registered with directoryURL, if there is one.
func (s *Store) ForgetAccount(ctx context.Context, directoryURL string) error {
	if err := s.st.Q.DeleteACMEAccount(ctx, directoryURL); err != nil {
		return fmt.Errorf("delete the ACME account %q: %w", directoryURL, err)
	}
	return nil
}

// checkpoint folds the write-ahead log into the database file. The store runs with
// synchronous(NORMAL), under which a committed transaction can still be lost to a power cut until the
// log is checkpointed; a key that is about to be used to talk to a certificate authority must not be,
// so the writers here do not return before this has finished (RESEARCH Pitfall 2).
func (s *Store) checkpoint(ctx context.Context, what string) error {
	var busy, logFrames, checkpointed int
	if err := s.st.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("make %s durable: %w", what, err)
	}
	if busy != 0 {
		return fmt.Errorf("make %s durable: the checkpoint was blocked by another connection", what)
	}
	return nil
}

// toNanos is the stored form of an instant: unix nanoseconds, with the zero time as 0.
func toNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromNanos is the inverse of toNanos.
func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// newKey makes an ECDSA P-256 key.
func newKey() (*ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	return key, nil
}

// encodeKey writes an ECDSA P-256 key as PKCS#8 PEM.
func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode the key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// decodeKey reads an ECDSA P-256 key from its PKCS#8 PEM encoding.
func decodeKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("the stored key is not an ECDSA P-256 key")
	}
	return key, nil
}

// The rest of the Store is written in the next commit.

var errNotImplemented = errors.New("acme: not implemented")

// InsertOrder stores a new open order and seals its certificate key.
func (s *Store) InsertOrder(context.Context, Order, *ecdsa.PrivateKey) error {
	return errNotImplemented
}

// OpenOrder returns the newest open order and its key.
func (s *Store) OpenOrder(context.Context) (Order, *ecdsa.PrivateKey, bool, error) {
	return Order{}, nil, false, errNotImplemented
}

// UpdateOrder moves an order to a stage.
func (s *Store) UpdateOrder(context.Context, string, string, int, time.Time) error {
	return errNotImplemented
}

// CloseOrder closes an order.
func (s *Store) CloseOrder(context.Context, string, string, time.Time) error {
	return errNotImplemented
}

// StoreIssued stores the issued certificate and closes its order.
func (s *Store) StoreIssued(context.Context, string, []byte, *ecdsa.PrivateKey, time.Time) error {
	return errNotImplemented
}

// Certificate returns the stored certificate chain and key.
func (s *Store) Certificate(context.Context) ([]byte, *ecdsa.PrivateKey, bool, error) {
	return nil, nil, false, errNotImplemented
}

// State returns the ledger.
func (s *Store) State(context.Context) (State, error) { return State{}, errNotImplemented }

// SaveState stores the ledger.
func (s *Store) SaveState(context.Context, State) error { return errNotImplemented }

// InsertRun records a new run.
func (s *Store) InsertRun(context.Context, Run) error { return errNotImplemented }

// FinishRun finishes a run and stores the ledger.
func (s *Store) FinishRun(context.Context, string, string, string, time.Time, State) error {
	return errNotImplemented
}

// Runs returns the newest runs.
func (s *Store) Runs(context.Context, int) ([]Run, error) { return nil, errNotImplemented }

// FailedRunsSince returns the start times of the failed runs since a time.
func (s *Store) FailedRunsSince(context.Context, time.Time) ([]time.Time, error) {
	return nil, errNotImplemented
}

// InterruptRunning marks the runs still running as failed.
func (s *Store) InterruptRunning(context.Context, time.Time) error { return errNotImplemented }
