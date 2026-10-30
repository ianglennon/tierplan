// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// The placement constraint evaluator (RES-12). It follows swarmkit's
// manager/constraint package, which is what the scheduler itself runs, so the answer
// to "does this node match" is the one the swarm gives (RESEARCH Pattern 9): the
// operators are == and != only, values compare without regard to case, the names of
// labels do not, a field a node lacks is the empty string, and a key nobody defined
// matches no node. web/src/features/placement/constraint.ts is the same evaluator for
// the browser, and both read constraint-vectors.json.

var (
	// constraintKey and constraintValue are swarmkit's patterns. They are anchored and
	// bounded by RE2, so a hostile constraint cannot make the parse slow (T-06-35).
	constraintKey   = regexp.MustCompile(`^(?i)[a-z_][a-z0-9\-_.]+$`)
	constraintValue = regexp.MustCompile(`^(?i)[a-z0-9:\-_\s\.\*\(\)\?\+\[\$\^\]\/\\]+$`)
)

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

// ParseConstraint reads one constraint of the form key==value or key!=value. The key
// and the value are trimmed. A constraint with another operator, an empty side or a
// character the swarm does not accept is an error.
func ParseConstraint(s string) (Constraint, error) {
	for _, op := range []string{"==", "!="} {
		key, value, found := strings.Cut(s, op)
		if !found {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !constraintKey.MatchString(key) {
			return Constraint{}, fmt.Errorf("constraint %q: key %q is invalid", s, key)
		}
		if !constraintValue.MatchString(value) {
			return Constraint{}, fmt.Errorf("constraint %q: value %q is invalid", s, value)
		}
		return Constraint{Key: key, Value: value, Equal: op == "=="}, nil
	}
	return Constraint{}, fmt.Errorf("constraint %q: expected key==value or key!=value", s)
}

// NodeFactsOf takes the facts a constraint reads from a cluster node row.
func NodeFactsOf(n dockerx.NodeRow) NodeFacts {
	return NodeFacts{
		ID: n.ID, Hostname: n.Hostname, IP: n.Address, Role: n.Role, OS: n.OS, Arch: n.Arch,
		Labels: n.Labels, EngineLabels: n.EngineLabels,
	}
}

// Matches reports whether the node satisfies the constraint.
func (c Constraint) Matches(n NodeFacts) bool {
	key := strings.ToLower(c.Key)
	switch {
	case key == "node.id":
		return c.matchValue(n.ID)
	case key == "node.hostname":
		return c.matchValue(n.Hostname)
	case key == "node.ip":
		return c.matchIP(n.IP)
	case key == "node.role":
		return c.matchValue(n.Role)
	case key == "node.platform.os":
		return c.matchValue(n.OS)
	case key == "node.platform.arch":
		return c.matchValue(n.Arch)
	// The prefixes are case-sensitive and so are the label names, as in swarmkit.
	case strings.HasPrefix(c.Key, "node.labels."):
		return c.matchValue(n.Labels[strings.TrimPrefix(c.Key, "node.labels.")])
	case strings.HasPrefix(c.Key, "engine.labels."):
		return c.matchValue(n.EngineLabels[strings.TrimPrefix(c.Key, "engine.labels.")])
	}
	// A key the swarm does not define matches no node, whichever the operator.
	return false
}

func (c Constraint) matchValue(got string) bool {
	return strings.EqualFold(c.Value, got) == c.Equal
}

// matchIP compares the node's address with an address or a CIDR. A value that is
// neither matches no node, whichever the operator.
func (c Constraint) matchIP(addr string) bool {
	node := net.ParseIP(addr)
	if ip := net.ParseIP(c.Value); ip != nil {
		return ip.Equal(node) == c.Equal
	}
	if _, subnet, err := net.ParseCIDR(c.Value); err == nil {
		return subnet.Contains(node) == c.Equal
	}
	return false
}

// MatchesAll reports whether the node satisfies every constraint; no constraints
// match every node. A constraint that does not parse is an error, and the error says
// which, so a caller never reads a typo as "no node matches".
func MatchesAll(constraints []string, n NodeFacts) (bool, error) {
	parsed := make([]Constraint, 0, len(constraints))
	for _, s := range constraints {
		c, err := ParseConstraint(s)
		if err != nil {
			return false, err
		}
		parsed = append(parsed, c)
	}
	for _, c := range parsed {
		if !c.Matches(n) {
			return false, nil
		}
	}
	return true, nil
}
