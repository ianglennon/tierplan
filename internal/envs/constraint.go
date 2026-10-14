// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import "github.com/ianglennon/bayplan/internal/dockerx"

// Constraint is one parsed placement constraint: key, operator and value.
type Constraint struct {
	Key, Value string
	// Equal is true for ==, false for !=.
	Equal bool
}

// NodeFacts are the facts of one node that a constraint reads.
type NodeFacts struct {
	ID, Hostname, IP, Role, OS, Arch string
	Labels, EngineLabels             map[string]string
}

// ParseConstraint reads one constraint.
func ParseConstraint(s string) (Constraint, error) { return Constraint{}, nil }

// NodeFactsOf takes the facts a constraint reads from a cluster node row.
func NodeFactsOf(n dockerx.NodeRow) NodeFacts { return NodeFacts{} }

// Matches reports whether the node satisfies the constraint.
func (c Constraint) Matches(n NodeFacts) bool { return false }

// MatchesAll reports whether the node satisfies every constraint.
func MatchesAll(constraints []string, n NodeFacts) (bool, error) { return false, nil }
