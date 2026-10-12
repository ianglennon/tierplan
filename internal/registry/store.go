// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Errors a Store returns. Callers match them with errors.Is.
var (
	// ErrNotFound means no credential has that ID.
	ErrNotFound = errors.New("registry: no such credential")
	// ErrNameTaken means another credential of the same layer (and environment) has the name.
	ErrNameTaken = errors.New("registry: a credential of this layer already has that name")
	// ErrHostPrefixTaken means another credential of the same layer already covers the host and prefix.
	ErrHostPrefixTaken = errors.New("registry: a credential of this layer already covers that host and prefix")
	// ErrHubFallbackTaken means the layer already has its one Docker Hub fallback credential.
	ErrHubFallbackTaken = errors.New("registry: this layer already has a Docker Hub fallback credential")
)

// ValidationError is a submission Validate refused. Nothing was stored.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Code
	}
	return "registry: invalid credential (" + strings.Join(parts, ", ") + ")"
}

// LastTest is the outcome of the most recent test of a credential.
type LastTest struct {
	At     time.Time
	OK     bool
	Detail string
}

// LastFailure is the most recent time a pull or a browse with the credential failed
// to sign in, and the code it failed with.
type LastFailure struct {
	At   time.Time
	Code string
}

// Credential is a stored credential as every reader sees it: every column but the secret,
// which only the resolver ever opens.
type Credential struct {
	ID string
	// EnvID is empty for an instance credential.
	EnvID       string
	Name        string
	Type        string
	Host        string
	Prefix      string
	Username    string
	Last4       string
	HubFallback bool
	CAPEM       string
	Insecure    bool
	ExpiresAt   *time.Time
	LastTest    *LastTest
	LastFailure *LastFailure
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CreatedBy   string
}

// Layer is LayerInstance or LayerEnvironment.
func (c Credential) Layer() string {
	if c.EnvID == "" {
		return LayerInstance
	}
	return LayerEnvironment
}

// TxHook runs inside the transaction that stores a credential, after the insert. An error
// rolls the credential back: the audit row of a create is written here, so a credential
// never exists without it.
type TxHook func(ctx context.Context, tx *sql.Tx, c Credential) error

// Store keeps registry credentials. Secrets are sealed by the vault under the credential's
// own ID, so a sealed value copied into another row does not open.
type Store struct {
	st  *store.Store
	v   *vault.Vault
	now func() time.Time
}

// NewStore returns a Store. now is injected for tests; nil means time.Now.
func NewStore(st *store.Store, v *vault.Vault, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{st: st, v: v, now: now}
}

// Create validates in, seals the secret and stores the credential, then runs hook (which may
// be nil) in the same transaction. actorID is recorded as the creator. It returns a
// *ValidationError for a refused submission and ErrNameTaken, ErrHostPrefixTaken or
// ErrHubFallbackTaken for a clash within the layer. The input is never logged.
func (s *Store) Create(ctx context.Context, in CreateInput, actorID string, hook TxHook) (Credential, error) {
	now := s.now().UTC()
	in, errs := Validate(in, now)
	if len(errs) > 0 {
		return Credential{}, &ValidationError{Fields: errs}
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Credential{}, fmt.Errorf("registry: new credential ID: %w", err)
	}
	id := uid.String()
	sealed, err := s.v.Seal(vault.ContextRegistryCred+id, []byte(in.Secret))
	if err != nil {
		return Credential{}, fmt.Errorf("registry: seal the secret: %w", err)
	}

	params := sqlc.InsertRegistryCredentialParams{
		ID: id, EnvID: nullString(in.EnvID), Name: in.Name, Type: in.Type, Host: in.Host, Prefix: in.Prefix,
		Username: in.Username, SecretSealed: sealed, SecretLast4: Last4(in.Secret), HubFallback: boolInt(in.HubFallback),
		CreatedAt: now.UnixNano(), UpdatedAt: now.UnixNano(), CreatedBy: actorID,
	}
	if in.Expires != "" {
		end, _ := ExpiryEnd(in.Expires) // Validate accepted it
		params.ExpiresAt = sql.NullInt64{Int64: end.UnixNano(), Valid: true}
	}

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("registry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := q.InsertRegistryCredential(ctx, params); err != nil {
		return Credential{}, clash(err)
	}
	row, err := q.GetRegistryCredential(ctx, id)
	if err != nil {
		return Credential{}, fmt.Errorf("registry: read back: %w", err)
	}
	c := credentialOf(row)
	if hook != nil {
		if err := hook(ctx, tx, c); err != nil {
			return Credential{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, fmt.Errorf("registry: commit: %w", err)
	}
	return c, nil
}

// Get returns one credential, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (Credential, error) {
	row, err := s.st.RQ.GetRegistryCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("registry: get: %w", err)
	}
	return credentialOf(row), nil
}

// List returns every credential: the instance ones first, then each environment's, by host and prefix.
func (s *Store) List(ctx context.Context) ([]Credential, error) {
	rows, err := s.st.RQ.ListRegistryCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("registry: list: %w", err)
	}
	out := make([]Credential, len(rows))
	for i, r := range rows {
		out[i] = credentialOf(sqlc.GetRegistryCredentialRow(r))
	}
	return out, nil
}

// clash maps a unique-index failure to the typed conflict the caller words. The message names
// the columns of the index that refused the row.
func clash(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed") {
		return fmt.Errorf("registry: insert: %w", err)
	}
	switch {
	case strings.Contains(msg, "registry_credentials.name"):
		return ErrNameTaken
	case strings.Contains(msg, "registry_credentials.prefix"):
		return ErrHostPrefixTaken
	}
	return ErrHubFallbackTaken
}

func credentialOf(r sqlc.GetRegistryCredentialRow) Credential {
	c := Credential{
		ID: r.ID, EnvID: r.EnvID.String, Name: r.Name, Type: r.Type, Host: r.Host, Prefix: r.Prefix,
		Username: r.Username, Last4: r.SecretLast4, HubFallback: r.HubFallback != 0, CAPEM: r.CaPem, Insecure: r.Insecure != 0,
		CreatedAt: time.Unix(0, r.CreatedAt).UTC(), UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(), CreatedBy: r.CreatedBy,
	}
	if r.ExpiresAt.Valid {
		t := time.Unix(0, r.ExpiresAt.Int64).UTC()
		c.ExpiresAt = &t
	}
	if r.LastTestAt.Valid {
		c.LastTest = &LastTest{At: time.Unix(0, r.LastTestAt.Int64).UTC(), OK: r.LastTestOk.Int64 != 0, Detail: r.LastTestDetail}
	}
	if r.LastFailureAt.Valid {
		c.LastFailure = &LastFailure{At: time.Unix(0, r.LastFailureAt.Int64).UTC(), Code: r.LastFailureCode}
	}
	return c
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
