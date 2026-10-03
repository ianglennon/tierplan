// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package agents owns what makes an agent trusted: the join token an admin
// copies onto a host, the per-environment certificate authority, and the
// enrolment that turns a token plus a proof of key possession into a
// certificate and an agent row (D-01, D-02, D-05, D-07, D-13).
//
// Secrets are handled in one of two ways and never a third. A join token is
// hashed with SHA-256 and cannot be recovered; a CA private key is sealed with
// the vault and carries its key ID (D-55, D-56).
package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
)

// ErrInvalidName means an environment name is longer than 64 characters.
var ErrInvalidName = errors.New("agents: the name must be at most 64 characters")

const (
	// KindAgent is the kind of an environment reached through agents.
	KindAgent = "agent"

	tokenPrefix = "bpjt_"
	// secretBytes is the entropy of a join token: 256 bits.
	secretBytes = 32
	maxNameLen  = 64
	// maxAgentName caps the hostname an agent reports, which is untrusted input.
	maxAgentName = 128

	targetEnvironment = "environment"
	targetAgent       = "agent"

	// enrolActor is the system principal under which an enrolment is audited,
	// because the agent has no user behind it (D-13, P1 D-51).
	enrolActor = "system:agent-enrol"
)

// Created is a new agent environment and its first join token. JoinToken is
// shown once: only its hash is stored.
type Created struct {
	EnvID     string
	Name      string
	JoinToken string
	CreatedAt time.Time
}

// Service creates agent environments and enrols agents.
type Service struct {
	st  *store.Store
	v   *vault.Vault
	aud *audit.Writer
	now func() time.Time
}

// NewService returns the service. now defaults to time.Now.
func NewService(st *store.Store, v *vault.Vault, w *audit.Writer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, v: v, aud: w, now: now}
}

var _ tunnel.Verifier = (*Service)(nil)

// CreateEnvironment stores a new agent environment, its CA and its first join
// token, and audits environment.added, in one transaction (D-01, D-07). A blank
// name is filled in by the first agent that enrols (D-23).
func (s *Service) CreateEnvironment(ctx context.Context, actor audit.Actor, name, ip, ua string) (Created, error) {
	name = strings.TrimSpace(name)
	if name != "" && !validName(name) {
		return Created{}, ErrInvalidName
	}
	envUUID, err := uuid.NewV7()
	if err != nil {
		return Created{}, fmt.Errorf("generate environment id: %w", err)
	}
	envID := envUUID.String()
	tokenUUID, err := uuid.NewV7()
	if err != nil {
		return Created{}, fmt.Errorf("generate token id: %w", err)
	}
	now := s.now().UTC()

	authority, err := newCA(envID, now)
	if err != nil {
		return Created{}, err
	}
	sealed, err := authority.seal(s.v, envID)
	if err != nil {
		return Created{}, err
	}
	token, secret, err := newToken(envID)
	if err != nil {
		return Created{}, err
	}
	hash := sha256.Sum256([]byte(secret))

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	ts := now.UnixNano()
	if err := q.InsertAgentEnvironment(ctx, sqlc.InsertAgentEnvironmentParams{ID: envID, Name: name, CreatedAt: ts, UpdatedAt: ts}); err != nil {
		return Created{}, fmt.Errorf("insert environment: %w", err)
	}
	if err := q.InsertCA(ctx, sqlc.InsertCAParams{EnvID: envID, CertPem: string(authority.certPEM), KeySealed: sealed, CreatedAt: ts}); err != nil {
		return Created{}, fmt.Errorf("insert the CA: %w", err)
	}
	if err := q.InsertJoinToken(ctx, sqlc.InsertJoinTokenParams{
		ID: tokenUUID.String(), EnvID: envID, TokenHash: hash[:], Generation: 1, CreatedAt: ts, CreatedBy: actor.ID,
	}); err != nil {
		return Created{}, fmt.Errorf("insert the join token: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentAdded, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"name": name, "kind": KindAgent},
	}); err != nil {
		return Created{}, fmt.Errorf("audit environment added: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Created{}, fmt.Errorf("commit: %w", err)
	}
	return Created{EnvID: envID, Name: name, JoinToken: token, CreatedAt: now}, nil
}

// Enrol turns a join token and a proof of key possession into a certificate. It
// implements tunnel.Verifier. Every way of being refused for the token looks the
// same to the agent (auth_rejected), so a probe learns neither which
// environments exist nor whether a token was once valid.
func (s *Service) Enrol(ctx context.Context, req tunnel.EnrolRequest) (tunnel.EnrolResult, error) {
	reject := func(reason string) error {
		return &tunnel.RefuseError{
			Code: wire.ReasonAuthRejected, Detail: "the join token was not accepted", AuditReason: reason,
		}
	}

	envID, secret, ok := ParseToken(req.Token)
	if !ok {
		return tunnel.EnrolResult{}, reject("malformed_token")
	}

	// The hash is computed and compared whether or not the environment exists,
	// so the time taken does not say.
	row, err := s.st.RQ.CurrentJoinToken(ctx, envID)
	stored := make([]byte, sha256.Size)
	found := true
	switch {
	case errors.Is(err, sql.ErrNoRows):
		found = false
	case err != nil:
		return tunnel.EnrolResult{}, fmt.Errorf("load the join token: %w", err)
	default:
		stored = row.TokenHash
	}
	got := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(got[:], stored) != 1 || !found {
		return tunnel.EnrolResult{}, reject("token_mismatch")
	}
	if row.DisabledAt.Valid {
		return tunnel.EnrolResult{}, reject(wire.RefuseTokenDisabled)
	}

	// Proof of possession: the agent signed the transcript the handler built
	// with the key it wants a certificate for.
	if !authn.Verify(req.Pub, req.Transcript, req.Sig) {
		return tunnel.EnrolResult{}, reject("bad_proof_of_possession")
	}

	caRow, err := s.st.RQ.GetCA(ctx, envID)
	if err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("load the CA: %w", err)
	}
	authority, err := loadCA(s.v, envID, caRow.CertPem, caRow.KeySealed)
	if err != nil {
		return tunnel.EnrolResult{}, err
	}

	agentUUID, err := uuid.NewV7()
	if err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("generate agent id: %w", err)
	}
	agentID := agentUUID.String()
	now := s.now().UTC()
	leaf, err := authority.issue(agentID, envID, req.Pub, now)
	if err != nil {
		return tunnel.EnrolResult{}, err
	}
	fp := sha256.Sum256(leaf.DER)
	fingerprint := hex.EncodeToString(fp[:])

	host := cleanName(req.Facts.Hostname)
	role := roleOf(req.Facts)
	nodeID := sql.NullString{}
	if sw := req.Facts.Swarm; sw != nil && sw.NodeID != "" {
		nodeID = sql.NullString{String: sw.NodeID, Valid: true}
	}

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	ts := now.UnixNano()
	if err := q.InsertAgent(ctx, sqlc.InsertAgentParams{
		ID: agentID, EnvID: envID, Name: host, Role: role, EngineID: req.Facts.EngineID, SwarmNodeID: nodeID,
		PublicKey: req.Pub, CertSerial: leaf.Serial, CertFingerprint: fingerprint, CertNotAfter: leaf.NotAfter.UnixNano(),
		AgentVersion: req.AgentVersion, CreatedAt: ts,
	}); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("insert the agent: %w", err)
	}
	// A blank environment name is the first agent's hostname (D-23); a name the
	// admin typed is never overwritten.
	if host != "" {
		if _, err := q.NameEnvironmentIfBlank(ctx, sqlc.NameEnvironmentIfBlankParams{Name: host, UpdatedAt: ts, ID: envID}); err != nil {
			return tunnel.EnrolResult{}, fmt.Errorf("name the environment: %w", err)
		}
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: rbac.System(enrolActor).AuditActor(), Action: audit.ActionAgentEnrolled,
		TargetType: targetAgent, TargetID: agentID, EnvID: envID, Result: audit.ResultSuccess,
		IP: req.IP, UserAgent: "bayplan-agent/" + req.AgentVersion,
		Details: map[string]string{"fingerprint": fingerprint, "node": host, "serial": leaf.Serial},
	}); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("audit agent enrolled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("commit: %w", err)
	}
	return tunnel.EnrolResult{AgentID: agentID, EnvID: envID, Cert: leaf.DER, NotAfter: leaf.NotAfter}, nil
}

// Hello checks the reconnect of an enrolled agent. Stub.
func (s *Service) Hello(_ context.Context, _ tunnel.HelloRequest) (tunnel.HelloResult, error) {
	return tunnel.HelloResult{}, &tunnel.Refusal{Code: wire.ReasonAuthRejected, Detail: "hello is not implemented"}
}

// newToken makes a join token bpjt_<envId>_<base64url of 32 random bytes> and
// returns it with its secret part, the only part that is hashed.
func newToken(envID string) (token, secret string, err error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate a join token: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(b)
	return tokenPrefix + envID + "_" + secret, secret, nil
}

// ParseToken splits a join token into its environment ID and secret. The
// environment ID is a UUID and holds no underscore, and the secret may, so the
// first underscore after the prefix ends the ID.
func ParseToken(token string) (envID, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, tokenPrefix)
	if !found {
		return "", "", false
	}
	envID, secret, found = strings.Cut(rest, "_")
	if !found {
		return "", "", false
	}
	if _, err := uuid.Parse(envID); err != nil {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != secretBytes {
		return "", "", false
	}
	return envID, secret, true
}

// AgentURL is the address an agent dials for a public URL: the same host and
// base path, https as wss and http as ws, and /agent/connect on the end (D-18).
func AgentURL(publicURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(publicURL))
	if err != nil {
		return "", fmt.Errorf("parse the public URL: %w", err)
	}
	var scheme string
	switch strings.ToLower(u.Scheme) {
	case "https":
		scheme = "wss"
	case "http":
		scheme = "ws"
	default:
		return "", fmt.Errorf("the public URL %q is not http or https", publicURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("the public URL %q has no host", publicURL)
	}
	out := url.URL{Scheme: scheme, Host: u.Host, Path: strings.TrimRight(u.Path, "/") + "/agent/connect"}
	return out.String(), nil
}

func roleOf(f wire.Facts) string {
	if f.Swarm == nil {
		return "standalone"
	}
	if f.Swarm.Control {
		return "manager"
	}
	return "worker"
}

func validName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= maxNameLen
}

// cleanName cuts a hostname an agent reported to the stored length. The agent
// is not trusted to send something short or printable.
func cleanName(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > maxNameLen {
		s = string([]rune(s)[:maxNameLen])
	}
	if len(s) > maxAgentName {
		s = s[:maxAgentName]
	}
	return s
}
