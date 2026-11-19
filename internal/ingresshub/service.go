// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// secretBytes is the entropy of a join token: 256 bits.
	secretBytes = 32
	// maxNameLen caps the name a node reports, which is untrusted input.
	maxNameLen = ingresswire.MaxNameLen

	statusActive  = "active"
	statusRevoked = "revoked"

	targetIngressNode = "ingress_node"
	targetEnvironment = "environment"

	// notReadyRetryAfter is how long a node is told to wait when the server cannot yet confirm it: an agent
	// environment that has not reported its type, or a swarm with no fresh node list.
	notReadyRetryAfter = 15 * time.Second

	// Kinds and types of an environment, as the environments table stores them.
	kindAgent         = "agent"
	kindLocal         = "local"
	envTypeSwarm      = "swarm"
	envTypeStandalone = "standalone"

	// userAgentPrefix is the user agent an enrolment is audited under.
	userAgentPrefix = "tierplan-ingress/"
)

// Service is the ingress node service: the join token of each environment and the enrolment of nodes with it.
// It is a structural copy of agents.Service with its own token kind and table.
type Service struct {
	st    *store.Store
	aud   *audit.Writer
	now   func() time.Time
	nodes agents.NodeLists
}

// NewService returns the service. now defaults to time.Now.
func NewService(st *store.Store, w *audit.Writer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, aud: w, now: now}
}

// WithNodeLists tells the service where the node lists of swarm environments are, so an enrolment checks the
// swarm node a node reports against the list of a manager (envs.Clusters in production). Without one a swarm
// node cannot be confirmed and is refused node_unverified. Call it before the service is used.
func (s *Service) WithNodeLists(n agents.NodeLists) *Service {
	s.nodes = n
	return s
}

// EnrolRequest is the first connection of a node. Transcript is the digest the hub rebuilt from its own nonce,
// the node's nonce, the audience, the key and the channel binding; the service never builds or trusts one from
// the node, and never sees or computes the channel binding, so the rule for it lives in one place (the hub).
type EnrolRequest struct {
	// Token is the join token the node presented.
	Token string
	// Pub is the Ed25519 public key the node generated, Sig its signature over Transcript.
	Pub, Sig, Transcript []byte
	// Name is the node's own name for itself, SwarmNodeID the swarm node it says it runs on, Version its
	// supervisor version and IP the address it connected from. All but the last are untrusted.
	Name, SwarmNodeID, Version, IP string
}

// EnrolResult is a node that has enrolled.
type EnrolResult struct {
	NodeID, EnvID, Name, SwarmNodeID string
}

// Enrol turns a join token and a proof of key possession into a registered node. Every way of being refused
// for the token looks the same to the node (auth_rejected), so a probe learns neither which environments
// exist nor whether a token was once valid; the audit reason says which it was.
func (s *Service) Enrol(ctx context.Context, req EnrolRequest) (EnrolResult, error) {
	reject := func(reason string) error {
		return &RefuseError{
			Code: ingresswire.RefuseAuthRejected, Detail: "the join token was not accepted", AuditReason: reason,
		}
	}

	// (1) The token has the ingress shape. An agent token given to this channel is named, since the person
	// holding it has only picked the wrong command.
	envID, secret, ok := ParseToken(req.Token)
	if !ok {
		if strings.HasPrefix(req.Token, ingresswire.AgentTokenPrefix) {
			return EnrolResult{}, &RefuseError{
				Code:        ingresswire.RefuseWrongTokenKind,
				Detail:      "this is an agent join token; an ingress node needs the ingress join token of its environment",
				AuditReason: ingresswire.RefuseWrongTokenKind,
			}
		}
		return EnrolResult{}, reject("malformed")
	}

	// (2) The hash is compared against every token the environment ever had, whether or not any exists, so the
	// time taken does not say which one matched. Only the audit learns whether a token that matched was
	// replaced or disabled; the node is told the same thing for all of them.
	tokens, err := s.st.RQ.ListIngressJoinTokens(ctx, envID)
	if err != nil {
		return EnrolResult{}, fmt.Errorf("load the join tokens: %w", err)
	}
	got := sha256.Sum256([]byte(secret))
	var matched *sqlc.IngressJoinToken
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
		return EnrolResult{}, reject("bad_token")
	case matched.SupersededAt != 0:
		return EnrolResult{}, reject("token_superseded")
	case matched.DisabledAt != 0:
		return EnrolResult{}, reject("token_disabled")
	}

	// (3) Proof of possession: the node signed the transcript the hub built with the key it wants to enrol.
	if !authn.Verify(req.Pub, req.Transcript, req.Sig) {
		return EnrolResult{}, reject("bad_proof_of_possession")
	}

	// (4) Shape. The name and the swarm node ID are claims of a join-token holder and reach the database and
	// the audit log, so they are cleaned or refused here.
	name := cleanName(req.Name)
	if req.SwarmNodeID != "" && !ingresswire.ValidIdentity(req.SwarmNodeID) {
		return EnrolResult{}, reject("malformed")
	}

	// (5) Topology: what kind of environment the token belongs to decides whether the node is a swarm node.
	topo, err := s.st.RQ.IngressEnvironmentTopology(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrolResult{}, reject("bad_token")
	} else if err != nil {
		return EnrolResult{}, fmt.Errorf("look up the environment: %w", err)
	}
	var swarm bool
	switch {
	case topo.Kind == kindAgent && topo.EnvType.Valid && topo.EnvType.String == envTypeSwarm:
		swarm = true
	case topo.Kind == kindAgent && topo.EnvType.Valid && topo.EnvType.String == envTypeStandalone:
	case topo.Kind == kindLocal:
		// Not retryable: no wait makes it succeed in this version, since the local-socket variant of ingress
		// is a later phase's decision.
		return EnrolResult{}, &RefuseError{
			Code:        ingresswire.RefuseAuthRejected,
			Detail:      "ingress nodes cannot enrol into TierPlan's local-socket environment",
			AuditReason: "environment_kind_local",
		}
	default:
		// An agent environment whose agent has not reported its type yet: this passes once it has.
		return EnrolResult{}, &RefuseError{
			Code:        ingresswire.RefuseNodeUnverified,
			Detail:      "TierPlan does not yet know whether this environment is a swarm or a standalone host; try again shortly",
			AuditReason: "environment_type_unknown", RetryAfter: notReadyRetryAfter,
		}
	}

	// (6) A swarm node must be in the node list a manager last gave. The node ID is only the node's own claim,
	// so no fresh list that lists it is no enrolment (D-22), where the agent would accept it.
	nodeID := sql.NullString{}
	if swarm {
		switch agents.CheckSwarmNode(ctx, s.nodes, s.now, envID, req.SwarmNodeID) {
		case agents.NodeListed:
			nodeID = sql.NullString{String: req.SwarmNodeID, Valid: true}
		case agents.NodeNotListed:
			return EnrolResult{}, &RefuseError{
				Code: ingresswire.RefuseNodeNotInSwarm, AuditReason: ingresswire.RefuseNodeNotInSwarm,
				Detail: "this node is not in the swarm's node list as TierPlan last read it; a node that just joined enrols by itself once TierPlan reads the list again, within about a minute",
			}
		default:
			return EnrolResult{}, &RefuseError{
				Code: ingresswire.RefuseNodeUnverified, AuditReason: ingresswire.RefuseNodeUnverified,
				Detail:     "TierPlan cannot yet confirm this node against the swarm's node list; try again shortly",
				RetryAfter: notReadyRetryAfter,
			}
		}
	}

	// (7) One write transaction: the node row and its audit row, or neither.
	nodeUUID, err := uuid.NewV7()
	if err != nil {
		return EnrolResult{}, fmt.Errorf("generate node id: %w", err)
	}
	id := nodeUUID.String()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return EnrolResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if err := q.InsertIngressNode(ctx, sqlc.InsertIngressNodeParams{
		ID: id, EnvID: envID, Name: name, SwarmNodeID: nodeID, PublicKey: req.Pub,
		Status: statusActive, CreatedAt: s.now().UTC().UnixNano(),
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return EnrolResult{}, &RefuseError{
				Code: ingresswire.RefuseNodeAlreadyEnrolled, AuditReason: ingresswire.RefuseNodeAlreadyEnrolled,
				Detail: "this node already has an ingress node record; revoke it in TierPlan before it enrols again",
			}
		}
		return EnrolResult{}, fmt.Errorf("insert the ingress node: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: audit.SystemIngress, Action: audit.ActionIngressNodeEnrolled,
		TargetType: targetIngressNode, TargetID: id, EnvID: envID, Result: audit.ResultSuccess,
		IP: req.IP, UserAgent: userAgentPrefix + req.Version,
		Details: map[string]string{"node": name, "swarm_node": nodeID.String},
	}); err != nil {
		return EnrolResult{}, fmt.Errorf("audit ingress node enrolled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return EnrolResult{}, fmt.Errorf("commit: %w", err)
	}
	return EnrolResult{NodeID: id, EnvID: envID, Name: name, SwarmNodeID: nodeID.String}, nil
}

// cleanName cuts a name a node reported to the stored length. The node is not trusted to send something short
// or printable.
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
	return s
}
