// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/wire"
)

// ErrInvalidName means an environment name is longer than 64 characters.
var ErrInvalidName = errors.New("agents: the name must be at most 64 characters")

// ErrEnvironmentNotFound means there is no agent environment with that ID. The
// local host has no join token, so it is not found here either.
var ErrEnvironmentNotFound = errors.New("agents: no such agent environment")

// Errors of revoking and unblocking an agent.
var (
	// ErrAgentNotFound means the environment has no agent with that ID.
	ErrAgentNotFound = errors.New("agents: no such agent in this environment")
	// ErrAlreadyRevoked means the agent was revoked before, by this admin or another.
	ErrAlreadyRevoked = errors.New("agents: the agent is already revoked")
	// ErrNotBlocked means the agent has no block to lift.
	ErrNotBlocked = errors.New("agents: the agent has no block to lift")
	// ErrNoClusterChange means the environment has no recorded change of cluster to
	// adopt, for example because another admin adopted it a moment ago.
	ErrNoClusterChange = errors.New("agents: the environment has no cluster change to adopt")
	// ErrClusterInUse means another environment has bound the cluster since the change
	// was recorded, so this one cannot adopt it.
	ErrClusterInUse = errors.New("agents: another environment already manages that cluster")
)

const (
	// KindAgent is the kind of an environment reached through agents.
	KindAgent = "agent"

	tokenPrefix = "tpjt_"
	// secretBytes is the entropy of a join token: 256 bits.
	secretBytes = 32
	maxNameLen  = 64
	// maxAgentName caps the hostname an agent reports, which is untrusted input.
	maxAgentName = 128

	// stillActiveTimeout bounds the status read made as a session registers.
	stillActiveTimeout = 5 * time.Second

	// lastSeenEvery is the shortest time between two writes of one agent's
	// last_seen_at.
	lastSeenEvery = time.Minute

	// statusActive is the status of an agent that may connect.
	statusActive = "active"

	targetEnvironment = "environment"
	targetAgent       = "agent"

	// Kinds of identity a revoke blocks (the CHECK of agent_blocks).
	blockEngineID    = "engine_id"
	blockSwarmNodeID = "swarm_node_id"

	// maxRevokeReason is the longest reason an admin may give, in characters.
	maxRevokeReason = 256

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

// Rotation is a new join token for an environment. JoinToken is shown once: only
// its hash is stored.
type Rotation struct {
	JoinToken string
	// Generation is the new token's number: one more than the one it replaced. It is
	// in the name of the swarm secret.
	Generation int
	// EnvType is "swarm" when an active agent runs on a swarm node, "standalone"
	// when one runs on a plain host, and "" while the environment has no active
	// agent, so there is nothing to rotate on.
	EnvType string
	// HasAgents is whether the environment has an active agent.
	HasAgents bool
}

// TokenStatus is what an admin may see of the current join token. The token itself
// cannot be recovered and is not here.
type TokenStatus struct {
	Generation int
	CreatedAt  time.Time
	// CreatedBy is the username of the admin who made the token, or the principal as
	// it was recorded when that is not an account (the CLI, a deleted user).
	CreatedBy string
	// Disabled is true while the token enrols nobody, until the next rotation.
	Disabled bool
}

// Environment types reported by Rotation.
const (
	// EnvTypeSwarm and EnvTypeStandalone are the values of Rotation.EnvType.
	EnvTypeSwarm      = "swarm"
	EnvTypeStandalone = "standalone"
)

// SessionKicker closes live agent sessions. The tunnel hub is one. The service
// calls it after the change that makes a session unwelcome has committed, never
// before, so a kicked agent that reconnects at once is judged by the new state.
type SessionKicker interface {
	// Kick closes the live session of one agent, recording code as why.
	Kick(agentID, code string)
	// KickEnv closes every live session of an environment's agents.
	KickEnv(envID, code string)
}

// EnvNotifier is told when something about an environment changed that its health
// shows, so the overview updates at once and not at the next tick. The health
// broker is one. It never blocks.
type EnvNotifier interface {
	Notify(envID string)
}

// LiveSessions says whether an agent is connected now. The tunnel hub is one.
type LiveSessions interface {
	// Live reports whether agentID has a live session now.
	Live(agentID string) bool
}

// NodeLists says which nodes a swarm environment's managers last listed. envs.Clusters is one.
type NodeLists interface {
	// SwarmNodeIDs is the node IDs of the swarm of envID in the node list last read, and
	// when it was read. ok is false when no list is held or the quorum is lost.
	SwarmNodeIDs(envID string) (ids []string, fetchedAt time.Time, ok bool)
}

// nodeListMaxAge is how old a node list may be and still decide an enrolment. The cluster
// cache re-reads every 15 seconds, so a list older than this means no manager has answered
// for a while (the quorum is lost, the only manager is the one enrolling, or none is
// connected), and a list that stale is no basis for a refusal.
const nodeListMaxAge = 2 * time.Minute

// nodeVerdict is what the node list says about the swarm node an enrolling agent reports.
type nodeVerdict int

const (
	// nodeNotChecked: a standalone host, or no NodeLists provider is set.
	nodeNotChecked nodeVerdict = iota
	// nodeUnverified: no fresh list to check against, or the agent reports no node ID.
	nodeUnverified
	// nodeListed: a fresh list holds the node.
	nodeListed
)

// Service creates agent environments and enrols agents.
type Service struct {
	st  *store.Store
	v   *vault.Vault
	aud *audit.Writer
	now func() time.Time
	// kicker, notifier, live and nodes are set before the service is used (WithKicker,
	// WithNotifier, WithLive, WithNodeLists).
	kicker   SessionKicker
	notifier EnvNotifier
	live     LiveSessions
	nodes    NodeLists

	// seen is when last_seen_at was last written for each agent, so TouchLastSeen
	// can skip all but one write a minute.
	seenMu sync.Mutex
	seen   map[string]time.Time

	// unknown is the agents refused unknown_agent lately (D-31).
	unknown *UnknownAgents
}

// NewService returns the service. now defaults to time.Now.
func NewService(st *store.Store, v *vault.Vault, w *audit.Writer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, v: v, aud: w, now: now, seen: map[string]time.Time{}, unknown: NewUnknownAgents()}
}

// Unknown is the list of agents refused unknown_agent, which the Settings page and
// the bell show to instance admins (D-31).
func (s *Service) Unknown() *UnknownAgents { return s.unknown }

// WithKicker tells the service who closes sessions after a revoke. Without one a
// revoke still commits and is honoured at the next hello. Call it before the
// service is used.
func (s *Service) WithKicker(k SessionKicker) *Service {
	s.kicker = k
	return s
}

// WithNotifier tells the service who to tell when an environment's cluster
// change is recorded or adopted. Without one the overview learns at its next
// tick. Call it before the service is used.
func (s *Service) WithNotifier(n EnvNotifier) *Service {
	s.notifier = n
	return s
}

// WithLive tells the service who knows which agents are connected now, so an
// enrolment never retires a host's earlier agent while that agent is connected
// (review 17-2 WR-02). Without one every earlier agent of the host counts as not
// connected. Call it before the service is used.
func (s *Service) WithLive(l LiveSessions) *Service {
	s.live = l
	return s
}

// WithNodeLists tells the service where the node lists of swarm environments are, so an
// enrolment checks the swarm node the agent reports against the list of a manager, and never
// retires a node's earlier agent for a node no list has shown. Without one the node is not
// checked and the retire behaves as it did before the check existed. Call it before the
// service is used.
func (s *Service) WithNodeLists(n NodeLists) *Service {
	s.nodes = n
	return s
}

// checkSwarmNode says what the node list makes of the swarm node f reports. A standalone
// host, or a service with no NodeLists, is not checked. An agent that reports no node ID, and
// a swarm with no fresh list (none held, the quorum lost, or older than nodeListMaxAge), is
// unverified: accepted, but its earlier agents are left to the reaper. A fresh list that
// holds the node is listed. A fresh list that lacks it is refused node_not_in_swarm: the node
// the agent claims is not in the swarm, or joined since the list was read and enrols at a
// retry. The check is on the node ID only; a hostname is as visible as a node ID, so
// comparing it would prove nothing and would refuse a renamed host.
func (s *Service) checkSwarmNode(envID string, f wire.Facts) (nodeVerdict, error) {
	if f.Swarm == nil || s.nodes == nil {
		return nodeNotChecked, nil
	}
	if f.Swarm.NodeID == "" {
		return nodeUnverified, nil
	}
	ids, fetchedAt, ok := s.nodes.SwarmNodeIDs(envID)
	if !ok || s.now().Sub(fetchedAt) > nodeListMaxAge {
		return nodeUnverified, nil
	}
	if slices.Contains(ids, f.Swarm.NodeID) {
		return nodeListed, nil
	}
	return nodeUnverified, &tunnel.RefuseError{
		Code: wire.RefuseNodeNotInSwarm, AuditReason: wire.RefuseNodeNotInSwarm,
		Detail: "this node is not in the swarm's node list as TierPlan last read it; a node that just joined enrols by itself once TierPlan reads the list again, within about a minute; " +
			"a node removed from the swarm with docker node rm must join the swarm again",
	}
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
	// An environment created without a name carries the placeholder until the
	// first host that can name it does (D-23).
	nameAuto := int64(0)
	if name == "" {
		name, nameAuto = placeholderName, 1
	}
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
	if err := q.InsertAgentEnvironment(ctx, sqlc.InsertAgentEnvironmentParams{ID: envID, Name: name, CreatedAt: ts, UpdatedAt: ts, NameAuto: nameAuto}); err != nil {
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

// RotateToken makes a new join token for an environment and retires the current
// one in the same transaction, so there is never a moment with two (D-02). Agents
// that are already enrolled keep their certificates and reconnect as before; only
// an enrolment with the old token stops working. It audits
// environment.token_rotated with the new generation, never the token.
func (s *Service) RotateToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) (Rotation, error) {
	tokenUUID, err := uuid.NewV7()
	if err != nil {
		return Rotation{}, fmt.Errorf("generate token id: %w", err)
	}
	now := s.now().UTC()

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Rotation{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := requireAgentEnvironment(ctx, q, envID); err != nil {
		return Rotation{}, err
	}
	// The store has one writer, so no other rotation runs between this read and the
	// insert below.
	next, err := q.NextGeneration(ctx, envID)
	if err != nil {
		return Rotation{}, fmt.Errorf("next generation: %w", err)
	}
	token, secret, err := newToken(envID)
	if err != nil {
		return Rotation{}, err
	}
	hash := sha256.Sum256([]byte(secret))
	ts := now.UnixNano()
	if _, err := q.SupersedeCurrentToken(ctx, sqlc.SupersedeCurrentTokenParams{SupersededAt: sql.NullInt64{Int64: ts, Valid: true}, EnvID: envID}); err != nil {
		return Rotation{}, fmt.Errorf("supersede the join token: %w", err)
	}
	if err := q.InsertJoinToken(ctx, sqlc.InsertJoinTokenParams{
		ID: tokenUUID.String(), EnvID: envID, TokenHash: hash[:], Generation: next, CreatedAt: ts, CreatedBy: actor.ID,
	}); err != nil {
		return Rotation{}, fmt.Errorf("insert the join token: %w", err)
	}
	roles, err := q.ActiveAgentRoles(ctx, envID)
	if err != nil {
		return Rotation{}, fmt.Errorf("count the agents: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentTokenRotated, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"generation": strconv.FormatInt(next, 10)},
	}); err != nil {
		return Rotation{}, fmt.Errorf("audit token rotated: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Rotation{}, fmt.Errorf("commit: %w", err)
	}
	envType, has := envTypeOf(roles)
	return Rotation{JoinToken: token, Generation: int(next), EnvType: envType, HasAgents: has}, nil
}

// DisableToken stops the current join token enrolling anyone, until the next
// rotation makes a new one. Agents already enrolled are not affected. Disabling a
// token that is already disabled changes nothing and is not audited again.
func (s *Service) DisableToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) error {
	now := s.now().UTC()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := requireAgentEnvironment(ctx, q, envID); err != nil {
		return err
	}
	cur, err := q.TokenStatus(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("load the join token: %w", err)
	}
	rows, err := q.DisableCurrentToken(ctx, sqlc.DisableCurrentTokenParams{DisabledAt: sql.NullInt64{Int64: now.UnixNano(), Valid: true}, EnvID: envID})
	if err != nil {
		return fmt.Errorf("disable the join token: %w", err)
	}
	if rows == 0 {
		return nil
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentTokenDisabled, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"generation": strconv.FormatInt(cur.Generation, 10)},
	}); err != nil {
		return fmt.Errorf("audit token disabled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// TokenStatus reports which join token is current and whether it is disabled.
func (s *Service) TokenStatus(ctx context.Context, envID string) (TokenStatus, error) {
	if err := requireAgentEnvironment(ctx, s.st.RQ, envID); err != nil {
		return TokenStatus{}, err
	}
	row, err := s.st.RQ.TokenStatus(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TokenStatus{}, ErrEnvironmentNotFound
	case err != nil:
		return TokenStatus{}, fmt.Errorf("load the join token: %w", err)
	}
	return TokenStatus{
		Generation: int(row.Generation), CreatedAt: time.Unix(0, row.CreatedAt).UTC(), CreatedBy: row.CreatedBy, Disabled: row.DisabledAt.Valid,
	}, nil
}

// requireAgentEnvironment reports ErrEnvironmentNotFound unless envID is an agent
// environment.
func requireAgentEnvironment(ctx context.Context, q *sqlc.Queries, envID string) error {
	n, err := q.AgentEnvironmentExists(ctx, envID)
	if err != nil {
		return fmt.Errorf("look up the environment: %w", err)
	}
	if n == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}

// envTypeOf says what kind of host the active agents run on. A swarm node makes
// it a swarm; with no active agent there is no type.
func envTypeOf(roles []sqlc.ActiveAgentRolesRow) (envType string, hasAgents bool) {
	for _, r := range roles {
		if r.Agents == 0 {
			continue
		}
		hasAgents = true
		if r.Role == "manager" || r.Role == "worker" {
			return EnvTypeSwarm, true
		}
	}
	if hasAgents {
		return EnvTypeStandalone, true
	}
	return "", false
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

	// The hash is computed and compared against every token the environment has
	// ever had, whether or not the environment exists, so the time taken does not
	// say which one matched. Only the audit learns whether a token that matched was
	// replaced or disabled; the agent is told the same thing for all of them.
	tokens, err := s.st.RQ.ListJoinTokens(ctx, envID)
	if err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("load the join tokens: %w", err)
	}
	got := sha256.Sum256([]byte(secret))
	var matched *sqlc.EnvJoinToken
	for i := range tokens {
		if subtle.ConstantTimeCompare(got[:], tokens[i].TokenHash) == 1 && matched == nil {
			matched = &tokens[i]
		}
	}
	if len(tokens) == 0 {
		_ = subtle.ConstantTimeCompare(got[:], make([]byte, sha256.Size))
	}
	switch {
	case matched == nil:
		return tunnel.EnrolResult{}, reject("token_mismatch")
	case matched.SupersededAt.Valid:
		return tunnel.EnrolResult{}, reject(wire.RefuseTokenSuperseded)
	case matched.DisabledAt.Valid:
		return tunnel.EnrolResult{}, reject(wire.RefuseTokenDisabled)
	}

	// Proof of possession: the agent signed the transcript the handler built
	// with the key it wants a certificate for.
	if !authn.Verify(req.Pub, req.Transcript, req.Sig) {
		return tunnel.EnrolResult{}, reject("bad_proof_of_possession")
	}

	// The order of RESEARCH Pattern 2 for an enrolment is token, blocked identity,
	// duplicates, version, clock, then issuance. The token is above; the version
	// window was checked by the hub before it asked. The block list is next, then
	// the identity checks (type, host, cluster and duplicates). The host proved it
	// holds a valid token before it is told it is blocked or is a duplicate, so
	// the answer is nothing a probe could use. The identity is read here to refuse
	// early, before a certificate is made, and decided again inside the
	// transaction that inserts the agent, which is the decision that binds.
	if err := s.checkBlocked(ctx, s.st.RQ, envID, req.Facts); err != nil {
		return tunnel.EnrolResult{}, err
	}
	if dec, err := CheckIdentity(ctx, s.st.RQ, envID, req.Facts); err != nil {
		return tunnel.EnrolResult{}, s.identityRefused(ctx, envID, dec, err)
	}
	// The node an agent reports is a claim of a join-token holder. A fresh node list that lacks
	// it refuses the enrolment before anything is written; with no fresh list the enrolment
	// goes on, and retires no earlier agent (retireReplaced).
	verdict, err := s.checkSwarmNode(envID, req.Facts)
	if err != nil {
		return tunnel.EnrolResult{}, err
	}
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
	// A revoke of this host's earlier agent may have committed since the check above;
	// the writer is single, so this read sees everything committed before it.
	if err := s.checkBlocked(ctx, q, envID, req.Facts); err != nil {
		return tunnel.EnrolResult{}, err
	}
	// A host that bound the same identity first wins: this read sees it, so the
	// loser is refused here and nothing of it is written.
	dec, err := CheckIdentity(ctx, q, envID, req.Facts)
	if err != nil {
		return tunnel.EnrolResult{}, err
	}
	ts := now.UnixNano()
	if err := q.InsertAgent(ctx, sqlc.InsertAgentParams{
		ID: agentID, EnvID: envID, Name: host, Role: role, EngineID: req.Facts.EngineID, SwarmNodeID: nodeID,
		PublicKey: req.Pub, CertSerial: leaf.Serial, CertFingerprint: fingerprint, CertNotAfter: leaf.NotAfter.UnixNano(),
		AgentVersion: req.AgentVersion, CreatedAt: ts,
	}); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("insert the agent: %w", err)
	}
	// The first host sets the type, binds the host or the cluster and may name the
	// environment (D-11, D-23); a name the admin typed is never overwritten.
	if err := dec.apply(ctx, q, envID, ts); err != nil {
		return tunnel.EnrolResult{}, err
	}
	enrolled := map[string]string{"fingerprint": fingerprint, "node": host, "serial": leaf.Serial}
	switch verdict {
	case nodeListed:
		enrolled["node_check"] = "listed"
	case nodeUnverified:
		enrolled["node_check"] = "unverified"
	case nodeNotChecked:
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: rbac.System(enrolActor).AuditActor(), Action: audit.ActionAgentEnrolled,
		TargetType: targetAgent, TargetID: agentID, EnvID: envID, Result: audit.ResultSuccess,
		IP: req.IP, UserAgent: "tierplan-agent/" + req.AgentVersion,
		Details: enrolled,
	}); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("audit agent enrolled: %w", err)
	}
	replaced, err := s.retireReplaced(ctx, tx, q, envID, agentID, req, ts, verdict != nodeUnverified)
	if err != nil {
		return tunnel.EnrolResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return tunnel.EnrolResult{}, fmt.Errorf("commit: %w", err)
	}
	// An agent listed as unknown that has now enrolled with the same key is known.
	s.unknown.Forget(envID, KeyFingerprint(req.Pub))
	s.afterRevoke(envID, replaced...)
	return tunnel.EnrolResult{AgentID: agentID, EnvID: envID, Cert: leaf.DER, NotAfter: leaf.NotAfter}, nil
}

// retireReplaced revokes, inside the enrolment's transaction, every active agent of the
// environment that runs on the host that is enrolling: the same engine ID and the same
// swarm node (none for a standalone host). The host enrols again with a join token when its
// agent gave up its identity, as after an agent reinstalled with a new install command or an
// identity the server rejected for a cause the agent cannot tell (review 17 CR-02); the old
// row would otherwise stay active forever with no session, as an offline node whose
// certificate never renews. Each is revoked with the reason replaced and no block (the host
// is the one enrolling), under the enrolment's system actor, with one agent.revoked row
// naming the agent that replaced it. The engine ID is never empty here: CheckIdentity, in
// the same transaction, refuses an enrolment that reports none. It returns the IDs it
// revoked, for afterRevoke once the transaction has committed.
//
// An earlier agent that is connected now is never retired (review 17-2 WR-02, WR-03):
// in every case the retire is for (the agent set its identity aside after a refused
// hello, a reinstall stopped the old container, an expired certificate) the earlier
// agent is not connected. One that is connected is a second agent on the host (a
// leftover container next to a new one, a cloned VM), and retiring it would make the
// two replace each other every minute, forever; or it is a spoofed enrolment that
// claims a connected host's engine and node IDs to take over its routing. The
// enrolment is refused host_already_connected instead, and the caller's rollback leaves
// nothing of it.
//
// retire is false for a swarm enrolment whose node no fresh node list has shown
// (checkSwarmNode: unverified). It still gets the live check above, but retires no earlier
// row: the node ID is only the agent's own claim, and the reaper retires the older offline
// rows once a list shows the node (retireListedDuplicates).
func (s *Service) retireReplaced(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, envID, agentID string, req tunnel.EnrolRequest, ts int64, retire bool) ([]string, error) {
	var node string
	if sw := req.Facts.Swarm; sw != nil {
		node = sw.NodeID
	}
	olds, err := q.ActiveAgentsOfHost(ctx, sqlc.ActiveAgentsOfHostParams{EnvID: envID, EngineID: req.Facts.EngineID, SwarmNodeID: node})
	if err != nil {
		return nil, fmt.Errorf("look up the host's earlier agents: %w", err)
	}
	if s.live != nil {
		for _, old := range olds {
			if old.ID != agentID && s.live.Live(old.ID) {
				return nil, &tunnel.RefuseError{
					Code:        wire.RefuseHostAlreadyConnected,
					Detail:      "another agent of this host is connected to this environment now; remove the duplicate agent container or service from the host, or stop the other agent, and this one enrols on its next try",
					AuditReason: wire.RefuseHostAlreadyConnected,
				}
			}
		}
	}
	if !retire {
		return nil, nil
	}
	older := olds[:0:0]
	for _, old := range olds {
		if old.ID != agentID {
			older = append(older, old)
		}
	}
	return s.retireRows(ctx, tx, q, envID, rbac.System(enrolActor).AuditActor(), older, agentID, ts, req.IP, "tierplan-agent/"+req.AgentVersion, nil)
}

// retireRows revokes each of olds as replaced by replacedBy, in the caller's transaction, with
// no block and one agent.revoked row each. extra adds details to every row. It returns the IDs
// it revoked, for afterRevoke once the transaction has committed; a row that was revoked
// already is skipped.
func (s *Service) retireRows(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, envID string, actor audit.Actor, olds []sqlc.Agent, replacedBy string, ts int64, ip, ua string, extra map[string]string) ([]string, error) {
	var ids []string
	for _, old := range olds {
		changed, err := q.RevokeAgent(ctx, sqlc.RevokeAgentParams{
			RevokedAt: sql.NullInt64{Int64: ts, Valid: true}, RevokedBy: sql.NullString{String: actor.ID, Valid: true},
			RevokeReason: sql.NullString{String: replacedReason, Valid: true}, ID: old.ID, EnvID: envID,
		})
		if err != nil {
			return nil, fmt.Errorf("retire the host's earlier agent: %w", err)
		}
		if changed == 0 {
			continue
		}
		details := map[string]string{
			"node": old.Name, "fingerprint": old.CertFingerprint, "reason": replacedReason,
			"swarm_node": old.SwarmNodeID.String, "replaced_by": replacedBy,
		}
		maps.Copy(details, extra)
		if err := s.aud.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionAgentRevoked, TargetType: targetAgent, TargetID: old.ID,
			EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua, Details: details,
		}); err != nil {
			return nil, fmt.Errorf("audit agent revoked: %w", err)
		}
		ids = append(ids, old.ID)
	}
	return ids, nil
}

// retireListedDuplicates is the retire an unverified swarm enrolment left for later (the
// reaper calls it with each node list it believes). listed is the set of node IDs of that
// list. Among the active agents of the environment that run on a listed swarm node, those of
// one engine and node form a group; in a group of two or more the newest row stays and each
// older row that is not connected now is revoked as replaced, with replaced_by the newest and
// the detail source=node_list, under the reaper's actor. Without this an unverified
// enrolment's older offline row would stay the oldest active row of the node, and
// ActiveAgentOfSwarmNode would route node-local calls to a dead agent for good. A connected
// older row is left, as at enrolment. A node the list does not show is not touched here.
func (s *Service) retireListedDuplicates(ctx context.Context, envID string, listed map[string]struct{}) error {
	// Most lists find nothing to retire; look before taking the writer.
	rows, err := s.st.RQ.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return fmt.Errorf("list the agents: %w", err)
	}
	if len(s.duplicatesToRetire(rows, listed)) == 0 {
		return nil
	}

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	// The writer is single, so this read sees every revoke committed before it.
	rows, err = q.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return fmt.Errorf("list the agents: %w", err)
	}
	actor := rbac.System(reaperActor).AuditActor()
	ts := s.now().UTC().UnixNano()
	var retired []string
	for _, d := range s.duplicatesToRetire(rows, listed) {
		ids, err := s.retireRows(ctx, tx, q, envID, actor, []sqlc.Agent{d.old}, d.newest, ts, "", "", map[string]string{"source": "node_list"})
		if err != nil {
			return err
		}
		retired = append(retired, ids...)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.afterRevoke(envID, retired...)
	return nil
}

// duplicate is an older row of a swarm node that has a newer active row.
type duplicate struct {
	old    sqlc.Agent
	newest string
}

// duplicatesToRetire is the older, not connected, active rows of the listed nodes that have a
// newer active row of the same engine and node. rows are in created_at, id order.
func (s *Service) duplicatesToRetire(rows []sqlc.Agent, listed map[string]struct{}) []duplicate {
	type key struct{ engine, node string }
	groups := map[key][]sqlc.Agent{}
	var order []key
	for _, a := range rows {
		if a.Status != statusActive || !a.SwarmNodeID.Valid || a.SwarmNodeID.String == "" {
			continue
		}
		if _, ok := listed[a.SwarmNodeID.String]; !ok {
			continue
		}
		k := key{a.EngineID, a.SwarmNodeID.String}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], a)
	}
	var out []duplicate
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		newest := g[len(g)-1].ID
		for _, old := range g[:len(g)-1] {
			if s.live != nil && s.live.Live(old.ID) {
				continue
			}
			out = append(out, duplicate{old: old, newest: newest})
		}
	}
	return out
}

// afterRevoke is what follows the commit of a revoke of agentIDs in envID that makes no
// block: their sessions are closed, as revoked, and the environment's health is
// recomputed once. Nothing happens for no agents.
func (s *Service) afterRevoke(envID string, agentIDs ...string) {
	if len(agentIDs) == 0 {
		return
	}
	s.seenMu.Lock()
	for _, id := range agentIDs {
		delete(s.seen, id)
	}
	s.seenMu.Unlock()
	// After the commit, so the hub's check of the agent's status sees the revoke.
	if s.kicker != nil {
		for _, id := range agentIDs {
			s.kicker.Kick(id, tunnel.DisconnectRevoked)
		}
	}
	if s.notifier != nil {
		s.notifier.Notify(envID)
	}
}

// identityRefused is what Enrol and Hello return for a failed identity check. A
// manager of another cluster is recorded on the environment first, once, so the
// overview shows cluster_changed with the cluster to adopt; the refusal is
// returned either way. If recording fails the fault is returned instead and the
// agent retries.
func (s *Service) identityRefused(ctx context.Context, envID string, dec Decision, refusal error) error {
	if !dec.RecordChange {
		return refusal
	}
	ts := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	rows, err := s.st.Q.WithTx(tx).RecordClusterChange(ctx, sqlc.RecordClusterChangeParams{
		ClusterChangedAt: sql.NullInt64{Int64: ts, Valid: true}, PendingClusterID: sql.NullString{String: dec.PendingCluster, Valid: true},
		UpdatedAt: ts, ID: envID,
	})
	if err != nil {
		return fmt.Errorf("record the cluster change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if rows > 0 && s.notifier != nil {
		s.notifier.Notify(envID)
	}
	return refusal
}

// Revoke makes an agent unwelcome for good: its row becomes revoked, the engine ID
// and, on a swarm, the swarm node ID of its host are blocked so the host cannot
// enrol again with a join token that is still valid, and agent.revoked and
// agent.blocked are audited, all in one transaction. Once that has committed the
// agent's live session is closed. It never changes anything on the host: removing
// the agent from it is the admin's to do. reason is optional, at most 256
// characters, and is shown in the audit log.
//
// A revoke of an agent that is already revoked, by this admin or another, is
// ErrAlreadyRevoked and changes nothing.
func (s *Service) Revoke(ctx context.Context, actor audit.Actor, envID, agentID, reason, ip, ua string) error {
	reason = cleanReason(reason)
	ts := s.now().UTC().UnixNano()

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := agentOf(ctx, q, envID, agentID)
	if err != nil {
		return err
	}
	changed, err := q.RevokeAgent(ctx, sqlc.RevokeAgentParams{
		RevokedAt: sql.NullInt64{Int64: ts, Valid: true}, RevokedBy: sql.NullString{String: actor.ID, Valid: true},
		RevokeReason: sql.NullString{String: reason, Valid: reason != ""}, ID: agentID, EnvID: envID,
	})
	if err != nil {
		return fmt.Errorf("revoke the agent: %w", err)
	}
	if changed == 0 {
		return ErrAlreadyRevoked
	}
	details := map[string]string{"node": row.Name, "fingerprint": row.CertFingerprint}
	if reason != "" {
		details["reason"] = reason
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionAgentRevoked, TargetType: targetAgent, TargetID: agentID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua, Details: details,
	}); err != nil {
		return fmt.Errorf("audit agent revoked: %w", err)
	}
	for _, b := range identitiesOf(row) {
		if err := q.InsertBlock(ctx, sqlc.InsertBlockParams{
			EnvID: envID, Kind: b.kind, Identity: b.identity, AgentID: sql.NullString{String: agentID, Valid: true},
			CreatedAt: ts, CreatedBy: actor.ID,
		}); err != nil {
			return fmt.Errorf("block the %s: %w", b.kind, err)
		}
		if err := s.aud.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionAgentBlocked, TargetType: targetAgent, TargetID: agentID,
			EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
			Details: map[string]string{"node": row.Name, "kind": b.kind, "identity": b.identity},
		}); err != nil {
			return fmt.Errorf("audit agent blocked: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.seenMu.Lock()
	delete(s.seen, agentID)
	s.seenMu.Unlock()
	// After the commit, so the hub's check of the agent's status sees the revoke.
	if s.kicker != nil {
		s.kicker.Kick(agentID, tunnel.DisconnectRevoked)
	}
	return nil
}

// AutoRevoke revokes an agent whose node has left its swarm (D-10), as the reaper
// decides. It is Revoke with three differences: the actor is system:agent-reaper, the
// action is agent.auto_revoked, and no block is made, so the host can enrol again
// with the join token the moment it rejoins the swarm. Once the change has committed
// the agent's live session is closed and the environment's health is recomputed.
//
// An agent that is already revoked is ErrAlreadyRevoked and nothing changes.
func (s *Service) AutoRevoke(ctx context.Context, envID, agentID string) error {
	return s.revokeKeepingHost(ctx, rbac.System(reaperActor).AuditActor(), envID, agentID, leftSwarmReason, audit.ActionAgentAutoRevoked, "", "")
}

// RevokeRemovedNode revokes the agent of a node an administrator has just removed from its
// swarm (D-22). It is AutoRevoke done at once, on the administrator's word: the actor is the
// administrator, the action is agent.revoked, the reason is node_removed, and no block is made,
// so a host that rejoins the swarm can enrol again with the join token. Once the change has
// committed the agent's live session is closed and the environment's health is recomputed. It
// never changes anything on the host.
//
// An agent that is already revoked is ErrAlreadyRevoked and nothing changes.
func (s *Service) RevokeRemovedNode(ctx context.Context, actor audit.Actor, envID, agentID, ip, ua string) error {
	return s.revokeKeepingHost(ctx, actor, envID, agentID, removedNodeReason, audit.ActionAgentRevoked, ip, ua)
}

// revokeKeepingHost is the revoke that makes no block, for an agent whose node has gone from its
// swarm: its row becomes revoked and one audit row says why, in one transaction. After the commit
// the agent's session is closed and the environment is told.
func (s *Service) revokeKeepingHost(ctx context.Context, actor audit.Actor, envID, agentID, reason, action, ip, ua string) error {
	ts := s.now().UTC().UnixNano()

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := agentOf(ctx, q, envID, agentID)
	if err != nil {
		return err
	}
	changed, err := q.RevokeAgent(ctx, sqlc.RevokeAgentParams{
		RevokedAt: sql.NullInt64{Int64: ts, Valid: true}, RevokedBy: sql.NullString{String: actor.ID, Valid: true},
		RevokeReason: sql.NullString{String: reason, Valid: true}, ID: agentID, EnvID: envID,
	})
	if err != nil {
		return fmt.Errorf("revoke the agent: %w", err)
	}
	if changed == 0 {
		return ErrAlreadyRevoked
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: action, TargetType: targetAgent, TargetID: agentID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{
			"node": row.Name, "fingerprint": row.CertFingerprint, "reason": reason, "swarm_node": row.SwarmNodeID.String,
		},
	}); err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.afterRevoke(envID, agentID)
	return nil
}

// ActiveAgentOfSwarmNode is the ID of the active agent of the environment that runs on the swarm
// node nodeID, or "" when no active agent does (a node with no agent, an agent already revoked,
// an empty ID). It reads the records, not the live sessions, so the agent of a node that is down
// and disconnected is found.
func (s *Service) ActiveAgentOfSwarmNode(ctx context.Context, envID, nodeID string) (string, error) {
	if nodeID == "" {
		return "", nil
	}
	rows, err := s.st.RQ.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return "", fmt.Errorf("list the agents: %w", err)
	}
	for _, a := range rows {
		if a.Status == statusActive && a.SwarmNodeID.Valid && a.SwarmNodeID.String == nodeID {
			return a.ID, nil
		}
	}
	return "", nil
}

// Unblock lifts the blocks a revoke of agentID made, so the host can enrol again
// with the environment's join token, and audits agent.unblocked for each. The
// revoked agent stays revoked and cannot come back: the host enrols as a new
// agent. ErrNotBlocked means the agent has no block to lift.
func (s *Service) Unblock(ctx context.Context, actor audit.Actor, envID, agentID, ip, ua string) error {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := agentOf(ctx, q, envID, agentID)
	if err != nil {
		return err
	}
	blocks, err := q.ListBlocksForAgent(ctx, sqlc.ListBlocksForAgentParams{EnvID: envID, AgentID: sql.NullString{String: agentID, Valid: true}})
	if err != nil {
		return fmt.Errorf("list the blocks: %w", err)
	}
	if len(blocks) == 0 {
		return ErrNotBlocked
	}
	if _, err := q.DeleteBlocksForAgent(ctx, sqlc.DeleteBlocksForAgentParams{EnvID: envID, AgentID: sql.NullString{String: agentID, Valid: true}}); err != nil {
		return fmt.Errorf("delete the blocks: %w", err)
	}
	for _, b := range blocks {
		if err := s.aud.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionAgentUnblocked, TargetType: targetAgent, TargetID: agentID,
			EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
			Details: map[string]string{"node": row.Name, "kind": b.Kind, "identity": b.Identity},
		}); err != nil {
			return fmt.Errorf("audit agent unblocked: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Adopted is a cluster change an admin accepted.
type Adopted struct {
	// From is the cluster the environment was bound to; empty if it had none.
	From string
	// To is the cluster it is bound to now.
	To string
}

// AdoptCluster is the audited admin action that binds an environment to the cluster
// its managers now report, after the swarm behind it was re-initialised (D-14). It is
// the only way a swarm environment is ever rebound: the handshake only records the
// change. It binds the pending cluster, clears the change and audits
// environment.cluster_adopted with the cluster it was and the one it is, in one
// transaction, and then tells the broker so every overview updates at once.
//
// ErrNoClusterChange means there is nothing recorded to adopt, for example because
// another admin just did. ErrClusterInUse means another environment has bound the
// cluster since.
func (s *Service) AdoptCluster(ctx context.Context, actor audit.Actor, envID, ip, ua string) (Adopted, error) {
	ts := s.now().UTC().UnixNano()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Adopted{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := requireAgentEnvironment(ctx, q, envID); err != nil {
		return Adopted{}, err
	}
	env, err := q.GetEnvironment(ctx, envID)
	if err != nil {
		return Adopted{}, fmt.Errorf("load the environment: %w", err)
	}
	if !env.ClusterChangedAt.Valid || env.PendingClusterID.String == "" {
		return Adopted{}, ErrNoClusterChange
	}
	pending := env.PendingClusterID.String
	// A cluster is one environment's. The unique index would refuse the update too;
	// this says why.
	if _, err := q.OtherEnvironmentWithCluster(ctx, sqlc.OtherEnvironmentWithClusterParams{
		EnvID: envID, ClusterID: sql.NullString{String: pending, Valid: true},
	}); err == nil {
		return Adopted{}, ErrClusterInUse
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Adopted{}, fmt.Errorf("look up the cluster: %w", err)
	}
	rows, err := q.AdoptPendingCluster(ctx, sqlc.AdoptPendingClusterParams{UpdatedAt: ts, ID: envID})
	if err != nil {
		return Adopted{}, fmt.Errorf("adopt the cluster: %w", err)
	}
	if rows == 0 {
		return Adopted{}, ErrNoClusterChange
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentClusterAdopted, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"from": env.SwarmClusterID.String, "to": pending},
	}); err != nil {
		return Adopted{}, fmt.Errorf("audit cluster adopted: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Adopted{}, fmt.Errorf("commit: %w", err)
	}
	if s.notifier != nil {
		s.notifier.Notify(envID)
	}
	return Adopted{From: env.SwarmClusterID.String, To: pending}, nil
}

// Node is one enrolled agent as the database knows it: what the node list shows
// before live state is added.
type Node struct {
	ID string
	// Name is the hostname the agent reported when it enrolled.
	Name string
	// Role is manager, worker or standalone.
	Role string
	// Status is active or revoked.
	Status string
	// Blocked is true while the host's identity is on the block list because of
	// this agent's revoke.
	Blocked         bool
	AgentVersion    string
	CertFingerprint string
	CertNotAfter    time.Time
	// LastSeen is when the agent was last heard from, as far as the database
	// knows (it is written at most once a minute); zero if never.
	LastSeen time.Time
	// RevokedAt and RevokeReason are set once the agent is revoked. RevokeReason may
	// be empty.
	RevokedAt    time.Time
	RevokeReason string
}

// ListNodes returns every agent enrolled in an environment, oldest first, with
// whether each is blocked. An environment with no agents, or one that is not an
// agent environment, has none.
func (s *Service) ListNodes(ctx context.Context, envID string) ([]Node, error) {
	rows, err := s.st.RQ.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the agents: %w", err)
	}
	blocks, err := s.st.RQ.ListBlocks(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the blocks: %w", err)
	}
	blocked := make(map[string]bool, len(blocks))
	for _, b := range blocks {
		if b.AgentID.Valid {
			blocked[b.AgentID.String] = true
		}
	}
	out := make([]Node, 0, len(rows))
	for _, a := range rows {
		n := Node{
			ID: a.ID, Name: a.Name, Role: a.Role, Status: a.Status, Blocked: blocked[a.ID],
			AgentVersion: a.AgentVersion, CertFingerprint: a.CertFingerprint, CertNotAfter: time.Unix(0, a.CertNotAfter).UTC(),
			RevokeReason: a.RevokeReason.String,
		}
		if a.LastSeenAt.Valid {
			n.LastSeen = time.Unix(0, a.LastSeenAt.Int64).UTC()
		}
		if a.RevokedAt.Valid {
			n.RevokedAt = time.Unix(0, a.RevokedAt.Int64).UTC()
		}
		out = append(out, n)
	}
	return out, nil
}

// renewEligibleBelow is how little life a certificate must have left before the
// server renews it: half of a full life. An agent asks at two thirds of the life, so
// at a third left, and anything much earlier is a fault or a flood. Every renewal is
// a certificate signed and a write on the single database writer.
const renewEligibleBelow = leafLifetime / 2

// Renew issues a new certificate for the same key to an agent that asked for one
// over its live session (D-04, RESEARCH Pattern 3). It implements tunnel.Verifier.
//
// The status is read again inside the transaction that records the new certificate,
// and the update itself only changes an active agent, so a revoke that commits first
// always wins and no certificate is recorded for a revoked agent (T-02-83). The key
// is the one stored at enrolment: a renewal never rotates it. A certificate that is
// still good for more than half a life is refused with wire.RenewNotDue and nothing
// is written. The agent's old certificate stays valid until it expires, because
// authentication is by key, so an agent that crashes before saving the new one
// loses nothing.
func (s *Service) Renew(ctx context.Context, agentID, ip string) (tunnel.RenewResult, error) {
	revoked := &tunnel.Refusal{Code: wire.ReasonRevoked, Detail: "an administrator revoked this agent", AuditReason: wire.ReasonRevoked}
	notDue := &tunnel.Refusal{Code: wire.RenewNotDue, Detail: "the certificate has more than half of its life left", AuditReason: wire.RenewNotDue}

	row, err := s.st.RQ.GetAgent(ctx, agentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tunnel.RenewResult{}, &tunnel.Refusal{Code: wire.ReasonAuthRejected, Detail: "no such agent", AuditReason: "unknown_agent"}
	case err != nil:
		return tunnel.RenewResult{}, fmt.Errorf("load the agent: %w", err)
	case row.Status != statusActive:
		return tunnel.RenewResult{}, revoked
	}
	now := s.now().UTC()
	// The cheap refusal comes before the CA is opened and a certificate is signed.
	if time.Unix(0, row.CertNotAfter).Sub(now) > renewEligibleBelow {
		return tunnel.RenewResult{}, notDue
	}
	if len(row.PublicKey) != ed25519.PublicKeySize {
		return tunnel.RenewResult{}, fmt.Errorf("the stored key of agent %s is not an Ed25519 key", agentID)
	}
	caRow, err := s.st.RQ.GetCA(ctx, row.EnvID)
	if err != nil {
		return tunnel.RenewResult{}, fmt.Errorf("load the CA: %w", err)
	}
	authority, err := loadCA(s.v, row.EnvID, caRow.CertPem, caRow.KeySealed)
	if err != nil {
		return tunnel.RenewResult{}, err
	}
	leaf, err := authority.issue(agentID, row.EnvID, ed25519.PublicKey(row.PublicKey), now)
	if err != nil {
		return tunnel.RenewResult{}, err
	}
	fp := sha256.Sum256(leaf.DER)
	fingerprint := hex.EncodeToString(fp[:])

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return tunnel.RenewResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	// Read again under the single writer: a revoke, or another renewal, may have
	// committed since the read above.
	cur, err := q.GetAgent(ctx, agentID)
	switch {
	case err != nil:
		return tunnel.RenewResult{}, fmt.Errorf("load the agent: %w", err)
	case cur.Status != statusActive:
		return tunnel.RenewResult{}, revoked
	case time.Unix(0, cur.CertNotAfter).Sub(now) > renewEligibleBelow:
		return tunnel.RenewResult{}, notDue
	}
	changed, err := q.RenewAgentCert(ctx, sqlc.RenewAgentCertParams{
		CertSerial: leaf.Serial, CertFingerprint: fingerprint, CertNotAfter: leaf.NotAfter.UnixNano(), ID: agentID,
	})
	if err != nil {
		return tunnel.RenewResult{}, fmt.Errorf("record the renewed certificate: %w", err)
	}
	if changed == 0 {
		return tunnel.RenewResult{}, revoked
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: rbac.System(enrolActor).AuditActor(), Action: audit.ActionAgentCertRenewed,
		TargetType: targetAgent, TargetID: agentID, EnvID: row.EnvID, Result: audit.ResultSuccess,
		IP: ip, UserAgent: "tierplan-agent/" + cur.AgentVersion,
		Details: map[string]string{
			"fingerprint": fingerprint, "node": cur.Name, "serial": leaf.Serial,
			"not_after": leaf.NotAfter.Format(time.RFC3339), "previous_serial": cur.CertSerial,
		},
	}); err != nil {
		return tunnel.RenewResult{}, fmt.Errorf("audit certificate renewed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return tunnel.RenewResult{}, fmt.Errorf("commit: %w", err)
	}
	return tunnel.RenewResult{Cert: leaf.DER, NotAfter: leaf.NotAfter}, nil
}

// EnrolledAgentIDs is the IDs of the agents that may connect: the active ones, in
// every environment. Settings counts them to say how many agents a change of
// address concerns and how many of those are offline (D-19).
func (s *Service) EnrolledAgentIDs(ctx context.Context) ([]string, error) {
	ids, err := s.st.RQ.ListActiveAgentIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the enrolled agents: %w", err)
	}
	return ids, nil
}

// StillActive reports whether the agent exists and has not been revoked. The hub
// asks it when it registers a session. It implements tunnel.Verifier.
func (s *Service) StillActive(agentID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), stillActiveTimeout)
	defer cancel()
	row, err := s.st.RQ.GetAgent(ctx, agentID)
	return err == nil && row.Status == statusActive
}

// agentOf loads an agent through its environment: an unknown environment is
// ErrEnvironmentNotFound, and an agent that does not exist or belongs to another
// environment is ErrAgentNotFound.
func agentOf(ctx context.Context, q *sqlc.Queries, envID, agentID string) (sqlc.Agent, error) {
	if err := requireAgentEnvironment(ctx, q, envID); err != nil {
		return sqlc.Agent{}, err
	}
	row, err := q.GetAgent(ctx, agentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sqlc.Agent{}, ErrAgentNotFound
	case err != nil:
		return sqlc.Agent{}, fmt.Errorf("load the agent: %w", err)
	case row.EnvID != envID:
		return sqlc.Agent{}, ErrAgentNotFound
	}
	return row, nil
}

type identity struct{ kind, identity string }

// identitiesOf is what a revoke blocks for an agent: its engine ID and, when it
// was a swarm node, its swarm node ID. An empty value is not an identity.
func identitiesOf(a sqlc.Agent) []identity {
	var out []identity
	if a.EngineID != "" {
		out = append(out, identity{blockEngineID, a.EngineID})
	}
	if a.SwarmNodeID.Valid && a.SwarmNodeID.String != "" {
		out = append(out, identity{blockSwarmNodeID, a.SwarmNodeID.String})
	}
	return out
}

// checkBlocked refuses a host whose engine ID or swarm node ID an admin blocked.
func (s *Service) checkBlocked(ctx context.Context, q *sqlc.Queries, envID string, f wire.Facts) error {
	var ids []identity
	if f.EngineID != "" {
		ids = append(ids, identity{blockEngineID, f.EngineID})
	}
	if f.Swarm != nil && f.Swarm.NodeID != "" {
		ids = append(ids, identity{blockSwarmNodeID, f.Swarm.NodeID})
	}
	for _, id := range ids {
		n, err := q.IsBlocked(ctx, sqlc.IsBlockedParams{EnvID: envID, Kind: id.kind, Identity: id.identity})
		if err != nil {
			return fmt.Errorf("look up the block list: %w", err)
		}
		if n > 0 {
			return &tunnel.RefuseError{
				Code:        wire.ReasonBlocked,
				Detail:      "an administrator removed this host from the environment; it must be unblocked in TierPlan before it can enrol again",
				AuditReason: "blocked_" + id.kind,
			}
		}
	}
	return nil
}

// cleanReason makes the reason an admin typed fit the audit log: no control
// characters, valid UTF-8, at most maxRevokeReason characters.
func cleanReason(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > maxRevokeReason {
		s = string([]rune(s)[:maxRevokeReason])
	}
	return s
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
// auth_rejected with the same text, whatever dates the certificate carries. Only an
// agent that holds a certificate the environment really issued can learn that it
// expired or was revoked.
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
	// environment's CA both end here with the same answer, and so does a forged
	// certificate for a real environment whatever its dates.
	caRow, err := s.st.RQ.GetCA(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tunnel.HelloResult{}, reject("unknown_environment")
	case err != nil:
		return tunnel.HelloResult{}, fmt.Errorf("load the CA: %w", err)
	}
	// VerifyLeaf proves the chain before it looks at the dates, so only a
	// certificate this environment's CA signed can be told it expired. Whatever
	// its dates, anything else is the same auth_rejected as an unknown
	// environment (T-02-21).
	if _, err := VerifyLeaf(caRow.CertPem, req.Cert, s.now()); err != nil {
		switch {
		case errors.Is(err, ErrLeafExpired):
			return tunnel.HelloResult{}, &tunnel.Refusal{
				Code:        wire.ReasonCertExpired,
				Detail:      "the agent's certificate has expired; enrol the agent again with a join token",
				AuditReason: wire.ReasonCertExpired,
			}
		case errors.Is(err, ErrLeafNotYetValid):
			return tunnel.HelloResult{}, reject("not_yet_valid")
		}
		return tunnel.HelloResult{}, reject("bad_chain")
	}

	row, err := s.st.RQ.GetAgent(ctx, agentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The certificate is this environment's own and the agent is not in the
		// database: it was enrolled after the backup this server was restored from
		// (D-31). Only a holder of the certificate's key is told so, and only that
		// holder is listed for an administrator; anyone else gets the uniform
		// auth_rejected (T-03-115).
		if !authn.Verify(pub, req.Transcript, req.Sig) {
			return tunnel.HelloResult{}, reject("bad_signature")
		}
		s.unknown.Record(UnknownAgent{
			EnvID: envID, Node: cleanName(agentID), IP: req.IP, LastAttempt: s.now().UTC(), Key: KeyFingerprint(pub),
		})
		return tunnel.HelloResult{}, &tunnel.Refusal{
			Code:        wire.ReasonUnknownAgent,
			Detail:      "this agent was enrolled after the backup this TierPlan was restored from",
			AuditReason: wire.ReasonUnknownAgent,
		}
	case err != nil:
		return tunnel.HelloResult{}, fmt.Errorf("load the agent: %w", err)
	}
	if row.EnvID != envID {
		return tunnel.HelloResult{}, reject("environment_mismatch")
	}
	if row.Status != statusActive {
		detail := "an administrator revoked this agent"
		if row.RevokeReason.String == replacedReason {
			detail = "this host enrolled again as a new agent, which replaced this one"
		}
		return tunnel.HelloResult{}, &tunnel.Refusal{
			Code:        wire.ReasonRevoked,
			Detail:      detail,
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
	// A manager that now reports another cluster is refused and the change is
	// recorded for an admin to adopt (D-14).
	if dec, err := CheckCluster(ctx, s.st.RQ, envID, req.Facts); err != nil {
		return tunnel.HelloResult{}, s.identityRefused(ctx, envID, dec, err)
	}
	return tunnel.HelloResult{AgentID: agentID, EnvID: envID, NotAfter: leaf.NotAfter}, nil
}

// newToken makes a join token tpjt_<envId>_<base64url of 32 random bytes> and
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
