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
	"sync"
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

	// nodeUnverifiedRetryAfter is how long a node is told to wait when the server cannot yet confirm it: an
	// agent environment that has not reported its type, or a swarm with no fresh node list, or a node the list
	// does not hold yet.
	nodeUnverifiedRetryAfter = 15 * time.Second

	// detailTokenNotAccepted is the one outward detail of every refusal of the token or the proof of
	// possession, so a probe learns neither which environments exist nor whether a token was once valid. The
	// audit reason says which it was (D-27, D-35).
	detailTokenNotAccepted = "the ingress join token was not accepted"
	// detailWrongTokenKind names both token kinds, since the person holding an agent token has only picked the
	// wrong command.
	detailWrongTokenKind = "this is an agent join token; an ingress node needs the environment's ingress join token"
	// detailNodeAlreadyEnrolled and detailNodeRevoked tell the holder of a valid token what to ask an
	// administrator for (D-05, D-20).
	detailNodeAlreadyEnrolled = "this node is already enrolled; an administrator must revoke the old enrolment first"
	detailNodeRevoked         = "an administrator revoked this node; it can enrol again only after an administrator allows it"

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
	st     *store.Store
	aud    *audit.Writer
	now    func() time.Time
	nodes  agents.NodeLists
	kicker Kicker

	// seen is when each node's last_seen_at was last written, so a heartbeat costs the single writer at most
	// one write a minute per node (TouchLastSeen).
	seenMu sync.Mutex
	seen   map[string]time.Time
	// healthAt is when each node's health was last written, under seenMu, so a node that flips its health costs
	// at most one write per healthWriteEvery (RecordHealth).
	healthAt map[string]time.Time
}

// NewService returns the service. now defaults to time.Now.
func NewService(st *store.Store, w *audit.Writer, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{st: st, aud: w, now: now, seen: map[string]time.Time{}, healthAt: map[string]time.Time{}}
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
			Code: ingresswire.RefuseAuthRejected, Detail: detailTokenNotAccepted, AuditReason: reason,
		}
	}

	// (1) The token has the ingress shape. An agent token given to this channel is named, since the person
	// holding it has only picked the wrong command.
	envID, secret, ok := ParseToken(req.Token)
	if !ok {
		if strings.HasPrefix(req.Token, ingresswire.AgentTokenPrefix) {
			return EnrolResult{}, &RefuseError{
				Code:        ingresswire.RefuseWrongTokenKind,
				Detail:      detailWrongTokenKind,
				AuditReason: ingresswire.RefuseWrongTokenKind,
			}
		}
		return EnrolResult{}, reject("bad_token")
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
	case topo.Kind == kindLocal && req.SwarmNodeID == "":
		// The local socket is a standalone host for ingress (Phase 25 D-02): its proxy has no swarm node ID
		// and takes the environment's one NULL-ID slot, which the partial unique index guards.
	case topo.Kind == kindLocal:
		// Not retryable: a local socket that is a swarm manager has proxies that present node IDs, which the
		// single slot cannot hold (Phase 25 A-9), and no wait makes that succeed.
		return EnrolResult{}, &RefuseError{
			Code:        ingresswire.RefuseAuthRejected,
			Detail:      "a proxy of TierPlan's local-socket environment enrols without a swarm node ID",
			AuditReason: "environment_kind_local",
		}
	default:
		// An agent environment whose agent has not reported its type yet: this passes once it has.
		return EnrolResult{}, &RefuseError{
			Code:        ingresswire.RefuseNodeUnverified,
			Detail:      "TierPlan does not yet know whether this environment is a swarm or a standalone host; try again shortly",
			AuditReason: "environment_type_unknown", RetryAfter: nodeUnverifiedRetryAfter,
		}
	}

	// (5b) The block of a revoke (D-20) and the live enrolment of the node, before the node list is asked: a
	// swarm node is the node ID it reports, a standalone host the environment's one slot (a NULL swarm node
	// ID). A swarm node that reports no ID has no row to match and is refused node_unverified below. The
	// pre-check on the live row only spares the list read; the partial unique index decides under a race, in
	// the insert (7). This order, the slot before the node list, is noted for the Phase 28 threat model: a
	// holder of a valid token learns which swarm node IDs are enrolled or blocked, bounded by the enrolment
	// bucket.
	//
	// A live row with the same key is this node enrolling again, after a welcome it never received (a drop
	// between the commit and the welcome, CR-A-01): it proved it holds that key, so it is given the row's ID
	// instead of being refused. Another key is still refused node_already_enrolled and never replaces the row
	// (D-05).
	if !swarm || req.SwarmNodeID != "" {
		if existing, refuse, err := s.checkNodeSlot(ctx, envID, swarm, req.SwarmNodeID, req.Pub); err != nil {
			return EnrolResult{}, err
		} else if refuse != nil {
			return EnrolResult{}, refuse
		} else if existing != nil {
			return s.enrolledAgain(ctx, existing.ID, req)
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
				Detail:     "this node is not in the swarm's node list as TierPlan last read it; a node that just joined enrols by itself once TierPlan reads the list again, within about a minute",
				RetryAfter: nodeUnverifiedRetryAfter,
			}
		default:
			return EnrolResult{}, &RefuseError{
				Code: ingresswire.RefuseNodeUnverified, AuditReason: ingresswire.RefuseNodeUnverified,
				Detail:     "TierPlan cannot yet confirm this node against the swarm's node list; try again shortly",
				RetryAfter: nodeUnverifiedRetryAfter,
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
			// Another enrolment of the slot committed between the pre-check and this insert. When it was this
			// node's own (a retry that overlapped the first attempt), it is given that row, as above.
			_ = tx.Rollback()
			if existing, _, err := s.checkNodeSlot(ctx, envID, swarm, req.SwarmNodeID, req.Pub); err == nil && existing != nil {
				return s.enrolledAgain(ctx, existing.ID, req)
			}
			return EnrolResult{}, &RefuseError{
				Code: ingresswire.RefuseNodeAlreadyEnrolled, AuditReason: ingresswire.RefuseNodeAlreadyEnrolled,
				Detail: detailNodeAlreadyEnrolled,
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

// checkNodeSlot refuses an enrolment for a node an administrator revoked with the block (node_revoked) or one that
// has an active row already with another key than pub (node_already_enrolled). An active row with the key pub
// is returned as existing: the same node enrolling again. Both are nil for a free slot and any other state. The
// error is a database failure, not a refusal.
func (s *Service) checkNodeSlot(ctx context.Context, envID string, swarm bool, swarmNodeID string, pub []byte) (existing *sqlc.IngressNode, refuse *RefuseError, err error) {
	var blocked int64
	var row sqlc.IngressNode
	if swarm {
		node := sql.NullString{String: swarmNodeID, Valid: true}
		if blocked, err = s.st.RQ.CountBlockedIngressSwarmNode(ctx, sqlc.CountBlockedIngressSwarmNodeParams{EnvID: envID, SwarmNodeID: node}); err != nil {
			return nil, nil, fmt.Errorf("look for a block on the node: %w", err)
		}
		if blocked == 0 {
			row, err = s.st.RQ.ActiveIngressSwarmNode(ctx, sqlc.ActiveIngressSwarmNodeParams{EnvID: envID, SwarmNodeID: node})
		}
	} else {
		if blocked, err = s.st.RQ.CountBlockedIngressStandalone(ctx, envID); err != nil {
			return nil, nil, fmt.Errorf("look for a block on the node: %w", err)
		}
		if blocked == 0 {
			row, err = s.st.RQ.ActiveIngressStandalone(ctx, envID)
		}
	}
	switch {
	case blocked > 0:
		return nil, &RefuseError{Code: ingresswire.RefuseNodeRevoked, AuditReason: ingresswire.RefuseNodeRevoked, Detail: detailNodeRevoked}, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("look for a live enrolment of the node: %w", err)
	case subtle.ConstantTimeCompare(row.PublicKey, pub) == 1:
		return &row, nil, nil
	}
	return nil, &RefuseError{Code: ingresswire.RefuseNodeAlreadyEnrolled, AuditReason: ingresswire.RefuseNodeAlreadyEnrolled, Detail: detailNodeAlreadyEnrolled}, nil
}

// enrolledAgain answers an enrolment of a node whose live row, nodeID, already holds the key it proved: it is the
// node's own earlier enrolment, whose welcome it never received. The row is not changed. The enrolment is audited
// all the same, as ingress.node_enrolled with the detail repeated, and the row's status is read again inside the
// audit's transaction, so a revoke that committed since the slot was checked is not undone by a welcome; the node
// is then told to retry and the retry gets the revoke's answer.
func (s *Service) enrolledAgain(ctx context.Context, nodeID string, req EnrolRequest) (EnrolResult, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return EnrolResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	row, err := s.st.Q.WithTx(tx).GetIngressNode(ctx, nodeID)
	if err != nil {
		return EnrolResult{}, fmt.Errorf("load the ingress node: %w", err)
	}
	if row.Status != statusActive || subtle.ConstantTimeCompare(row.PublicKey, req.Pub) != 1 {
		return EnrolResult{}, errors.New("the node's enrolment changed while it enrolled again")
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: audit.SystemIngress, Action: audit.ActionIngressNodeEnrolled,
		TargetType: targetIngressNode, TargetID: row.ID, EnvID: row.EnvID, Result: audit.ResultSuccess,
		IP: req.IP, UserAgent: userAgentPrefix + req.Version,
		Details: map[string]string{"node": row.Name, "swarm_node": row.SwarmNodeID.String, "repeated": "true"},
	}); err != nil {
		return EnrolResult{}, fmt.Errorf("audit ingress node enrolled again: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return EnrolResult{}, fmt.Errorf("commit: %w", err)
	}
	return EnrolResult{NodeID: row.ID, EnvID: row.EnvID, Name: row.Name, SwarmNodeID: row.SwarmNodeID.String}, nil
}
