// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

const (
	// maxRevokeReason is the longest reason an administrator may give, in characters.
	maxRevokeReason = 256
	// revokeSampleBytes bounds the hostnames an audit row lists: the audit log cuts a detail at 512 bytes, and a
	// cut in the middle of a name would be a lie, so the sample is built here and stops on a name boundary.
	revokeSampleBytes = 480
)

var (
	// ErrNodeNotFound means no ingress node with that ID belongs to the environment.
	ErrNodeNotFound = errors.New("ingresshub: no such ingress node")
	// ErrAlreadyRevoked means the node was revoked already; nothing changed.
	ErrAlreadyRevoked = errors.New("ingresshub: the ingress node is already revoked")
)

// Kicker closes the live session of a node. The hub implements it; Revoke calls it only once the revoke has
// committed, so a node that reconnects at once finds its row revoked.
type Kicker interface {
	Kick(nodeID, code string)
}

// WithKicker tells the service whom to ask to close a revoked node's session. Without one a revoke still
// commits, and the row refuses the node's next connect. Call it before the service is used.
func (s *Service) WithKicker(k Kicker) *Service {
	s.kicker = k
	return s
}

// RevokeOptions is what an administrator chooses when revoking a node.
type RevokeOptions struct {
	// Reason is optional, shown in the audit log and cut to 256 characters.
	Reason string
	// AllowReenrol lets the host enrol again with the join token that is still mounted in its container. By
	// default a revoke blocks that (D-20).
	AllowReenrol bool
}

// Revoked is the outcome of a revoke.
type Revoked struct {
	NodeID string
	// HeldHostnames is every hostname whose key the node was ever sent (D-24), sorted, and never nil: the
	// administrator decides what to reissue.
	HeldHostnames []string
	// RotateTokenRecommended is always true: the join token stays mounted in the revoked node's container
	// until the administrator rotates it (Pitfall 9).
	RotateTokenRecommended bool
	RevokedAt              time.Time
}

// Revoke makes one node unwelcome: its row becomes revoked, by whom and why, with the block on enrolling
// again unless the administrator allowed it, and ingress.node_revoked is audited, in one transaction. Once that
// has committed the node's live session is closed. It touches no other node (D-16) and never changes
// anything on the host.
//
// A node of another environment is ErrNodeNotFound and an unknown environment ErrEnvironmentNotFound. A
// node that is revoked already is ErrAlreadyRevoked and nothing changes.
func (s *Service) Revoke(ctx context.Context, actor audit.Actor, envID, nodeID string, o RevokeOptions, ip, ua string) (Revoked, error) {
	reason := cleanReason(o.Reason)
	ts := s.now().UTC()

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Revoked{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if _, err := q.IngressEnvironmentTopology(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return Revoked{}, ErrEnvironmentNotFound
	} else if err != nil {
		return Revoked{}, fmt.Errorf("look up the environment: %w", err)
	}
	row, err := q.GetIngressNode(ctx, nodeID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Revoked{}, ErrNodeNotFound
	case err != nil:
		return Revoked{}, fmt.Errorf("load the ingress node: %w", err)
	case row.EnvID != envID:
		return Revoked{}, ErrNodeNotFound
	}
	var blocked int64 = 1
	if o.AllowReenrol {
		blocked = 0
	}
	changed, err := q.RevokeIngressNode(ctx, sqlc.RevokeIngressNodeParams{
		RevokedAt: ts.UnixNano(), RevokedBy: actor.ID, RevokeReason: reason, ReenrolBlocked: blocked,
		ID: nodeID, EnvID: envID,
	})
	if err != nil {
		return Revoked{}, fmt.Errorf("revoke the ingress node: %w", err)
	}
	if changed == 0 {
		return Revoked{}, ErrAlreadyRevoked
	}

	held := decodeHeld(row.HeldHostnames)
	details := map[string]string{
		"node": row.Name, "swarm_node": row.SwarmNodeID.String,
		"held": strconv.Itoa(len(held)), "reenrol": "blocked",
	}
	if o.AllowReenrol {
		details["reenrol"] = "allowed"
	}
	if reason != "" {
		details["reason"] = reason
	}
	if sample := hostnameSample(held); sample != "" {
		details["sample"] = sample
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionIngressNodeRevoked, TargetType: targetIngressNode, TargetID: nodeID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua, Details: details,
	}); err != nil {
		return Revoked{}, fmt.Errorf("audit ingress node revoked: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Revoked{}, fmt.Errorf("commit: %w", err)
	}
	s.forgetSeen(nodeID)
	// After the commit, so the hub's check of the node's status as it reconnects sees the revoke.
	if s.kicker != nil {
		s.kicker.Kick(nodeID, ingresswire.CloseRevoked)
	}
	return Revoked{NodeID: nodeID, HeldHostnames: held, RotateTokenRecommended: true, RevokedAt: ts}, nil
}

// hostnameSample joins the names with commas, stopping before the name that would take the text past
// revokeSampleBytes. The count in the audit row tells how many there were in all.
func hostnameSample(names []string) string {
	var b strings.Builder
	for _, n := range names {
		add := len(n)
		if b.Len() > 0 {
			add++
		}
		if b.Len()+add > revokeSampleBytes {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
	}
	return b.String()
}

// cleanReason makes the reason an administrator typed fit the audit log: no control characters, valid UTF-8,
// at most maxRevokeReason characters.
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
