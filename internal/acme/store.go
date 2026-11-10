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
	"encoding/json"
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

	// afterStateRead is a test seam: ReadSnapshot calls it between reading the state row and reading
	// the runs, so a test can finish a run at exactly that point.
	afterStateRead func()
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

// AccountInfo returns the account registered with directoryURL without opening its key, for a reader
// that only shows it: a key that does not open (a wrong master key after a restore, a damaged row) is
// not its concern, and Account reports it to whoever needs the key.
func (s *Store) AccountInfo(ctx context.Context, directoryURL string) (Account, bool, error) {
	row, err := s.st.RQ.GetACMEAccount(ctx, directoryURL)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, fmt.Errorf("read the ACME account %q: %w", directoryURL, err)
	}
	return Account{
		DirectoryURL: row.DirectoryUrl, URL: row.AccountUrl, Contact: row.Contact,
		TermsOfService: row.TosUrl, CreatedAt: fromNanos(row.CreatedAt),
	}, true, nil
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

// InsertOrder stores a new open order and seals its fresh certificate key under the order's ID. The
// key is durable when InsertOrder returns, which is before any finalize can happen: a certificate
// issued for a key that was then lost could never be installed (RESEARCH Pitfall 2).
func (s *Store) InsertOrder(ctx context.Context, o Order, key *ecdsa.PrivateKey) error {
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	sealed, err := s.v.Seal(vault.ContextACMEOrderKey+o.ID, keyPEM)
	if err != nil {
		return fmt.Errorf("seal the ACME order key %q: %w", o.ID, err)
	}
	names, err := encodeNames(o.Names)
	if err != nil {
		return err
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	if err := s.st.Q.WithTx(tx).InsertACMEOrder(ctx, sqlc.InsertACMEOrderParams{
		ID: o.ID, DirectoryUrl: o.DirectoryURL, OrderUrl: o.URL, Kind: o.Kind, Names: names,
		Profile: o.Profile, Replaces: o.Replaces, CertKeySealed: sealed, Stage: o.Stage,
		ResumeCount: int64(o.ResumeCount), ExpiresAt: toNanos(o.ExpiresAt),
		CreatedAt: toNanos(o.CreatedAt), UpdatedAt: toNanos(o.UpdatedAt),
	}); err != nil {
		return fmt.Errorf("insert the ACME order %q: %w", o.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the ACME order key")
}

// OpenOrder returns the newest open order and its certificate key. found is false, with a nil error,
// only when no order is open: a key that does not open is an error.
func (s *Store) OpenOrder(ctx context.Context) (Order, *ecdsa.PrivateKey, bool, error) {
	row, err := s.st.RQ.GetOpenACMEOrder(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, nil, false, nil
	}
	if err != nil {
		return Order{}, nil, false, fmt.Errorf("read the open ACME order: %w", err)
	}
	keyPEM, err := s.v.Open(vault.ContextACMEOrderKey+row.ID, row.CertKeySealed)
	if err != nil {
		return Order{}, nil, false, fmt.Errorf("open the ACME order key %q: %w", row.ID, err)
	}
	key, err := decodeKey(keyPEM)
	if err != nil {
		return Order{}, nil, false, fmt.Errorf("decode the ACME order key %q: %w", row.ID, err)
	}
	var names []string
	if err := json.Unmarshal([]byte(row.Names), &names); err != nil {
		return Order{}, nil, false, fmt.Errorf("read the names of the ACME order %q: %w", row.ID, err)
	}
	return Order{
		ID: row.ID, DirectoryURL: row.DirectoryUrl, URL: row.OrderUrl, Kind: row.Kind, Names: names,
		Profile: row.Profile, Replaces: row.Replaces, Stage: row.Stage, Status: row.Status,
		ResumeCount: int(row.ResumeCount), ExpiresAt: fromNanos(row.ExpiresAt),
		CreatedAt: fromNanos(row.CreatedAt), UpdatedAt: fromNanos(row.UpdatedAt),
	}, key, true, nil
}

// UpdateOrder moves an order to a stage and records how many times it has been resumed.
func (s *Store) UpdateOrder(ctx context.Context, id, stage string, resumeCount int, now time.Time) error {
	if err := s.st.Q.UpdateACMEOrderStage(ctx, sqlc.UpdateACMEOrderStageParams{
		Stage: stage, ResumeCount: int64(resumeCount), UpdatedAt: toNanos(now), ID: id,
	}); err != nil {
		return fmt.Errorf("update the ACME order %q: %w", id, err)
	}
	return nil
}

// CloseOrder closes an order in a final stage. The order and its sealed key stay for the record, and
// OpenOrder no longer returns it.
func (s *Store) CloseOrder(ctx context.Context, id, stage string, now time.Time) error {
	if err := s.st.Q.CloseACMEOrder(ctx, sqlc.CloseACMEOrderParams{Stage: stage, UpdatedAt: toNanos(now), ID: id}); err != nil {
		return fmt.Errorf("close the ACME order %q: %w", id, err)
	}
	return nil
}

// StoreIssued writes the issued chain and its key into the tls_certs row CertID, with the key sealed
// under the tls-key context the server reads it with, and closes the order as stored, in one
// transaction: a failure keeps neither. The self-signed row is not touched. The certificate is
// durable when StoreIssued returns.
func (s *Store) StoreIssued(ctx context.Context, orderID string, chainPEM []byte, key *ecdsa.PrivateKey, now time.Time) error {
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	sealed, err := s.v.Seal(vault.ContextTLSKey+CertID, keyPEM)
	if err != nil {
		return fmt.Errorf("seal the TLS key %q: %w", CertID, err)
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	ts := toNanos(now)
	if err := q.UpsertTLSCert(ctx, sqlc.UpsertTLSCertParams{
		ID: CertID, CertPem: string(chainPEM), KeySealed: sealed, CreatedAt: ts, UpdatedAt: ts,
	}); err != nil {
		return fmt.Errorf("store the TLS certificate %q: %w", CertID, err)
	}
	if err := q.CloseACMEOrder(ctx, sqlc.CloseACMEOrderParams{Stage: StageStored, UpdatedAt: ts, ID: orderID}); err != nil {
		return fmt.Errorf("close the ACME order %q: %w", orderID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return s.checkpoint(ctx, "the issued certificate")
}

// Certificate returns the stored Let's Encrypt chain and its key. found is false, with a nil error,
// only before the first certificate is issued.
func (s *Store) Certificate(ctx context.Context) (chainPEM []byte, key *ecdsa.PrivateKey, found bool, err error) {
	row, err := s.st.RQ.GetTLSCert(ctx, CertID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("read the TLS certificate %q: %w", CertID, err)
	}
	keyPEM, err := s.v.Open(vault.ContextTLSKey+CertID, row.KeySealed)
	if err != nil {
		return nil, nil, false, fmt.Errorf("open the TLS key %q: %w", CertID, err)
	}
	key, err = decodeKey(keyPEM)
	if err != nil {
		return nil, nil, false, fmt.Errorf("decode the TLS key %q: %w", CertID, err)
	}
	return []byte(row.CertPem), key, true, nil
}

// State returns the ledger. A database that has never run ACME reads as mode off with no instants
// set, which the ledger treats as no backoff being in force.
func (s *Store) State(ctx context.Context) (State, error) {
	return readState(ctx, s.st.RQ)
}

func readState(ctx context.Context, q *sqlc.Queries) (State, error) {
	row, err := q.GetACMEState(ctx)
	if err != nil {
		return State{}, fmt.Errorf("read the ACME state: %w", err)
	}
	return State{
		Mode: row.Mode, ChallengeType: row.ChallengeType, Profile: row.Profile, PausedReason: row.PausedReason,
		ConsecutiveFailures: int(row.ConsecutiveFailures),
		LastAttemptAt:       fromNanos(row.LastAttemptAt), NextAttemptAt: fromNanos(row.NextAttemptAt),
		RetryAfterUntil: fromNanos(row.RetryAfterUntil), RenewAt: fromNanos(row.RenewAt),
		AriWindowStart: fromNanos(row.AriWindowStart), AriWindowEnd: fromNanos(row.AriWindowEnd),
		AriCheckedAt: fromNanos(row.AriCheckedAt), AriNextCheckAt: fromNanos(row.AriNextCheckAt),
		LastErrorAt: fromNanos(row.LastErrorAt), LastSuccessAt: fromNanos(row.LastSuccessAt),
		RenewBasis: row.RenewBasis, LastErrorClass: row.LastErrorClass, LastErrorDetail: row.LastErrorDetail,
		LastSuccessKind: row.LastSuccessKind, UpdatedAt: fromNanos(row.UpdatedAt),
	}, nil
}

// Snapshot is the state row, the newest runs and the starts of the recent failed runs as they stood
// at one instant.
type Snapshot struct {
	State State
	// Runs are the newest runs, newest first.
	Runs []Run
	// FailedStarts are when the failed runs since the asked instant started, oldest first.
	FailedStarts []time.Time
}

// ReadSnapshot reads the state row, up to runLimit runs and the failed runs since failedSince in one
// read transaction. FinishRun writes a run's outcome and the state in one transaction, so a snapshot
// never holds the new outcome with the old last error, or the other way round.
func (s *Store) ReadSnapshot(ctx context.Context, runLimit int, failedSince time.Time) (Snapshot, error) {
	tx, err := s.st.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a read transaction has nothing to commit
	q := s.st.RQ.WithTx(tx)
	var snap Snapshot
	if snap.State, err = readState(ctx, q); err != nil {
		return Snapshot{}, err
	}
	if s.afterStateRead != nil {
		s.afterStateRead()
	}
	if snap.Runs, err = readRuns(ctx, q, runLimit); err != nil {
		return Snapshot{}, err
	}
	if snap.FailedStarts, err = readFailedRunsSince(ctx, q, failedSince); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// SaveState stores the ledger. The row always exists (the migration seeds it), so this is an update.
func (s *Store) SaveState(ctx context.Context, st State) error {
	if err := s.st.Q.UpdateACMEState(ctx, stateParams(st)); err != nil {
		return fmt.Errorf("save the ACME state: %w", err)
	}
	return nil
}

func stateParams(st State) sqlc.UpdateACMEStateParams {
	return sqlc.UpdateACMEStateParams{
		Mode: st.Mode, ChallengeType: st.ChallengeType, Profile: st.Profile, PausedReason: st.PausedReason,
		ConsecutiveFailures: int64(st.ConsecutiveFailures),
		LastAttemptAt:       toNanos(st.LastAttemptAt), NextAttemptAt: toNanos(st.NextAttemptAt),
		RetryAfterUntil: toNanos(st.RetryAfterUntil), RenewAt: toNanos(st.RenewAt),
		AriWindowStart: toNanos(st.AriWindowStart), AriWindowEnd: toNanos(st.AriWindowEnd),
		AriCheckedAt: toNanos(st.AriCheckedAt), AriNextCheckAt: toNanos(st.AriNextCheckAt),
		LastErrorAt: toNanos(st.LastErrorAt), LastSuccessAt: toNanos(st.LastSuccessAt),
		RenewBasis: st.RenewBasis, LastErrorClass: st.LastErrorClass, LastErrorDetail: st.LastErrorDetail,
		LastSuccessKind: st.LastSuccessKind, UpdatedAt: toNanos(st.UpdatedAt),
	}
}

// InsertRun records a new run, normally with outcome "running".
func (s *Store) InsertRun(ctx context.Context, r Run) error {
	names, err := encodeNames(r.Names)
	if err != nil {
		return err
	}
	if err := s.st.Q.InsertACMERun(ctx, sqlc.InsertACMERunParams{
		ID: r.ID, Kind: r.Kind, Trigger: r.Trigger, Actor: r.Actor, Names: names,
		StartedAt: toNanos(r.StartedAt), FinishedAt: toNanos(r.FinishedAt), Outcome: r.Outcome, ErrorClass: r.ErrorClass,
	}); err != nil {
		return fmt.Errorf("insert the ACME run %q: %w", r.ID, err)
	}
	return nil
}

// FinishRun records how a run ended and stores the ledger that results from it, in one transaction, so
// the history and the backoff never disagree. It then prunes the history to the newest 50 runs.
func (s *Store) FinishRun(ctx context.Context, runID, outcome, errorClass string, finishedAt time.Time, st State) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := q.FinishACMERun(ctx, sqlc.FinishACMERunParams{
		Outcome: outcome, ErrorClass: errorClass, FinishedAt: toNanos(finishedAt), ID: runID,
	}); err != nil {
		return fmt.Errorf("finish the ACME run %q: %w", runID, err)
	}
	if err := q.UpdateACMEState(ctx, stateParams(st)); err != nil {
		return fmt.Errorf("save the ACME state: %w", err)
	}
	if err := q.PruneACMERuns(ctx); err != nil {
		return fmt.Errorf("prune the ACME runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Runs returns up to limit runs, newest first.
func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	return readRuns(ctx, s.st.RQ, limit)
}

func readRuns(ctx context.Context, q *sqlc.Queries, limit int) ([]Run, error) {
	rows, err := q.ListACMERuns(ctx, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list the ACME runs: %w", err)
	}
	runs := make([]Run, 0, len(rows))
	for _, r := range rows {
		var names []string
		if err := json.Unmarshal([]byte(r.Names), &names); err != nil {
			return nil, fmt.Errorf("read the names of the ACME run %q: %w", r.ID, err)
		}
		runs = append(runs, Run{
			ID: r.ID, Kind: r.Kind, Trigger: r.Trigger, Actor: r.Actor, Names: names,
			StartedAt: fromNanos(r.StartedAt), FinishedAt: fromNanos(r.FinishedAt),
			Outcome: r.Outcome, ErrorClass: r.ErrorClass,
		})
	}
	return runs, nil
}

// FailedRunsSince returns when the failed runs at or after since started, oldest first.
func (s *Store) FailedRunsSince(ctx context.Context, since time.Time) ([]time.Time, error) {
	return readFailedRunsSince(ctx, s.st.RQ, since)
}

func readFailedRunsSince(ctx context.Context, q *sqlc.Queries, since time.Time) ([]time.Time, error) {
	rows, err := q.ListACMEFailedRunsSince(ctx, toNanos(since))
	if err != nil {
		return nil, fmt.Errorf("list the failed ACME runs: %w", err)
	}
	starts := make([]time.Time, len(rows))
	for i, n := range rows {
		starts[i] = fromNanos(n)
	}
	return starts, nil
}

// InterruptRunning closes every run still marked running as failed with the class internal. It is for
// startup: a run that is running while no process is running was cut short.
func (s *Store) InterruptRunning(ctx context.Context, now time.Time) error {
	if err := s.st.Q.InterruptACMERuns(ctx, toNanos(now)); err != nil {
		return fmt.Errorf("interrupt the running ACME runs: %w", err)
	}
	return nil
}

// encodeNames is the stored form of a list of names: a JSON array, in order, never null.
func encodeNames(names []string) (string, error) {
	if names == nil {
		names = []string{}
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "", fmt.Errorf("encode the names: %w", err)
	}
	return string(b), nil
}
