// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package gitconn keeps git credentials and the repositories that use them (Phase 9 D-01, D-02),
// modelled on registry credentials: two layers (the instance's and one environment's), a token
// or a private key sealed by the vault under the credential's own ID and never read back, and no
// matching of a credential by address. A repository names the credential it uses explicitly.
package gitconn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Errors a Store returns. Callers match them with errors.Is.
var (
	// ErrNotFound means no credential or repository of that layer has that ID.
	ErrNotFound = errors.New("gitconn: no such credential or repository")
	// ErrNameTaken means another credential (or repository) of the same layer already has the name.
	ErrNameTaken = errors.New("gitconn: another one of this layer already has that name")
	// ErrCredentialInUse means repositories still use the credential.
	ErrCredentialInUse = errors.New("gitconn: repositories use the credential")
)

// InUseError says how many repositories use a credential. It matches ErrCredentialInUse.
type InUseError struct{ Count int }

func (e *InUseError) Error() string { return ErrCredentialInUse.Error() }

// Unwrap lets errors.Is match the sentinel.
func (e *InUseError) Unwrap() error { return ErrCredentialInUse }

// LastFailure is the most recent time a call with the credential was refused, and the code.
type LastFailure struct {
	At   time.Time
	Code string
}

// Credential is a stored credential as every reader sees it: every column but the secret, which
// only Remote ever opens.
type Credential struct {
	ID string
	// EnvID is empty for an instance credential.
	EnvID    string
	Name     string
	Type     string
	Username string
	// Last4 is the last characters of an HTTPS token (empty for a short one and for a key).
	Last4 string
	// PublicKey and Fingerprint are what is shown of an SSH key: the authorized_keys line and
	// SHA256:... .
	PublicKey   string
	Fingerprint string
	// UsedBy is how many repositories use it.
	UsedBy      int
	LastUseAt   *time.Time
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

// LastTest is the outcome of the most recent test of a saved repository.
type LastTest struct {
	At   time.Time
	OK   bool
	Code string
}

// Repo is a stored repository connection.
type Repo struct {
	ID string
	// EnvID is empty for an instance repository.
	EnvID string
	Name  string
	URL   string
	// CredentialID is the credential picked for it; empty for a public repository.
	CredentialID string
	// CredentialName and CredentialEnvID describe that credential (the environment empty for an
	// instance credential).
	CredentialName  string
	CredentialEnvID string
	// CAPEM is a custom certificate authority, public material.
	CAPEM string
	// HostKeyFP, HostKeyTrustedBy and HostKeyTrustedAt describe the SSH host key a person trusted.
	HostKeyFP        string
	HostKeyTrustedBy string
	HostKeyTrustedAt *time.Time
	LastTest         *LastTest
	// UsedBy is how many stacks use it. Stacks cannot point at a repository yet.
	UsedBy    int
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
}

// Layer is LayerInstance or LayerEnvironment.
func (r Repo) Layer() string {
	if r.EnvID == "" {
		return LayerInstance
	}
	return LayerEnvironment
}

// Host is the host of the repository's address, or empty when it does not parse.
func (r Repo) Host() string {
	u, err := gitx.ValidateURL(r.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// TxHook runs inside the transaction that stores a credential, after the write. An error rolls the
// write back: the audit row of a change is written here, so a change never exists without it.
type TxHook func(ctx context.Context, tx *sql.Tx, c Credential) error

// RepoHook is TxHook for a repository.
type RepoHook func(ctx context.Context, tx *sql.Tx, r Repo) error

// Store keeps credentials and repositories. Secrets are sealed by the vault under the
// credential's own ID, so a sealed value copied into another row does not open.
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

// CreateCredential validates in, seals the secret and stores the credential, then runs hook (which
// may be nil) in the same transaction, so the sealed secret exists exactly when its row does and an
// interrupted create leaves nothing. actorID is recorded as the creator. It returns a
// *ValidationError for a refused submission and ErrNameTaken for a name the layer already has. The
// input is never logged.
func (s *Store) CreateCredential(ctx context.Context, in CredentialInput, actorID string, hook TxHook) (Credential, error) {
	clean, errs := validCredential(in, true)
	if len(errs) > 0 {
		return Credential{}, &ValidationError{Fields: errs}
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: new credential ID: %w", err)
	}
	id := uid.String()
	sealed, err := s.v.Seal(vault.ContextGitCred+id, []byte(clean.Secret))
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: seal the secret: %w", err)
	}
	now := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := q.InsertGitCredential(ctx, sqlc.InsertGitCredentialParams{
		ID: id, EnvID: nullString(deref(in.EnvID)), Name: clean.Name, Type: clean.Type, Username: clean.Username,
		SecretSealed: sealed, SecretLast4: clean.last4, PublicKey: clean.publicKey, Fingerprint: clean.fingerprint,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actorID,
	}); err != nil {
		return Credential{}, writeError(err)
	}
	row, err := q.GetGitCredential(ctx, id)
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: read back: %w", err)
	}
	c := credentialOf(row)
	if hook != nil {
		if err := hook(ctx, tx, c); err != nil {
			return Credential{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, fmt.Errorf("gitconn: commit: %w", err)
	}
	return c, nil
}

// GetCredential returns one credential of any layer, or ErrNotFound.
func (s *Store) GetCredential(ctx context.Context, id string) (Credential, error) {
	row, err := s.st.RQ.GetGitCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: get credential: %w", err)
	}
	return credentialOf(row), nil
}

// RepoInput is what a person types to add a repository.
type RepoInput struct {
	// EnvID is nil for an instance repository.
	EnvID *string
	Name  string
	URL   string
	// CredentialID is the credential picked for it; nil or empty for a public repository.
	CredentialID *string
	// CAPEM is an optional custom certificate authority for a server with its own.
	CAPEM string
}

// CreateRepo validates in and stores the repository, then runs hook (which may be nil) in the same
// transaction. A credential must be picked by ID: it is read in the same transaction and checked
// against the layer rule (CheckCredentialScope), so a credential that is not the repository's to
// use is a *ValidationError (credentialId: credential_scope). It also returns *ValidationError for
// a refused address, CA or name, and ErrNameTaken for a name the layer already has.
func (s *Store) CreateRepo(ctx context.Context, in RepoInput, actorID string, hook RepoHook) (Repo, error) {
	name, url, ca, _, errs := validRepo(in.Name, in.URL, in.CAPEM)
	if len(errs) > 0 {
		return Repo{}, &ValidationError{Fields: errs}
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: new repository ID: %w", err)
	}
	id := uid.String()
	now := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	cred, err := s.pickedCredential(ctx, q, deref(in.EnvID), deref(in.CredentialID))
	if err != nil {
		return Repo{}, err
	}
	if err := q.InsertGitRepo(ctx, sqlc.InsertGitRepoParams{
		ID: id, EnvID: nullString(deref(in.EnvID)), Name: name, Url: url, CredentialID: nullString(cred),
		CaPem: ca, CreatedAt: now, UpdatedAt: now, CreatedBy: actorID,
	}); err != nil {
		return Repo{}, writeError(err)
	}
	row, err := q.GetGitRepo(ctx, id)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: read back: %w", err)
	}
	r := repoOf(row)
	if hook != nil {
		if err := hook(ctx, tx, r); err != nil {
			return Repo{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Repo{}, fmt.Errorf("gitconn: commit: %w", err)
	}
	return r, nil
}

// pickedCredential reads the credential a repository of repoEnv names, in the transaction of q, and
// applies the layer rule. It returns the credential's ID, or "" for none. A credential that does
// not exist is answered exactly like one the repository may not use, so the answer says nothing
// about another environment's credentials.
func (s *Store) pickedCredential(ctx context.Context, q *sqlc.Queries, repoEnv, credID string) (string, error) {
	if credID == "" {
		return "", nil
	}
	row, err := q.GetGitCredential(ctx, credID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("credentialId", CodeCredentialScope)
	}
	if err != nil {
		return "", fmt.Errorf("gitconn: read the picked credential: %w", err)
	}
	if err := CheckCredentialScope(repoEnv, credentialOf(row)); err != nil {
		return "", err
	}
	return credID, nil
}

// GetRepo returns one repository of any layer, or ErrNotFound.
func (s *Store) GetRepo(ctx context.Context, id string) (Repo, error) {
	row, err := s.st.RQ.GetGitRepo(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: get repository: %w", err)
	}
	return repoOf(row), nil
}

// Remote is what gitx needs to talk to the repository: its address, its certificate authority and,
// when it has a credential, the sign-in. The token is opened here, in memory, for this one call; it
// is in no field of the Repo and no route returns it. SSH keys are used by the SSH transport and
// give no Auth here yet.
func (s *Store) Remote(ctx context.Context, r Repo) (gitx.Remote, error) {
	if r.CredentialID == "" {
		return gitx.Remote{URL: r.URL, CAPEM: r.CAPEM}, nil
	}
	c, err := s.GetCredential(ctx, r.CredentialID)
	if err != nil {
		return gitx.Remote{}, err
	}
	return s.remote(ctx, r.URL, r.CAPEM, c)
}

// RemoteFor is Remote for an address that is not saved, with a credential picked by ID for a
// repository of repoEnv (empty for the instance layer). The credential must be one the layer rule
// allows that repository (CheckCredentialScope), else *ValidationError (credentialId:
// credential_scope), exactly as when the repository is saved. It returns the credential, for the
// audit row of the test that uses it.
func (s *Store) RemoteFor(ctx context.Context, repoEnv, url, caPEM, credID string) (gitx.Remote, Credential, error) {
	c, err := s.GetCredential(ctx, credID)
	if errors.Is(err, ErrNotFound) {
		return gitx.Remote{}, Credential{}, invalid("credentialId", CodeCredentialScope)
	}
	if err != nil {
		return gitx.Remote{}, Credential{}, err
	}
	if err := CheckCredentialScope(repoEnv, c); err != nil {
		return gitx.Remote{}, Credential{}, err
	}
	rem, err := s.remote(ctx, url, caPEM, c)
	return rem, c, err
}

// remote opens the secret of c, when it is a token, into the Remote of one call.
func (s *Store) remote(ctx context.Context, url, caPEM string, c Credential) (gitx.Remote, error) {
	rem := gitx.Remote{URL: url, CAPEM: caPEM}
	if c.Type != TypeHTTPS {
		return rem, nil
	}
	sealed, err := s.st.RQ.GetGitCredentialSecret(ctx, c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return gitx.Remote{}, ErrNotFound
	}
	if err != nil {
		return gitx.Remote{}, fmt.Errorf("gitconn: read the secret of credential %s: %w", c.ID, err)
	}
	plain, err := s.v.Open(vault.ContextGitCred+c.ID, sealed)
	if err != nil {
		return gitx.Remote{}, fmt.Errorf("gitconn: open the secret of credential %s: %w", c.ID, err)
	}
	rem.Auth = gitx.TokenAuth{Username: c.Username, Token: string(plain)}
	return rem, nil
}

// RecordTest remembers the outcome of a test of the saved repository, now: whether it passed and,
// when it did not, the code it failed with (empty otherwise). It also records what the test showed
// of the credential the repository uses: a call that signed in clears an earlier refusal and a
// refusal is remembered. A repository deleted meanwhile is not an error.
func (s *Store) RecordTest(ctx context.Context, repoID string, ok bool, code gitx.Code) error {
	at := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := q.GetGitRepo(ctx, repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gitconn: read the repository to record a test: %w", err)
	}
	if _, err := q.SetGitRepoTest(ctx, sqlc.SetGitRepoTestParams{
		LastTestAt: sql.NullInt64{Int64: at, Valid: true}, LastTestOk: sql.NullInt64{Int64: boolInt(ok), Valid: true},
		LastTestCode: string(code), ID: repoID,
	}); err != nil {
		return fmt.Errorf("gitconn: record the test of repository %s: %w", repoID, err)
	}
	if row.CredentialID.Valid {
		switch {
		case ok:
			_, err = q.SetGitCredentialUse(ctx, sqlc.SetGitCredentialUseParams{
				LastUseAt: sql.NullInt64{Int64: at, Valid: true}, ID: row.CredentialID.String,
			})
		case code == gitx.CodeAuthRefused:
			_, err = q.SetGitCredentialFailure(ctx, sqlc.SetGitCredentialFailureParams{
				LastFailureAt: sql.NullInt64{Int64: at, Valid: true}, LastFailureCode: string(code), ID: row.CredentialID.String,
			})
		}
		if err != nil {
			return fmt.Errorf("gitconn: record the use of credential %s: %w", row.CredentialID.String, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gitconn: commit: %w", err)
	}
	return nil
}

// writeError maps a unique-index failure to ErrNameTaken: the only unique rule besides the ID is
// the name within a layer.
func writeError(err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrNameTaken
	}
	return fmt.Errorf("gitconn: write: %w", err)
}

func credentialOf(r sqlc.GetGitCredentialRow) Credential {
	c := Credential{
		ID: r.ID, EnvID: r.EnvID.String, Name: r.Name, Type: r.Type, Username: r.Username, Last4: r.SecretLast4,
		PublicKey: r.PublicKey, Fingerprint: r.Fingerprint, UsedBy: int(r.UsedBy),
		CreatedAt: time.Unix(0, r.CreatedAt).UTC(), UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(), CreatedBy: r.CreatedBy,
	}
	if r.LastUseAt.Valid {
		t := time.Unix(0, r.LastUseAt.Int64).UTC()
		c.LastUseAt = &t
	}
	if r.LastFailureAt.Valid {
		c.LastFailure = &LastFailure{At: time.Unix(0, r.LastFailureAt.Int64).UTC(), Code: r.LastFailureCode}
	}
	return c
}

func repoOf(r sqlc.GetGitRepoRow) Repo {
	out := Repo{
		ID: r.ID, EnvID: r.EnvID.String, Name: r.Name, URL: r.Url, CredentialID: r.CredentialID.String,
		CredentialName: r.CredentialName, CredentialEnvID: r.CredentialEnvID.String, CAPEM: r.CaPem,
		HostKeyFP: r.HostKeyFp, HostKeyTrustedBy: r.HostKeyTrustedBy,
		CreatedAt: time.Unix(0, r.CreatedAt).UTC(), UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(), CreatedBy: r.CreatedBy,
	}
	if r.HostKeyTrustedAt.Valid {
		t := time.Unix(0, r.HostKeyTrustedAt.Int64).UTC()
		out.HostKeyTrustedAt = &t
	}
	if r.LastTestAt.Valid {
		out.LastTest = &LastTest{At: time.Unix(0, r.LastTestAt.Int64).UTC(), OK: r.LastTestOk.Int64 != 0, Code: r.LastTestCode}
	}
	return out
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
