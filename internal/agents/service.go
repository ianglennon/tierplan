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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
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

	// lastSeenEvery is the shortest time between two writes of one agent's
	// last_seen_at.
	lastSeenEvery = time.Minute

	// statusActive is the status of an agent that may connect.
	statusActive = "active"

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
	// Generation numbers the join token for the environment: 1 for the first, one
	// more for each rotation. It is in the name of the swarm secret.
	Generation int
	CreatedAt  time.Time
}

// Service creates agent environments and enrols agents.
type Service struct {
	st  *store.Store
	v   *vault.Vault
	aud *audit.Writer
	now func() time.Time

	// seen is when last_seen_at was last written for each agent, so TouchLastSeen
	// can skip all but one write a minute.
	seenMu sync.Mutex
	seen   map[string]time.Time
}

// NewService returns the service. now defaults to time.Now.
func NewService(st *store.Store, v *vault.Vault, w *audit.Writer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, v: v, aud: w, now: now, seen: map[string]time.Time{}}
}

var (
	_ tunnel.Verifier         = (*Service)(nil)
	_ tunnel.LastSeenRecorder = (*Service)(nil)
)

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
	return Created{EnvID: envID, Name: name, JoinToken: token, Generation: 1, CreatedAt: now}, nil
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

	// The order of RESEARCH Pattern 2 for an enrolment is token, blocked identity,
	// duplicates, version, clock, then issuance. The token is above; the version
	// window was checked by the hub before it asked. Plan 02-15 adds the block list
	// and plan 02-19 the duplicate check here, ahead of the clock.
	if err := tunnel.CheckClock(req.ClockOffset); err != nil {
		return tunnel.EnrolResult{}, err
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

// TouchLastSeen records that an agent was just heard from, in agents.last_seen_at.
// Heartbeats arrive every few seconds and the store has one writer, so a write
// per heartbeat would starve sign-ins (RESEARCH Pitfall 8): the time is written
// at most once a minute per agent, and the rest are dropped here, in memory. A
// failed write is forgotten, so the next heartbeat tries again. It implements
// tunnel.LastSeenRecorder.
func (s *Service) TouchLastSeen(ctx context.Context, agentID string) {
	now := s.now()
	s.seenMu.Lock()
	if last, ok := s.seen[agentID]; ok && now.Sub(last) < lastSeenEvery {
		s.seenMu.Unlock()
		return
	}
	s.seen[agentID] = now
	s.seenMu.Unlock()

	err := s.st.Q.TouchAgentLastSeen(ctx, sqlc.TouchAgentLastSeenParams{
		LastSeenAt: sql.NullInt64{Int64: now.UnixNano(), Valid: true}, ID: agentID,
	})
	if err != nil {
		s.seenMu.Lock()
		delete(s.seen, agentID)
		s.seenMu.Unlock()
	}
}

// Hello checks the reconnect of an enrolled agent. It implements tunnel.Verifier.
//
// Authentication is by key, not by serial: any unexpired certificate that chains
// to the environment's CA and carries the public key stored for that agent is
// accepted, so a renewal needs no two-phase commit (RESEARCH Pattern 3). The
// checks run cheapest and least revealing first, and everything that would let a
// probe learn something (an unknown environment, a certificate from another CA,
// an agent that does not exist, a key that differs, a bad signature) is the same
// auth_rejected with the same text. Only an agent that holds a certificate the
// environment really issued can learn that it expired or was revoked.
func (s *Service) Hello(ctx context.Context, req tunnel.HelloRequest) (tunnel.HelloResult, error) {
	reject := func(reason string) error {
		return &tunnel.Refusal{
			Code: wire.ReasonAuthRejected, Detail: "the agent's certificate was not accepted", AuditReason: reason,
		}
	}

	leaf, err := x509.ParseCertificate(req.Cert)
	if err != nil {
		return tunnel.HelloResult{}, reject("bad_certificate")
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.CommonName == "" {
		return tunnel.HelloResult{}, reject("no_identity_in_certificate")
	}
	envID, agentID := leaf.Subject.OrganizationalUnit[0], leaf.Subject.CommonName
	if req.AgentID != agentID {
		return tunnel.HelloResult{}, reject("agent_id_mismatch")
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return tunnel.HelloResult{}, reject("not_an_ed25519_key")
	}

	// An environment that does not exist and a certificate from another
	// environment's CA both end here with the same answer.
	caRow, err := s.st.RQ.GetCA(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tunnel.HelloResult{}, reject("unknown_environment")
	case err != nil:
		return tunnel.HelloResult{}, fmt.Errorf("load the CA: %w", err)
	}
	now := s.now()
	if _, err := VerifyLeaf(caRow.CertPem, req.Cert, now); err != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) && invalid.Reason == x509.Expired && now.After(leaf.NotAfter) {
			return tunnel.HelloResult{}, &tunnel.Refusal{
				Code:        wire.ReasonCertExpired,
				Detail:      "the agent's certificate has expired; enrol the agent again with a join token",
				AuditReason: wire.ReasonCertExpired,
			}
		}
		return tunnel.HelloResult{}, reject("bad_chain")
	}

	row, err := s.st.RQ.GetAgent(ctx, agentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tunnel.HelloResult{}, reject("unknown_agent")
	case err != nil:
		return tunnel.HelloResult{}, fmt.Errorf("load the agent: %w", err)
	}
	if row.EnvID != envID {
		return tunnel.HelloResult{}, reject("environment_mismatch")
	}
	if row.Status != statusActive {
		return tunnel.HelloResult{}, &tunnel.Refusal{
			Code:        wire.ReasonRevoked,
			Detail:      "an administrator revoked this agent",
			AuditReason: wire.ReasonRevoked,
		}
	}
	if subtle.ConstantTimeCompare(row.PublicKey, pub) != 1 {
		return tunnel.HelloResult{}, reject("key_mismatch")
	}
	if !authn.Verify(pub, req.Transcript, req.Sig) {
		return tunnel.HelloResult{}, reject("bad_signature")
	}
	// Only an agent that has proved its identity is told its clock is wrong.
	if err := tunnel.CheckClock(req.ClockOffset); err != nil {
		return tunnel.HelloResult{}, err
	}
	return tunnel.HelloResult{AgentID: agentID, EnvID: envID, NotAfter: leaf.NotAfter}, nil
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
// It is config.AgentEndpoint, kept here for the callers that already use it.
func AgentURL(publicURL string) (string, error) { return config.AgentEndpoint(publicURL) }

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
