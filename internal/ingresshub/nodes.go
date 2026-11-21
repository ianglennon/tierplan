// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Node is one ingress node as the node list and the API show it. An instant that never happened is the zero
// time.
type Node struct {
	ID, EnvID, Name, SwarmNodeID, Status, Version string
	// Schemas are the bundle schemas the node said it can apply; empty until it has said hello.
	Schemas                          []int
	AppliedBundleID                  string
	AppliedAt                        time.Time
	RejectedBundleID, RejectedReason string
	RejectedAt                       time.Time
	// Health is "", "ok" or "unhealthy".
	Health                                     string
	HealthAt, LastSeenAt, CreatedAt, RevokedAt time.Time
	RevokedBy, RevokeReason                    string
	// ReenrolBlocked is true when the revoke also blocked the host from enrolling again (D-20).
	ReenrolBlocked bool
	// HeldHostnames is every hostname whose key the node was ever sent (D-24), never nil.
	HeldHostnames []string
	// Incompatible is true when the node's supervisor shares no bundle schema with this server, so it keeps
	// the last bundle it had; IncompatibleReason is then ingress_schema_unsupported.
	Incompatible       bool
	IncompatibleReason string
}

// ListNodes returns the environment's nodes, oldest first and by ID among those enrolled in the same instant.
func (s *Service) ListNodes(ctx context.Context, envID string) ([]Node, error) {
	rows, err := s.st.RQ.ListIngressNodes(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("list the ingress nodes: %w", err)
	}
	out := make([]Node, 0, len(rows))
	for _, r := range rows {
		out = append(out, nodeOf(r))
	}
	return out, nil
}

// RemoveEnvNodes deletes every active ingress node row of the environment as the last step of switching ingress off
// (D-39). A revoked row is kept: it is the only record that keeps a revoked host out (reenrol_blocked), so a switch off
// followed by a switch on must not re-admit it (WR-03). It records ingress.node_removed for each row it removes in the
// same transaction, so a row never goes without its audit entry and an audit entry never names a row that stayed. The rows
// that were removed are returned, oldest first. An environment with no active rows removes nothing and records nothing.
// It closes no session: the caller has kicked them.
func (s *Service) RemoveEnvNodes(ctx context.Context, actor audit.Actor, envID string) ([]Node, error) {
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	rows, err := s.st.Q.WithTx(tx).DeleteIngressNodesByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("remove the ingress nodes: %w", err)
	}
	out := make([]Node, 0, len(rows))
	for _, r := range rows {
		n := nodeOf(r)
		out = append(out, n)
		if err := s.aud.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressNodeRemoved, TargetType: targetIngressNode, TargetID: n.ID,
			EnvID: envID, Result: audit.ResultSuccess, Details: map[string]string{"node": n.Name},
		}); err != nil {
			return nil, fmt.Errorf("audit ingress node removed: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	slices.SortStableFunc(out, func(a, b Node) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// nodeOf decodes a row: the JSON columns, the instants and the compatibility of the node's schemas.
func nodeOf(r sqlc.IngressNode) Node {
	n := Node{
		ID: r.ID, EnvID: r.EnvID, Name: r.Name, SwarmNodeID: r.SwarmNodeID.String, Status: r.Status,
		Version: r.Version, Schemas: decodeSchemas(r.Schemas),
		AppliedBundleID: r.AppliedBundleID, AppliedAt: instant(r.AppliedAt),
		RejectedBundleID: r.RejectedBundleID, RejectedReason: r.RejectedReason, RejectedAt: instant(r.RejectedAt),
		Health: r.Health, HealthAt: instant(r.HealthAt), LastSeenAt: instant(r.LastSeenAt),
		CreatedAt: instant(r.CreatedAt), RevokedAt: instant(r.RevokedAt),
		RevokedBy: r.RevokedBy, RevokeReason: r.RevokeReason, ReenrolBlocked: r.ReenrolBlocked == 1,
		HeldHostnames: decodeHeld(r.HeldHostnames),
	}
	// A node that has not said hello has no schemas to compare, so it is not marked incompatible.
	if len(n.Schemas) > 0 {
		if _, err := ingressrender.PickSchema(n.Schemas); err != nil {
			n.Incompatible, n.IncompatibleReason = true, ingressrender.CodeSchemaUnsupported
		}
	}
	return n
}

// instant is a stored UnixNano, where 0 is "never".
func instant(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// decodeSchemas reads a schemas column; one that cannot be read is the empty list.
func decodeSchemas(col string) []int {
	var out []int
	if err := json.Unmarshal([]byte(col), &out); err != nil || out == nil {
		return []int{}
	}
	return out
}
