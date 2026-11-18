// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/vault"
)

// Where a certificate came from.
const (
	// SourceACME is a certificate TierPlan orders from a certificate authority.
	SourceACME = "acme"
	// SourceUpload is a certificate an administrator supplied.
	SourceUpload = "upload"
)

// The phases of a certificate row.
const (
	// PhaseQueued waits for an order to be placed (a rate-limit budget may be the reason).
	PhaseQueued = "queued"
	// PhaseOrdering has an order in flight.
	PhaseOrdering = "ordering"
	// PhaseIdle holds a certificate and waits for its renewal.
	PhaseIdle = "idle"
	// PhaseStopped does nothing until a person acts; stopped_reason says why.
	PhaseStopped = "stopped"
)

// The issuers of a certificate; an upload has none (""). String copies of ingress.Issuer, so a caller
// that only reads rows need not import the ingress package.
const (
	IssuerStaging    = string(ingress.IssuerStaging)
	IssuerProduction = string(ingress.IssuerProduction)
)

// ErrCertNotFound is a certificate id that names no row.
var ErrCertNotFound = errors.New("ingresscert: no such certificate")

// ErrNoKey is a certificate row that holds no private key yet (it has not been issued).
var ErrNoKey = errors.New("ingresscert: the certificate has no key yet")

// AuditTx records an audit entry inside a transaction the caller owns. *audit.Writer satisfies it; a
// nil AuditTx records nothing.
type AuditTx interface {
	RecordTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// StoreDeps are what a Store needs.
type StoreDeps struct {
	St    *store.Store
	Vault *vault.Vault
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Notifier is told the environment's id after every committed change; nil means nobody is told.
	Notifier *ingress.Notifier
	// Audit records the audit rows written inside a certificate write; nil records nothing.
	Audit AuditTx
}

// Store keeps the per-hostname certificates in the database. Private keys are vault envelopes sealed
// under the certificate row's id; they are never written anywhere else.
type Store struct {
	st       *store.Store
	v        *vault.Vault
	now      func() time.Time
	notifier *ingress.Notifier
	audit    AuditTx
}

// NewStore returns a Store.
func NewStore(d StoreDeps) *Store {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Store{st: d.St, v: d.Vault, now: now, notifier: d.Notifier, audit: d.Audit}
}

// Cert is one ingress_certs row. Every instant is the zero time.Time when it is not set. KeySealed is the
// vault envelope of the private key; it is never shown, and OpenKey is the only reader.
type Cert struct {
	ID                  string
	Hostname            string
	EnvID               string
	DomainID            string
	Source              string
	Issuer              string
	RetiredAt           time.Time
	RetiredReason       string
	ChainPEM            []byte
	KeySealed           string
	Serial              string
	NotBefore           time.Time
	NotAfter            time.Time
	IssuedAt            time.Time
	AccountURL          string
	Phase               string
	StoppedReason       string
	QueueCode           string
	QueueUsed           int
	QueueCap            int
	QueueDomain         string
	QueueNextSlotAt     time.Time
	RequestedAt         time.Time
	RequestedBy         string
	UnreferencedSince   time.Time
	ConsecutiveFailures int
	LastAttemptAt       time.Time
	NextAttemptAt       time.Time
	RetryAfterUntil     time.Time
	FailureTimes        []time.Time
	LastErrorAt         time.Time
	LastErrorClass      string
	LastErrorDetail     string
	RenewAt             time.Time
	RenewBasis          string
	AriWindowStart      time.Time
	AriWindowEnd        time.Time
	AriCheckedAt        time.Time
	AriNextCheckAt      time.Time
	Ready               bool
	LastSuccessAt       time.Time
	CreatedBy           string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// CertHook runs inside the transaction of a certificate write, after the write; an error rolls the write
// back. The API writes its audit row there, so a certificate change never exists without it.
type CertHook func(ctx context.Context, tx *sql.Tx, c Cert) error

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
	return time.Unix(0, n).UTC()
}

// certOf converts a row. A failure_times column that is not a JSON array of instants is an error.
func certOf(row sqlc.IngressCert) (Cert, error) {
	var nanos []int64
	if err := json.Unmarshal([]byte(row.FailureTimes), &nanos); err != nil {
		return Cert{}, fmt.Errorf("ingresscert: read the failure times of certificate %q: %w", row.ID, err)
	}
	failures := make([]time.Time, 0, len(nanos))
	for _, n := range nanos {
		failures = append(failures, fromNanos(n))
	}
	return Cert{
		ID: row.ID, Hostname: row.Hostname, EnvID: row.EnvID, DomainID: row.DomainID,
		Source: row.Source, Issuer: row.Issuer,
		RetiredAt: fromNanos(row.RetiredAt), RetiredReason: row.RetiredReason,
		ChainPEM: []byte(row.ChainPem), KeySealed: row.KeySealed, Serial: row.Serial,
		NotBefore: fromNanos(row.NotBefore), NotAfter: fromNanos(row.NotAfter), IssuedAt: fromNanos(row.IssuedAt),
		AccountURL: row.AccountUrl, Phase: row.Phase, StoppedReason: row.StoppedReason,
		QueueCode: row.QueueCode, QueueUsed: int(row.QueueUsed), QueueCap: int(row.QueueCap),
		QueueDomain: row.QueueDomain, QueueNextSlotAt: fromNanos(row.QueueNextSlotAt),
		RequestedAt: fromNanos(row.RequestedAt), RequestedBy: row.RequestedBy,
		UnreferencedSince:   fromNanos(row.UnreferencedSince),
		ConsecutiveFailures: int(row.ConsecutiveFailures),
		LastAttemptAt:       fromNanos(row.LastAttemptAt), NextAttemptAt: fromNanos(row.NextAttemptAt),
		RetryAfterUntil: fromNanos(row.RetryAfterUntil), FailureTimes: failures,
		LastErrorAt: fromNanos(row.LastErrorAt), LastErrorClass: row.LastErrorClass, LastErrorDetail: row.LastErrorDetail,
		RenewAt: fromNanos(row.RenewAt), RenewBasis: row.RenewBasis,
		AriWindowStart: fromNanos(row.AriWindowStart), AriWindowEnd: fromNanos(row.AriWindowEnd),
		AriCheckedAt: fromNanos(row.AriCheckedAt), AriNextCheckAt: fromNanos(row.AriNextCheckAt),
		Ready: row.Ready != 0, LastSuccessAt: fromNanos(row.LastSuccessAt),
		CreatedBy: row.CreatedBy, CreatedAt: fromNanos(row.CreatedAt), UpdatedAt: fromNanos(row.UpdatedAt),
	}, nil
}

// certsOf converts rows.
func certsOf(rows []sqlc.IngressCert) ([]Cert, error) {
	out := make([]Cert, 0, len(rows))
	for _, row := range rows {
		c, err := certOf(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// write runs fn in one write transaction. When fn reports a change, every distinct environment in envIDs has
// its ingress_env.certs_version raised in the same transaction, and nothing else on ingress_env moves: a
// certificate is not a route, so config_version stays where it was (D-31). A write that changes nothing
// raises nothing and tells nobody. fn's error rolls everything back. After the commit of a change the
// notifier, if there is one, is told each environment's id; Notify never blocks.
func (s *Store) write(ctx context.Context, envIDs []string, fn func(tx *sql.Tx, q *sqlc.Queries) (changed bool, err error)) error {
	envs := slices.Clone(envIDs)
	slices.Sort(envs)
	envs = slices.Compact(envs)
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ingresscert: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	changed, err := fn(tx, q)
	if err != nil {
		return err
	}
	if changed {
		now := toNanos(s.now())
		for _, env := range envs {
			if err := q.BumpIngressCertsVersion(ctx, sqlc.BumpIngressCertsVersionParams{EnvID: env, UpdatedAt: now}); err != nil {
				return fmt.Errorf("ingresscert: count the certificate change of environment %q: %w", env, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ingresscert: commit: %w", err)
	}
	if changed {
		for _, env := range envs {
			s.notifier.Notify(env)
		}
	}
	return nil
}

// writeDurable is write followed by a WAL checkpoint, for a write that stores a private key: a key that
// is about to be used must not be lost to a power cut (the acme Store does the same).
func (s *Store) writeDurable(ctx context.Context, envIDs []string, what string, fn func(tx *sql.Tx, q *sqlc.Queries) (changed bool, err error)) error {
	if err := s.write(ctx, envIDs, fn); err != nil {
		return err
	}
	return s.checkpoint(ctx, what)
}

// checkpoint folds the write-ahead log into the database file. The store runs with synchronous(NORMAL),
// under which a committed transaction can still be lost to a power cut until the log is checkpointed.
func (s *Store) checkpoint(ctx context.Context, what string) error {
	var busy, logFrames, checkpointed int
	if err := s.st.Write.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("ingresscert: make %s durable: %w", what, err)
	}
	if busy != 0 {
		return fmt.Errorf("ingresscert: make %s durable: the checkpoint was blocked by another connection", what)
	}
	return nil
}

// PendingInput is a certificate to ask for.
type PendingInput struct {
	Hostname    string
	EnvID       string
	DomainID    string
	Issuer      string
	CreatedBy   string
	RequestedAt time.Time
}

// CreatePending records that hostname wants a certificate: a new live row in phase queued. When the
// hostname already has a live row, that row is returned unchanged with created false and nothing is
// written. hook, which may be nil, runs in the same transaction after a new row is inserted.
func (s *Store) CreatePending(ctx context.Context, in PendingInput, hook CertHook) (c Cert, created bool, err error) {
	host := ingress.NormalizeHost(in.Hostname)
	if host == "" || in.EnvID == "" || in.DomainID == "" {
		return Cert{}, false, errors.New("ingresscert: a certificate needs a hostname, an environment and a domain")
	}
	if in.Issuer != IssuerStaging && in.Issuer != IssuerProduction {
		return Cert{}, false, fmt.Errorf("ingresscert: the issuer %q is neither staging nor production", in.Issuer)
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Cert{}, false, fmt.Errorf("ingresscert: make a certificate id: %w", err)
	}
	now := s.now()
	requested := in.RequestedAt
	if requested.IsZero() {
		requested = now
	}
	err = s.write(ctx, []string{in.EnvID}, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		existing, err := q.GetLiveIngressCertByHostname(ctx, host)
		if err == nil {
			c, err = certOf(existing)
			return false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ingresscert: read the certificate of %q: %w", host, err)
		}
		id := uid.String()
		if err := q.InsertIngressCert(ctx, sqlc.InsertIngressCertParams{
			ID: id, Hostname: host, EnvID: in.EnvID, DomainID: in.DomainID, Source: SourceACME, Issuer: in.Issuer,
			Phase: PhaseQueued, RequestedAt: toNanos(requested), RequestedBy: in.CreatedBy,
			CreatedBy: in.CreatedBy, CreatedAt: toNanos(now), UpdatedAt: toNanos(now),
		}); err != nil {
			return false, fmt.Errorf("ingresscert: insert the certificate of %q: %w", host, err)
		}
		row, err := q.GetIngressCert(ctx, id)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read back the certificate of %q: %w", host, err)
		}
		c, err = certOf(row)
		if err != nil {
			return false, err
		}
		if hook != nil {
			if err := hook(ctx, tx, c); err != nil {
				return false, err
			}
		}
		created = true
		return true, nil
	})
	if err != nil {
		return Cert{}, false, err
	}
	return c, created, nil
}

// Cert returns the certificate row id, ErrCertNotFound when there is none.
func (s *Store) Cert(ctx context.Context, id string) (Cert, error) {
	row, err := s.st.RQ.GetIngressCert(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Cert{}, ErrCertNotFound
	}
	if err != nil {
		return Cert{}, fmt.Errorf("ingresscert: read certificate %q: %w", id, err)
	}
	return certOf(row)
}

// LiveCert returns the live certificate of hostname. found is false, with a nil error, when it has none.
func (s *Store) LiveCert(ctx context.Context, hostname string) (Cert, bool, error) {
	host := ingress.NormalizeHost(hostname)
	row, err := s.st.RQ.GetLiveIngressCertByHostname(ctx, host)
	if errors.Is(err, sql.ErrNoRows) {
		return Cert{}, false, nil
	}
	if err != nil {
		return Cert{}, false, fmt.Errorf("ingresscert: read the certificate of %q: %w", host, err)
	}
	c, err := certOf(row)
	if err != nil {
		return Cert{}, false, err
	}
	return c, true, nil
}

// EnvCerts returns every certificate row of an environment, live and retired, ordered by hostname.
func (s *Store) EnvCerts(ctx context.Context, envID string) ([]Cert, error) {
	rows, err := s.st.RQ.ListIngressCertsByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the certificates of environment %q: %w", envID, err)
	}
	return certsOf(rows)
}

// LiveCerts returns every live certificate of the install, ordered by hostname.
func (s *Store) LiveCerts(ctx context.Context) ([]Cert, error) {
	rows, err := s.st.RQ.ListLiveIngressCerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the live certificates: %w", err)
	}
	return certsOf(rows)
}

// CertsVersion returns the environment's ingress_env.certs_version, 0 when it has no row.
func (s *Store) CertsVersion(ctx context.Context, envID string) (int64, error) {
	v, err := s.st.RQ.GetIngressCertsVersion(ctx, envID)
	if err != nil {
		return 0, fmt.Errorf("ingresscert: read the certificate version of environment %q: %w", envID, err)
	}
	return v, nil
}

// OpenKey opens the certificate's private key. The envelope is bound to the row's id, so a key_sealed
// copied into another row does not open there. A row that has not been issued has no key (ErrNoKey).
func (s *Store) OpenKey(c Cert) (crypto.Signer, error) {
	if c.KeySealed == "" {
		return nil, ErrNoKey
	}
	keyPEM, err := s.v.Open(vault.ContextIngressCertKey+c.ID, c.KeySealed)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: open the key of certificate %q: %w", c.ID, err)
	}
	return decodeKey(keyPEM)
}

// decodeKey reads a private key from its PKCS#8 PEM encoding: ECDSA, RSA or Ed25519, so an upload and an
// ACME key share one path.
func decodeKey(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("ingresscert: no PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: parse the private key: %w", err)
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("ingresscert: the private key cannot sign")
	}
	return signer, nil
}

// leafOf returns the first certificate of a PEM chain.
func leafOf(chainPEM []byte) (*x509.Certificate, error) {
	rest := chainPEM
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("ingresscert: the chain holds no certificate")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("ingresscert: parse the leaf certificate: %w", err)
		}
		return leaf, nil
	}
}

// writeCertificate stores an issued (or uploaded) certificate in row certID through q, inside the caller's
// transaction: the key (PKCS#8 PEM) is sealed under the row's own context, the leaf gives the serial and the
// validity, and the row becomes idle with its queue and failure columns cleared. The key must belong to the
// leaf; a pair that does not match is refused before anything is written.
func (s *Store) writeCertificate(ctx context.Context, q *sqlc.Queries, certID string, chainPEM, keyPKCS8PEM []byte, accountURL string, now time.Time) error {
	leaf, err := leafOf(chainPEM)
	if err != nil {
		return err
	}
	key, err := decodeKey(keyPKCS8PEM)
	if err != nil {
		return err
	}
	pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(leaf.PublicKey) {
		return errors.New("ingresscert: the private key does not belong to the certificate")
	}
	sealed, err := s.v.Seal(vault.ContextIngressCertKey+certID, keyPKCS8PEM)
	if err != nil {
		return fmt.Errorf("ingresscert: seal the key of certificate %q: %w", certID, err)
	}
	ts := toNanos(now)
	if err := q.UpdateIngressCertIssued(ctx, sqlc.UpdateIngressCertIssuedParams{
		ChainPem: string(chainPEM), KeySealed: sealed, Serial: leaf.SerialNumber.Text(16),
		NotBefore: toNanos(leaf.NotBefore), NotAfter: toNanos(leaf.NotAfter), IssuedAt: ts,
		AccountUrl: accountURL, LastSuccessAt: ts, UpdatedAt: ts, ID: certID,
	}); err != nil {
		return fmt.Errorf("ingresscert: store certificate %q: %w", certID, err)
	}
	return nil
}
