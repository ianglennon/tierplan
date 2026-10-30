// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/vault"
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

// ErrRepoInUse means stacks still use the repository.
var ErrRepoInUse = errors.New("gitconn: stacks use the repository")

// RepoInUseError says how many stacks use a repository. It matches ErrRepoInUse.
type RepoInUseError struct{ Count int }

func (e *RepoInUseError) Error() string { return ErrRepoInUse.Error() }

// Unwrap lets errors.Is match the sentinel.
func (e *RepoInUseError) Unwrap() error { return ErrRepoInUse }

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
	// HostKey is the SSH host key a person trusted for the repository's host, in authorized_keys
	// form (public material); empty when none was.
	HostKey string
	// HostKeyFP, HostKeyTrustedBy and HostKeyTrustedAt describe that key: its SHA256 fingerprint,
	// the user who trusted it and when.
	HostKeyFP        string
	HostKeyTrustedBy string
	HostKeyTrustedAt *time.Time
	LastTest         *LastTest
	// UsedBy is how many stacks use it.
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
	// HostKey is the SSH host key the person confirmed, in authorized_keys form; empty for none.
	// The creator is recorded as who trusted it. 422 hostKey: host_key_invalid or host_key_not_ssh.
	HostKey string
}

// CreateRepo validates in and stores the repository, then runs hook (which may be nil) in the same
// transaction. A credential must be picked by ID: it is read in the same transaction and checked
// against the layer rule (CheckCredentialScope), so a credential that is not the repository's to
// use is a *ValidationError (credentialId: credential_scope). It also returns *ValidationError for
// a refused address, CA or name, and ErrNameTaken for a name the layer already has.
func (s *Store) CreateRepo(ctx context.Context, in RepoInput, actorID string, hook RepoHook) (Repo, error) {
	name, url, ca, _, errs := validRepo(in.Name, in.URL, in.CAPEM)
	key, keyErr := validHostKey(in.HostKey, url)
	if keyErr != nil {
		errs = append(errs, *keyErr)
	}
	if len(errs) > 0 {
		return Repo{}, &ValidationError{Fields: errs}
	}
	uid, err := uuid.NewV7()
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: new repository ID: %w", err)
	}
	id := uid.String()
	now := s.now().UTC().UnixNano()
	var trustedBy string
	var trustedAt sql.NullInt64
	if key.Line != "" {
		trustedBy, trustedAt = actorID, sql.NullInt64{Int64: now, Valid: true}
	}
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
		CaPem: ca, HostKey: key.Line, HostKeyFp: key.Fingerprint, HostKeyTrustedBy: trustedBy, HostKeyTrustedAt: trustedAt,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actorID,
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

// Remote is what gitx needs to talk to the repository: its address, its certificate authority, the
// SSH host key a person trusted and, when it has a credential, the sign-in. A token or a private
// key is opened here, in memory, for this one call; it is in no field of the Repo and no route
// returns it.
func (s *Store) Remote(ctx context.Context, r Repo) (gitx.Remote, error) {
	rem := gitx.Remote{URL: r.URL, CAPEM: r.CAPEM}
	if r.HostKey != "" {
		rem.HostKeys = []gitx.HostKey{{Line: r.HostKey}}
	}
	if r.CredentialID == "" {
		return rem, nil
	}
	c, err := s.GetCredential(ctx, r.CredentialID)
	if err != nil {
		return gitx.Remote{}, err
	}
	auth, err := s.auth(ctx, c)
	if err != nil {
		return gitx.Remote{}, err
	}
	rem.Auth = auth
	return rem, nil
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
	auth, err := s.auth(ctx, c)
	if err != nil {
		return gitx.Remote{}, Credential{}, err
	}
	return gitx.Remote{URL: url, CAPEM: caPEM, Auth: auth}, c, nil
}

// auth opens the secret of c into the sign-in of one call: a token with its user name, or an SSH
// key as a signer.
func (s *Store) auth(ctx context.Context, c Credential) (gitx.Auth, error) {
	sealed, err := s.st.RQ.GetGitCredentialSecret(ctx, c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("gitconn: read the secret of credential %s: %w", c.ID, err)
	}
	plain, err := s.v.Open(vault.ContextGitCred+c.ID, sealed)
	if err != nil {
		return nil, fmt.Errorf("gitconn: open the secret of credential %s: %w", c.ID, err)
	}
	if c.Type != TypeSSH {
		return gitx.TokenAuth{Username: c.Username, Token: string(plain)}, nil
	}
	key, err := ParsePrivateKey(string(plain))
	if err != nil {
		return nil, fmt.Errorf("gitconn: read the key of credential %s: %w", c.ID, err)
	}
	return gitx.SSHKeyAuth{User: c.Username, Signer: key.Signer}, nil
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

// RecordUse remembers what a call that used the credential showed of it, now: code empty is a call
// that signed in, which clears an earlier refusal; auth_refused is a refusal and is remembered. Any
// other failure says nothing about the credential and is not recorded. An empty credentialID (a
// public repository) and a credential deleted meanwhile are not errors.
func (s *Store) RecordUse(ctx context.Context, credentialID string, code gitx.Code) error {
	if credentialID == "" || (code != "" && code != gitx.CodeAuthRefused) {
		return nil
	}
	at := sql.NullInt64{Int64: s.now().UTC().UnixNano(), Valid: true}
	var err error
	if code == "" {
		_, err = s.st.Q.SetGitCredentialUse(ctx, sqlc.SetGitCredentialUseParams{LastUseAt: at, ID: credentialID})
	} else {
		_, err = s.st.Q.SetGitCredentialFailure(ctx, sqlc.SetGitCredentialFailureParams{LastFailureAt: at, LastFailureCode: string(code), ID: credentialID})
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("gitconn: record the use of credential %s: %w", credentialID, err)
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
		HostKey: r.HostKey, HostKeyFP: r.HostKeyFp, HostKeyTrustedBy: r.HostKeyTrustedBy,
		UsedBy:    int(r.UsedBy),
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

// CredentialUpdate is what a person types to change a credential: everything but the layer, which
// a credential never leaves. Secret empty means keep the saved one.
type CredentialUpdate struct {
	Name, Type, Username, Secret string
}

// UpdateHook runs inside the transaction that stores a change to a credential, after the write, with
// the credential as it now is and the names of the fields that changed. An error rolls the change
// back: the audit row of an update is written here, so a change never exists without it.
type UpdateHook func(ctx context.Context, tx *sql.Tx, c Credential, changed []string) error

// UpdateCredential changes a credential of one layer (envID empty for the instance's); a credential
// of any other layer is ErrNotFound and nothing is read back, changed or hooked. An empty Secret
// keeps the saved one: its envelope is not read or touched. Switching the type needs a secret typed
// now (*ValidationError, secret: secret_required), so a token is never kept as a key. A new Secret
// is sealed again under the credential's own ID and what is shown of it replaced. It returns the
// credential as it now is and the names of the fields that changed, sorted (name, secret, type,
// username); the secret is only ever the word secret. A form that changes nothing writes nothing,
// runs no hook and returns no changed fields. A clash within the layer is ErrNameTaken. The input
// is never logged.
func (s *Store) UpdateCredential(ctx context.Context, envID, id string, in CredentialUpdate, hook UpdateHook) (Credential, []string, error) {
	now := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetGitCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, nil, ErrNotFound
	}
	if err != nil {
		return Credential{}, nil, fmt.Errorf("gitconn: read: %w", err)
	}
	cur := credentialOf(row)
	if cur.EnvID != envID {
		return Credential{}, nil, ErrNotFound
	}
	clean, errs := validCredential(CredentialInput{Name: in.Name, Type: in.Type, Username: in.Username, Secret: in.Secret}, false)
	if len(errs) == 0 && clean.Secret == "" && clean.Type != cur.Type {
		errs = append(errs, FieldError{"secret", CodeSecretRequired})
	}
	if len(errs) > 0 {
		return Credential{}, nil, &ValidationError{Fields: errs}
	}

	var changed []string
	for _, f := range []struct {
		name string
		diff bool
	}{
		{"name", clean.Name != cur.Name},
		{"secret", clean.Secret != ""},
		{"type", clean.Type != cur.Type},
		{"username", clean.Username != cur.Username},
	} {
		if f.diff {
			changed = append(changed, f.name)
		}
	}
	if len(changed) == 0 {
		return cur, nil, nil
	}

	var n int64
	if envID == "" {
		n, err = q.UpdateGitCredentialInstance(ctx, sqlc.UpdateGitCredentialInstanceParams{
			Name: clean.Name, Type: clean.Type, Username: clean.Username, UpdatedAt: now, ID: id,
		})
	} else {
		// The statement is bounded to the environment too, so the check above is not the only thing
		// keeping another layer's credential out of reach.
		n, err = q.UpdateGitCredentialInEnv(ctx, sqlc.UpdateGitCredentialInEnvParams{
			Name: clean.Name, Type: clean.Type, Username: clean.Username, UpdatedAt: now, ID: id, EnvID: nullString(envID),
		})
	}
	if err != nil {
		return Credential{}, nil, writeError(err)
	}
	if n == 0 {
		return Credential{}, nil, ErrNotFound
	}
	if slices.Contains(changed, "secret") {
		sealed, err := s.v.Seal(vault.ContextGitCred+id, []byte(clean.Secret))
		if err != nil {
			return Credential{}, nil, fmt.Errorf("gitconn: seal the secret: %w", err)
		}
		if _, err := q.UpdateGitCredentialSecret(ctx, sqlc.UpdateGitCredentialSecretParams{
			SecretSealed: sealed, SecretLast4: clean.last4, PublicKey: clean.publicKey, Fingerprint: clean.fingerprint,
			UpdatedAt: now, ID: id,
		}); err != nil {
			return Credential{}, nil, fmt.Errorf("gitconn: store the secret: %w", err)
		}
	}
	// A use or a refusal seen with the old secret, type or user name says nothing about the new one.
	if slices.Contains(changed, "secret") || slices.Contains(changed, "type") || slices.Contains(changed, "username") {
		if _, err := q.ClearGitCredentialOutcome(ctx, id); err != nil {
			return Credential{}, nil, fmt.Errorf("gitconn: clear the last outcome: %w", err)
		}
	}
	row, err = q.GetGitCredential(ctx, id)
	if err != nil {
		return Credential{}, nil, fmt.Errorf("gitconn: read back: %w", err)
	}
	c := credentialOf(row)
	if hook != nil {
		if err := hook(ctx, tx, c, changed); err != nil {
			return Credential{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, nil, fmt.Errorf("gitconn: commit: %w", err)
	}
	return c, changed, nil
}

// DeleteCredential removes a credential of one layer (envID empty for the instance's), running hook
// (which may be nil) in the same transaction with the credential as it was. A credential of any
// other layer, or one that is gone, is ErrNotFound, so a repeated delete is safe. A credential
// that repositories use is an *InUseError with their count, and nothing is removed.
func (s *Store) DeleteCredential(ctx context.Context, envID, id string, hook TxHook) (Credential, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetGitCredential(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: read: %w", err)
	}
	c := credentialOf(row)
	if c.EnvID != envID {
		return Credential{}, ErrNotFound
	}
	// The write connection is the only writer, so the count cannot change before the delete. The
	// foreign key (ON DELETE NO ACTION) is the backstop.
	used, err := q.CountReposUsingCredential(ctx, nullString(id))
	if err != nil {
		return Credential{}, fmt.Errorf("gitconn: count the repositories that use it: %w", err)
	}
	if used > 0 {
		return Credential{}, &InUseError{Count: int(used)}
	}
	if _, err := q.DeleteGitCredential(ctx, sqlc.DeleteGitCredentialParams{ID: id, EnvID: nullString(envID)}); errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrNotFound
	} else if err != nil {
		return Credential{}, fmt.Errorf("gitconn: delete: %w", err)
	}
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

// ListCredentials returns every credential of every layer: the instance's first, then each
// environment's, by name.
func (s *Store) ListCredentials(ctx context.Context) ([]Credential, error) {
	rows, err := s.st.RQ.ListGitCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitconn: list credentials: %w", err)
	}
	out := make([]Credential, len(rows))
	for i, r := range rows {
		out[i] = credentialOf(sqlc.GetGitCredentialRow(r))
	}
	return out, nil
}

// RepoUpdate is what a person types to change a repository. CredentialID nil keeps the saved
// credential (unless the address moved to another host), an empty string picks none.
type RepoUpdate struct {
	Name, URL    string
	CredentialID *string
	CAPEM        string
	// HostKey is an SSH host key the person confirmed, in authorized_keys form. Empty keeps the
	// saved key unless the address moved to another host or port, which forgets it: a key trusted
	// for one machine is never aimed at another. TrustedBy is who confirmed it.
	HostKey   string
	TrustedBy string
}

// RepoUpdateHook is UpdateHook for a repository.
type RepoUpdateHook func(ctx context.Context, tx *sql.Tx, r Repo, changed []string) error

// UpdateRepo changes a repository of one layer (envID empty for the instance's); one of any other
// layer is ErrNotFound. A credential must be one the layer rule allows (credential_scope). When the
// address moves to another host or port, a saved credential is kept only if the request names a
// credential again (the same one, another, or none with an empty ID): else *ValidationError
// (credentialId: credential_reselect), so a token the person picked for one server is never aimed
// at another by an edit that did not mention it. It returns the repository as it now is and the
// names of the fields that changed, sorted (ca_pem, credential, host_key, name, url). A form that changes
// nothing writes nothing and runs no hook. The last test is forgotten when the address, the
// credential or the CA changed: it was seen against something that no longer applies.
func (s *Store) UpdateRepo(ctx context.Context, envID, id string, in RepoUpdate, hook RepoUpdateHook) (Repo, []string, error) {
	now := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, nil, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetGitRepo(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, nil, ErrNotFound
	}
	if err != nil {
		return Repo{}, nil, fmt.Errorf("gitconn: read: %w", err)
	}
	cur := repoOf(row)
	if cur.EnvID != envID {
		return Repo{}, nil, ErrNotFound
	}
	name, url, ca, _, errs := validRepo(in.Name, in.URL, in.CAPEM)
	key, keyErr := validHostKey(in.HostKey, url)
	if keyErr != nil {
		errs = append(errs, *keyErr)
	}
	if len(errs) > 0 {
		return Repo{}, nil, &ValidationError{Fields: errs}
	}
	// The trusted key stays with the machine it was confirmed for.
	hostKey := cur.HostKey
	if destinationOf(cur.URL) != destinationOf(url) {
		hostKey = ""
	}
	if key.Line != "" {
		hostKey = key.Line
	}
	cred := cur.CredentialID
	if in.CredentialID != nil {
		cred = strings.TrimSpace(*in.CredentialID)
	}
	if in.CredentialID == nil && cur.CredentialID != "" && destinationOf(cur.URL) != destinationOf(url) {
		return Repo{}, nil, invalid("credentialId", CodeCredentialReselect)
	}
	if cred, err = s.pickedCredential(ctx, q, envID, cred); err != nil {
		return Repo{}, nil, err
	}

	var changed []string
	for _, f := range []struct {
		name string
		diff bool
	}{
		{"ca_pem", ca != cur.CAPEM},
		{"credential", cred != cur.CredentialID},
		{"host_key", hostKey != cur.HostKey},
		{"name", name != cur.Name},
		{"url", url != cur.URL},
	} {
		if f.diff {
			changed = append(changed, f.name)
		}
	}
	if len(changed) == 0 {
		return cur, nil, nil
	}

	var n int64
	if envID == "" {
		n, err = q.UpdateGitRepoInstance(ctx, sqlc.UpdateGitRepoInstanceParams{
			Name: name, Url: url, CredentialID: nullString(cred), CaPem: ca, UpdatedAt: now, ID: id,
		})
	} else {
		n, err = q.UpdateGitRepoInEnv(ctx, sqlc.UpdateGitRepoInEnvParams{
			Name: name, Url: url, CredentialID: nullString(cred), CaPem: ca, UpdatedAt: now, ID: id, EnvID: nullString(envID),
		})
	}
	if err != nil {
		return Repo{}, nil, writeError(err)
	}
	if n == 0 {
		return Repo{}, nil, ErrNotFound
	}
	if slices.Contains(changed, "host_key") {
		if err := setHostKey(ctx, q, id, hostKey, in.TrustedBy, now); err != nil {
			return Repo{}, nil, err
		}
	}
	if slices.Contains(changed, "ca_pem") || slices.Contains(changed, "credential") || slices.Contains(changed, "url") || slices.Contains(changed, "host_key") {
		if _, err := q.ClearGitRepoTest(ctx, id); err != nil {
			return Repo{}, nil, fmt.Errorf("gitconn: clear the last test: %w", err)
		}
	}
	row, err = q.GetGitRepo(ctx, id)
	if err != nil {
		return Repo{}, nil, fmt.Errorf("gitconn: read back: %w", err)
	}
	r := repoOf(row)
	if hook != nil {
		if err := hook(ctx, tx, r, changed); err != nil {
			return Repo{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Repo{}, nil, fmt.Errorf("gitconn: commit: %w", err)
	}
	return r, changed, nil
}

// setHostKey stores line as the trusted SSH host key of repository id, with who trusted it and when;
// an empty line forgets the trust. line has been validated.
func setHostKey(ctx context.Context, q *sqlc.Queries, id, line, trustedBy string, now int64) error {
	var fp string
	var at sql.NullInt64
	if line != "" {
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return fmt.Errorf("gitconn: read the host key to store: %w", err)
		}
		fp, at = ssh.FingerprintSHA256(pub), sql.NullInt64{Int64: now, Valid: true}
	} else {
		trustedBy = ""
	}
	if _, err := q.SetGitRepoHostKey(ctx, sqlc.SetGitRepoHostKeyParams{
		HostKey: line, HostKeyFp: fp, HostKeyTrustedBy: trustedBy, HostKeyTrustedAt: at, UpdatedAt: now, ID: id,
	}); err != nil {
		return fmt.Errorf("gitconn: store the host key of repository %s: %w", id, err)
	}
	return nil
}

// HostKeyHook is TxHook for trusting a host key: it sees the repository as it was and as it now is.
type HostKeyHook func(ctx context.Context, tx *sql.Tx, before, after Repo) error

// TrustHostKey stores line, an SSH host key in authorized_keys form, as the one the repository's host
// is trusted by, replacing any earlier one, and records actorID and the time. It is the explicit
// act that follows a host_key_changed or a first look at an unknown host; nothing in a fetch or a
// test ever calls it. The last test is forgotten, and hook (which may be nil) runs in the same
// transaction with the repository before and after. It returns ErrNotFound, or *ValidationError
// (hostKey: host_key_invalid, or host_key_not_ssh for an address that is not ssh).
func (s *Store) TrustHostKey(ctx context.Context, repoID, line, actorID string, hook HostKeyHook) (Repo, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := q.GetGitRepo(ctx, repoID)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: read: %w", err)
	}
	before := repoOf(row)
	key, keyErr := validHostKey(line, before.URL)
	if keyErr == nil && key.Line == "" {
		keyErr = &FieldError{Field: "hostKey", Code: CodeHostKeyInvalid}
	}
	if keyErr != nil {
		return Repo{}, &ValidationError{Fields: []FieldError{*keyErr}}
	}
	if err := setHostKey(ctx, q, repoID, key.Line, actorID, s.now().UTC().UnixNano()); err != nil {
		return Repo{}, err
	}
	if _, err := q.ClearGitRepoTest(ctx, repoID); err != nil {
		return Repo{}, fmt.Errorf("gitconn: clear the last test: %w", err)
	}
	row, err = q.GetGitRepo(ctx, repoID)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: read back: %w", err)
	}
	after := repoOf(row)
	if hook != nil {
		if err := hook(ctx, tx, before, after); err != nil {
			return Repo{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Repo{}, fmt.Errorf("gitconn: commit: %w", err)
	}
	return after, nil
}

// destinationOf is where an address connects to: its host and port.
func destinationOf(rawURL string) string {
	u, err := gitx.ValidateURL(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host + ":" + strconv.Itoa(u.Port)
}

// DeleteRepo removes a repository of one layer (envID empty for the instance's), running hook
// (which may be nil) in the same transaction with the repository as it was. One of any other layer,
// or one that is gone, is ErrNotFound.
func (s *Store) DeleteRepo(ctx context.Context, envID, id string, hook RepoHook) (Repo, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)

	row, err := q.GetGitRepo(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, ErrNotFound
	}
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: read: %w", err)
	}
	r := repoOf(row)
	if r.EnvID != envID {
		return Repo{}, ErrNotFound
	}
	// The write connection is the only writer, so the count cannot change before the delete. The foreign key
	// (ON DELETE NO ACTION) is the backstop.
	used, err := q.CountStacksUsingRepo(ctx, id)
	if err != nil {
		return Repo{}, fmt.Errorf("gitconn: count the stacks that use it: %w", err)
	}
	if used > 0 {
		return Repo{}, &RepoInUseError{Count: int(used)}
	}
	if _, err := q.DeleteGitRepo(ctx, sqlc.DeleteGitRepoParams{ID: id, EnvID: nullString(envID)}); errors.Is(err, sql.ErrNoRows) {
		return Repo{}, ErrNotFound
	} else if err != nil {
		return Repo{}, fmt.Errorf("gitconn: delete: %w", err)
	}
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

// ListRepos returns every repository of every layer: the instance's first, then each
// environment's, by name.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.st.RQ.ListGitRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitconn: list repositories: %w", err)
	}
	out := make([]Repo, len(rows))
	for i, r := range rows {
		out[i] = repoOf(sqlc.GetGitRepoRow(r))
	}
	return out, nil
}
