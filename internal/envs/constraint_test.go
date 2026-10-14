// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The placement constraint evaluator (plan 06-11, RES-12, RESEARCH Pattern 9). The
// vector file is shared with the browser's evaluator (constraint.test.ts), so the two
// cannot disagree about which nodes a constraint matches.

const constraintVectors = "../../web/src/features/placement/constraint-vectors.json"

type vectorNode struct {
	ID           string            `json:"id"`
	Hostname     string            `json:"hostname"`
	IP           string            `json:"ip"`
	Role         string            `json:"role"`
	OS           string            `json:"os"`
	Arch         string            `json:"arch"`
	Labels       map[string]string `json:"labels"`
	EngineLabels map[string]string `json:"engineLabels"`
}

type vector struct {
	Constraint string     `json:"constraint"`
	Node       vectorNode `json:"node"`
	Matches    bool       `json:"matches"`
	Error      bool       `json:"error"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile(constraintVectors)
	if err != nil {
		t.Fatalf("read the shared constraint vectors: %v", err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil {
		t.Fatalf("parse %s: %v", constraintVectors, err)
	}
	return vs
}

func factsOf(n vectorNode) NodeFacts {
	return NodeFacts{
		ID: n.ID, Hostname: n.Hostname, IP: n.IP, Role: n.Role, OS: n.OS, Arch: n.Arch,
		Labels: n.Labels, EngineLabels: n.EngineLabels,
	}
}

func TestConstraintVectorsHoldEnoughCases(t *testing.T) {
	if n := len(loadVectors(t)); n < 30 {
		t.Fatalf("the shared vectors hold %d cases, want at least 30", n)
	}
}

func TestConstraintMatchesTheSharedVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Constraint, func(t *testing.T) {
			got, err := MatchesAll([]string{v.Constraint}, factsOf(v.Node))
			if v.Error {
				if err == nil {
					t.Fatalf("MatchesAll(%q) = %v, nil; want a parse error", v.Constraint, got)
				}
				if _, perr := ParseConstraint(v.Constraint); perr == nil {
					t.Fatalf("ParseConstraint(%q) accepted a constraint the vectors call an error", v.Constraint)
				}
				return
			}
			if err != nil {
				t.Fatalf("MatchesAll(%q): %v", v.Constraint, err)
			}
			if got != v.Matches {
				t.Errorf("MatchesAll(%q) on node %q = %v, want %v", v.Constraint, v.Node.ID, got, v.Matches)
			}
		})
	}
}

func TestParseConstraintKeepsKeyOperatorAndValue(t *testing.T) {
	c, err := ParseConstraint("  node.labels.disk != SSD ")
	if err != nil {
		t.Fatalf("ParseConstraint: %v", err)
	}
	if c.Key != "node.labels.disk" || c.Value != "SSD" || c.Equal {
		t.Errorf("c = %+v, want key node.labels.disk, value SSD, not equal", c)
	}
	if c, err := ParseConstraint("node.role==manager"); err != nil || !c.Equal {
		t.Errorf("ParseConstraint(==) = %+v, %v, want Equal", c, err)
	}
}

func TestMatchesAllNeedsEveryConstraintAndRefusesABadOne(t *testing.T) {
	n := NodeFacts{ID: "n1", Role: "worker", Labels: map[string]string{"disk": "ssd"}}
	if ok, err := MatchesAll(nil, n); err != nil || !ok {
		t.Errorf("no constraints = %v, %v, want a match", ok, err)
	}
	if ok, err := MatchesAll([]string{"node.role==worker", "node.labels.disk==ssd"}, n); err != nil || !ok {
		t.Errorf("both true = %v, %v, want a match", ok, err)
	}
	if ok, err := MatchesAll([]string{"node.role==worker", "node.labels.disk==hdd"}, n); err != nil || ok {
		t.Errorf("one false = %v, %v, want no match", ok, err)
	}
	ok, err := MatchesAll([]string{"node.role==worker", "node.role=manager"}, n)
	if err == nil || ok {
		t.Errorf("a bad constraint = %v, %v, want an error and no match", ok, err)
	}
	if err != nil && !strings.Contains(err.Error(), "node.role=manager") {
		t.Errorf("the error %q does not name the constraint", err)
	}
}

func TestNodeFactsOfReadsTheClusterRow(t *testing.T) {
	got := NodeFactsOf(dockerx.NodeRow{
		ID: "n1", Hostname: "mgr-a", Role: "manager", OS: "linux", Arch: "aarch64", Address: "10.0.0.5",
		Labels: map[string]string{"zone": "a"}, EngineLabels: map[string]string{"type": "gpu"},
	})
	if got.ID != "n1" || got.Hostname != "mgr-a" || got.IP != "10.0.0.5" || got.Role != "manager" || got.OS != "linux" || got.Arch != "aarch64" {
		t.Errorf("facts = %+v", got)
	}
	if got.Labels["zone"] != "a" || got.EngineLabels["type"] != "gpu" {
		t.Errorf("facts labels = %v, engine labels = %v", got.Labels, got.EngineLabels)
	}
}
