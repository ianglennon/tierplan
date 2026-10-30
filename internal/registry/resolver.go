// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// hubServerAddress is the registry address the Docker CLI writes for Docker Hub in the
// X-Registry-Auth header and in config.json.
const hubServerAddress = "https://index.docker.io/v1/"

// Resolver chooses the stored credential an image is pulled with and turns it into the
// registry header the engine takes. It is the one place a secret leaves the vault for a
// registry (D-25): Phases 6, 7 and 12 call it on every service write, and a pull calls it
// once per job.
type Resolver struct {
	s *Store
}

// NewResolver returns a Resolver that reads credentials from s.
func NewResolver(s *Store) *Resolver { return &Resolver{s: s} }

// AuthFor returns the X-Registry-Auth header for ref in the environment, and the credential
// it was made from. No matching credential is "", nil, nil: the pull is anonymous. The secret
// is opened for this call and not kept. The header is a credential: do not log it, audit it,
// keep it in job state or return it from an API. An error names the credential by ID and never
// holds a value.
func (r *Resolver) AuthFor(ctx context.Context, envID string, ref Ref) (string, *Candidate, error) {
	cands, err := r.s.Candidates(ctx, envID)
	if err != nil {
		return "", nil, err
	}
	m := Select(cands, ref)
	if m.Cred == nil {
		return "", nil, nil
	}
	cred, err := r.s.Get(ctx, m.Cred.ID)
	if err != nil {
		return "", nil, fmt.Errorf("registry: read credential %s: %w", m.Cred.ID, err)
	}
	secret, err := r.s.openSecret(ctx, m.Cred.ID)
	if err != nil {
		return "", nil, err
	}
	header := EncodeAuth(cred.Username, string(secret), cred.Host)
	used := *m.Cred
	return header, &used, nil
}

// EncodeAuth writes the X-Registry-Auth header: the URL-safe base64 of a JSON object with the
// keys username, password and serveraddress. Docker Hub is addressed as the Docker CLI does.
func EncodeAuth(username, password, serverAddress string) string {
	if strings.EqualFold(serverAddress, hubDomain) {
		serverAddress = hubServerAddress
	}
	raw, _ := json.Marshal(struct { //nolint:gosec // G117: this is the registry header, the one place a secret is encoded
		Username      string `json:"username"`
		Password      string `json:"password"`
		ServerAddress string `json:"serveraddress"`
	}{username, password, serverAddress}) // three strings always marshal
	return base64.URLEncoding.EncodeToString(raw)
}

// RecordFailure remembers that the registry refused the credential's sign-in with code (D-07).
func (r *Resolver) RecordFailure(ctx context.Context, id, code string) error {
	return r.s.RecordFailure(ctx, id, code)
}

// ClearFailure forgets the credential's recorded failure after a pull with it worked.
func (r *Resolver) ClearFailure(ctx context.Context, id string) error {
	return r.s.ClearFailure(ctx, id)
}

// ResolvedCredential is a stored credential chosen for a call the server makes to a registry
// itself, with the sign-in opened for it. It holds a secret: it is for one call, never
// logged, audited, cached or returned from an API. A nil *ResolvedCredential means the call
// is anonymous.
//
// Candidate and HostOptions both have a Host; read them through the embedded names.
type ResolvedCredential struct {
	Candidate
	HostOptions
	// UpdatedAt is when the credential was last changed, so a client made for an older
	// version of it is not reused.
	UpdatedAt time.Time
}

// String names the credential by ID alone, so a %v or %+v of one in a log or a test failure
// never prints the secret it holds.
func (c ResolvedCredential) String() string { return "registry credential " + c.ID }

// GoString is String for %#v.
func (c ResolvedCredential) GoString() string { return c.String() }

// LogValue keeps the secret out of structured logs for the same reason.
func (c ResolvedCredential) LogValue() slog.Value { return slog.StringValue(c.String()) }

// ForBrowse chooses the credential the server browses a repository's tags with (D-24a). It
// looks at the instance credentials and, when envID is set, that environment's own, keeps the
// ones visible reports as visible to the caller, and picks among them with Select, the
// function pulls use. With nothing matching it answers nil: browse anonymously. The secret is
// opened for this call and not kept. An error names the credential by ID and never holds a
// value.
func (r *Resolver) ForBrowse(ctx context.Context, envID string, visible func(Candidate) bool, ref Ref) (*ResolvedCredential, error) {
	cands, err := r.s.Candidates(ctx, envID)
	if err != nil {
		return nil, err
	}
	allowed := cands[:0:0]
	for _, c := range cands {
		if visible == nil || visible(c) {
			allowed = append(allowed, c)
		}
	}
	m := Select(allowed, ref)
	if m.Cred == nil {
		return nil, nil
	}
	cred, err := r.s.Get(ctx, m.Cred.ID)
	if err != nil {
		return nil, fmt.Errorf("registry: read credential %s: %w", m.Cred.ID, err)
	}
	secret, err := r.s.openSecret(ctx, m.Cred.ID)
	if err != nil {
		return nil, err
	}
	return &ResolvedCredential{
		Candidate: *m.Cred,
		HostOptions: HostOptions{
			Host: cred.Host, Username: cred.Username, Password: string(secret), CAPEM: cred.CAPEM, Insecure: cred.Insecure,
		},
		UpdatedAt: cred.UpdatedAt,
	}, nil
}
