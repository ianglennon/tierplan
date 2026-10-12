// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// ConflictError is a clash within the layer, carrying the name of the other credential that
// holds the name, the host and prefix or the Hub fallback, so the page can say which one.
// It matches ErrNameTaken, ErrHostPrefixTaken or ErrHubFallbackTaken with errors.Is.
type ConflictError struct {
	// Kind is the sentinel that says what clashed.
	Kind error
	// With is the other credential's name; empty when it could not be read.
	With string
}

func (e *ConflictError) Error() string { return e.Kind.Error() }

// Unwrap lets errors.Is match the sentinel.
func (e *ConflictError) Unwrap() error { return e.Kind }

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
		return Credential{}, clash(ctx, q, err, id, in.EnvID, in.Name, in.Host, in.Prefix)
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

// UpdateHook runs inside the transaction that stores a change, after the update, with the
// credential as it now is and the names of the fields that changed. An error rolls the change
// back: the audit row of an update is written here, so a change never exists without it.
type UpdateHook func(ctx context.Context, tx *sql.Tx, c Credential, changed []string) error

// Update validates in, applies it to the credential and runs hook (which may be nil) in the
// same transaction. An empty Secret keeps the stored one: its envelope is not read or touched.
// A new one is sealed again under the credential's own ID and its last characters replaced.
// It returns the credential as it now is and the names of the fields that changed, sorted
// (expires, host, hub_fallback, name, prefix, secret, type, username); the secret is only ever
// the word secret. A form that changes nothing writes nothing, runs no hook and returns no
// changed fields. ErrNotFound for an unknown ID, *ValidationError for a refused change and a
// *ConflictError for a clash within the layer. The input is never logged.
func (s *Store) Update(ctx context.Context, id string, in UpdateInput, hook UpdateHook) (Credential, []string, error) {
	return s.update(ctx, nil, id, in, hook)
}

// UpdateInEnv is Update for a credential of one environment: the same rules, and the same
// ErrNotFound for a credential that is not of that environment (another environment's, or the
// instance layer's) as for one that does not exist. Nothing is read back, changed or hooked then.
func (s *Store) UpdateInEnv(ctx context.Context, envID, id string, in UpdateInput, hook UpdateHook) (Credential, []string, error) {
	if envID == "" {
		return Credential{}, nil, ErrNotFound
	}
	return s.update(ctx, &envID, id, in, hook)
}

// update is Update and UpdateInEnv: env is nil for any layer, or the one environment whose
// credential this is allowed to change.
func (s *Store) update(ctx context.Context, env *string, id string, in UpdateInput, hook UpdateHook) (Credential, []string, error) {
	now := s.now().UTC()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("registry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetRegistryCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, nil, ErrNotFound
	}
	if err != nil {
		return Credential{}, nil, fmt.Errorf("registry: read: %w", err)
	}
	cur := credentialOf(row)
	if env != nil && cur.EnvID != *env {
		return Credential{}, nil, ErrNotFound
	}
	prevExpires := ""
	if cur.ExpiresAt != nil {
		prevExpires = cur.ExpiresAt.UTC().Format(time.DateOnly)
	}
	in, errs := ValidateUpdate(in, prevExpires, now)
	if len(errs) > 0 {
		return Credential{}, nil, &ValidationError{Fields: errs}
	}

	var changed []string
	for _, f := range []struct {
		name string
		diff bool
	}{
		{"expires", in.Expires != prevExpires},
		{"host", in.Host != cur.Host},
		{"hub_fallback", in.HubFallback != cur.HubFallback},
		{"name", in.Name != cur.Name},
		{"prefix", in.Prefix != cur.Prefix},
		{"secret", in.Secret != ""},
		{"type", in.Type != cur.Type},
		{"username", in.Username != cur.Username},
	} {
		if f.diff {
			changed = append(changed, f.name)
		}
	}
	if len(changed) == 0 {
		return cur, nil, nil
	}

	params := sqlc.UpdateRegistryCredentialParams{
		ID: id, Name: in.Name, Type: in.Type, Host: in.Host, Prefix: in.Prefix, Username: in.Username,
		HubFallback: boolInt(in.HubFallback), UpdatedAt: now.UnixNano(),
	}
	if in.Expires != "" {
		end, _ := ExpiryEnd(in.Expires) // ValidateUpdate accepted it
		params.ExpiresAt = sql.NullInt64{Int64: end.UnixNano(), Valid: true}
	}
	var uerr error
	if env != nil {
		// The statement is constrained to the environment too, so the check above is not the
		// only thing keeping another layer's credential out of reach.
		var n int64
		n, uerr = q.UpdateRegistryCredentialInEnv(ctx, sqlc.UpdateRegistryCredentialInEnvParams{
			Name: params.Name, Type: params.Type, Host: params.Host, Prefix: params.Prefix, Username: params.Username,
			HubFallback: params.HubFallback, ExpiresAt: params.ExpiresAt, UpdatedAt: params.UpdatedAt,
			ID: id, EnvID: nullString(*env),
		})
		if uerr == nil && n == 0 {
			return Credential{}, nil, ErrNotFound
		}
	} else {
		_, uerr = q.UpdateRegistryCredential(ctx, params)
	}
	if uerr != nil {
		return Credential{}, nil, clash(ctx, q, uerr, id, cur.EnvID, in.Name, in.Host, in.Prefix)
	}
	if slices.Contains(changed, "secret") {
		sealed, err := s.v.Seal(vault.ContextRegistryCred+id, []byte(in.Secret))
		if err != nil {
			return Credential{}, nil, fmt.Errorf("registry: seal the secret: %w", err)
		}
		if _, err := q.UpdateRegistryCredentialSecret(ctx, sqlc.UpdateRegistryCredentialSecretParams{
			ID: id, SecretSealed: sealed, SecretLast4: Last4(in.Secret), UpdatedAt: now.UnixNano(),
		}); err != nil {
			return Credential{}, nil, fmt.Errorf("registry: store the secret: %w", err)
		}
	}
	row, err = q.GetRegistryCredential(ctx, id)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("registry: read back: %w", err)
	}
	c := credentialOf(row)
	if hook != nil {
		if err := hook(ctx, tx, c, changed); err != nil {
			return Credential{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, nil, fmt.Errorf("registry: commit: %w", err)
	}
	return c, changed, nil
}

// Delete removes a credential, running hook (which may be nil) in the same transaction with
// the credential as it was, so the audit row and the removal commit or roll back together.
// ErrNotFound when there is no such credential, which makes a repeated delete safe. Pulls
// the credential would have matched fall back to the next matching one, or to anonymous:
// nothing else refers to a credential.
func (s *Store) Delete(ctx context.Context, id string, hook TxHook) (Credential, error) {
	return s.delete(ctx, nil, id, hook)
}

// DeleteInEnv is Delete for a credential of one environment: a credential that is not of that
// environment is ErrNotFound, exactly like one that does not exist, and is left alone.
func (s *Store) DeleteInEnv(ctx context.Context, envID, id string, hook TxHook) (Credential, error) {
	if envID == "" {
		return Credential{}, ErrNotFound
	}
	return s.delete(ctx, &envID, id, hook)
}

// delete is Delete and DeleteInEnv: env is nil for any layer, or the one environment whose
// credential this is allowed to remove.
func (s *Store) delete(ctx context.Context, env *string, id string, hook TxHook) (Credential, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("registry: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetRegistryCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("registry: read: %w", err)
	}
	c := credentialOf(row)
	if env != nil && c.EnvID != *env {
		return Credential{}, ErrNotFound
	}
	var derr error
	if env != nil {
		_, derr = q.DeleteRegistryCredentialInEnv(ctx, sqlc.DeleteRegistryCredentialInEnvParams{ID: id, EnvID: nullString(*env)})
	} else {
		_, derr = q.DeleteRegistryCredential(ctx, id)
	}
	if errors.Is(derr, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	} else if derr != nil {
		return Credential{}, fmt.Errorf("registry: delete: %w", derr)
	}
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

// GetInEnv returns one credential of one environment. A credential of another environment, or of
// the instance layer, is ErrNotFound exactly like one that does not exist, so a route of one
// environment never reaches a credential that is not its own.
func (s *Store) GetInEnv(ctx context.Context, envID, id string) (Credential, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	if envID == "" || c.EnvID != envID {
		return Credential{}, ErrNotFound
	}
	return c, nil
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

// Candidates returns what the resolver chooses from for one environment: the instance
// credentials and that environment's own, by host and prefix. They carry no secret, and no
// other environment's credential is ever among them.
func (s *Store) Candidates(ctx context.Context, envID string) ([]Candidate, error) {
	rows, err := s.st.RQ.ListRegistryCandidates(ctx, sql.NullString{String: envID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("registry: candidates: %w", err)
	}
	out := make([]Candidate, len(rows))
	for i, r := range rows {
		out[i] = Candidate{
			ID: r.ID, Name: r.Name, Layer: LayerInstance, Host: r.Host, Prefix: r.Prefix, HubFallback: r.HubFallback != 0,
		}
		if r.EnvID.Valid {
			out[i].Layer, out[i].EnvID, out[i].EnvName = LayerEnvironment, r.EnvID.String, r.EnvName
		}
	}
	return out, nil
}

// openSecret opens the secret of one credential under its own ID. It is read for one pull and
// never kept: the caller holds the plaintext only as long as it takes to encode the header.
// The error names the credential and never a value.
func (s *Store) openSecret(ctx context.Context, id string) ([]byte, error) {
	sealed, err := s.st.RQ.GetRegistryCredentialSecret(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("registry: read the secret of credential %s: %w", id, err)
	}
	plain, err := s.v.Open(vault.ContextRegistryCred+id, sealed)
	if err != nil {
		return nil, fmt.Errorf("registry: open the secret of credential %s: %w", id, err)
	}
	return plain, nil
}

// Secret opens the stored secret of a credential for one test of it, and returns it as a
// string the caller hands straight to a registry client and drops. It is the only way the
// secret leaves the store besides the pull header, and no route returns it.
func (s *Store) Secret(ctx context.Context, id string) (string, error) {
	plain, err := s.openSecret(ctx, id)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// RecordTest remembers the outcome of a test of the saved values, now: whether it passed
// and, when it did not, the code of the first step that failed. A test that passed also
// clears the last sign-in failure, because the credential has just been seen to work. A
// credential deleted meanwhile is not an error.
func (s *Store) RecordTest(ctx context.Context, id string, ok bool, detail string) error {
	_, err := s.st.Q.SetRegistryCredentialTest(ctx, sqlc.SetRegistryCredentialTestParams{
		At: sql.NullInt64{Int64: s.now().UnixNano(), Valid: true}, Ok: sql.NullInt64{Int64: boolInt(ok), Valid: true},
		Detail: detail, ID: id,
	})
	if err != nil {
		return fmt.Errorf("registry: record the test of credential %s: %w", id, err)
	}
	return nil
}

// RecordFailure remembers that the registry refused to sign in with the credential, now, with
// the code the pull failed with (D-07). A credential deleted meanwhile is not an error.
func (s *Store) RecordFailure(ctx context.Context, id, code string) error {
	_, err := s.st.Q.SetRegistryCredentialFailure(ctx, sqlc.SetRegistryCredentialFailureParams{
		LastFailureAt: sql.NullInt64{Int64: s.now().UnixNano(), Valid: true}, LastFailureCode: code, ID: id,
	})
	if err != nil {
		return fmt.Errorf("registry: record the failure of credential %s: %w", id, err)
	}
	return nil
}

// ClearFailure forgets the failure of a credential after a pull with it worked. It writes
// nothing when there is none.
func (s *Store) ClearFailure(ctx context.Context, id string) error {
	if _, err := s.st.Q.ClearRegistryCredentialFailure(ctx, id); err != nil {
		return fmt.Errorf("registry: clear the failure of credential %s: %w", id, err)
	}
	return nil
}

// clash maps a unique-index failure to the typed conflict the caller words, naming the other
// credential that holds what was asked for. The message names the columns of the index that
// refused the row. self is the credential being written, which is never its own conflict.
func clash(ctx context.Context, q *sqlc.Queries, err error, self, envID, name, host, prefix string) error {
	msg := err.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed") {
		return fmt.Errorf("registry: write: %w", err)
	}
	var kind error
	var how string
	switch {
	case strings.Contains(msg, "registry_credentials.name"):
		kind, how = ErrNameTaken, "name"
	case strings.Contains(msg, "registry_credentials.prefix"):
		kind, how = ErrHostPrefixTaken, "prefix"
	default:
		kind, how = ErrHubFallbackTaken, "hub"
	}
	with, qerr := q.FindRegistryCredentialClash(ctx, sqlc.FindRegistryCredentialClashParams{
		EnvID: nullString(envID), ID: self, Kind: how, Name: name, Host: host, Prefix: prefix,
	})
	if qerr != nil {
		return &ConflictError{Kind: kind}
	}
	return &ConflictError{Kind: kind, With: with}
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
