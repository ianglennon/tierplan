// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"fmt"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// What a change to a node will do (plan 06-15, RES-05, D-20). The arithmetic is quorum: a
// majority of n managers is floor(n/2)+1, and demoting a reachable manager is at risk when the
// managers that can still be reached would not make a majority of those left.

func TestQuorumAtRisk(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		total, reachable int
		want             bool
	}{
		{3, 3, false}, // 2 reachable of 2 left, majority of 2 is 2
		{3, 2, true},  // 1 reachable of 2 left
		{3, 1, true},
		{5, 5, false},
		{5, 4, false}, // 3 reachable of 4 left, majority of 4 is 3
		{5, 3, true},  // 2 reachable of 4 left
		{4, 4, false},
		{4, 3, false}, // 2 reachable of 3 left, majority of 3 is 2
		{4, 2, true},
		{2, 2, false}, // 1 of 1
		{2, 1, true},
		{1, 1, true}, // nobody left
	} {
		if got := quorumAtRisk(tc.total, tc.reachable); got != tc.want {
			t.Errorf("quorumAtRisk(%d, %d) = %v, want %v", tc.total, tc.reachable, got, tc.want)
		}
	}
}

func niNode(id, role string) dockerx.NodeRow {
	n := dockerx.NodeRow{
		ID: id, Hostname: "host-" + id, Role: role, Availability: "active", State: "ready", OS: "linux", Arch: "x86_64",
		Labels: map[string]string{}, EngineLabels: map[string]string{},
	}
	if role == "manager" {
		n.Reachability = "reachable"
	}
	return n
}

// sameWarnings compares two warning lists, an empty one being the same as a nil one.
func sameWarnings(got, want []string) bool {
	return (len(got) == 0 && len(want) == 0) || slices.Equal(got, want)
}

func niManagers(reach ...string) []dockerx.NodeRow {
	var out []dockerx.NodeRow
	for i, r := range reach {
		n := niNode(fmt.Sprintf("m%d", i+1), "manager")
		n.Reachability = r
		out = append(out, n)
	}
	return out
}

func niTask(id, service string, slot int) dockerx.TaskRow {
	return dockerx.TaskRow{ID: id, ServiceID: service, NodeID: "w1", Slot: slot, State: "running", DesiredState: "running"}
}

func niServices(rows ...dockerx.ServiceRow) map[string]dockerx.ServiceRow {
	m := map[string]dockerx.ServiceRow{}
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

func niOutcomes(b NodeImpactBody) map[string]string {
	m := map[string]string{}
	for _, t := range b.Tasks {
		m[t.ID] = t.Outcome
	}
	return m
}

func TestNodeImpactDrainClassifiesEachTask(t *testing.T) {
	t.Parallel()
	w1 := niNode("w1", "worker")
	other := niNode("w2", "worker")
	ssd := niNode("w3", "worker")
	ssd.Labels = map[string]string{"disk": "ssd"}
	paused, down := niNode("w4", "worker"), niNode("w5", "worker")
	paused.Availability, down.State = "pause", "down"
	paused.Labels, down.Labels = map[string]string{"disk": "hdd"}, map[string]string{"disk": "hdd"}
	mgr := niNode("m1", "manager")

	in := nodeImpactInput{
		Action: "drain", Node: w1,
		Nodes: []dockerx.NodeRow{w1, other, ssd, paused, down, mgr},
		Tasks: []dockerx.TaskRow{
			niTask("t-web-1", "web", 1),
			niTask("t-web-2", "web", 2),
			niTask("t-agent", "agent", 0),
			niTask("t-db", "db", 1),
			niTask("t-hdd", "hdd", 1),
			niTask("t-job", "job", 1),
			niTask("t-cron", "cron", 0),
			niTask("t-typo", "typo", 1),
			niTask("t-orphan", "gone", 1),
		},
		Services: niServices(
			dockerx.ServiceRow{ID: "web", Name: "web", Mode: "replicated"},
			dockerx.ServiceRow{ID: "agent", Name: "agent", Mode: "global"},
			// Only the ssd node (w3) matches, and it is active and ready.
			dockerx.ServiceRow{ID: "db", Name: "db", Mode: "replicated", Constraints: []string{"node.labels.disk==ssd"}},
			// The nodes that carry hdd are paused and down: nothing else can take it.
			dockerx.ServiceRow{ID: "hdd", Name: "hdd", Mode: "replicated", Constraints: []string{"node.labels.disk==hdd"}},
			dockerx.ServiceRow{ID: "job", Name: "job", Mode: "replicated-job"},
			dockerx.ServiceRow{ID: "cron", Name: "cron", Mode: "global-job"},
			// A constraint that does not parse matches nothing.
			dockerx.ServiceRow{ID: "typo", Name: "typo", Mode: "replicated", Constraints: []string{"node.role=worker"}},
		),
	}
	b := nodeImpact(in)
	want := map[string]string{
		"t-web-1": taskMoves, "t-web-2": taskMoves, "t-agent": taskStops, "t-db": taskMoves, "t-hdd": taskCannotPlace,
		"t-job": taskMoves, "t-cron": taskStops, "t-typo": taskCannotPlace,
		// A task of a service the read no longer lists cannot be placed by anyone.
		"t-orphan": taskCannotPlace,
	}
	got := niOutcomes(b)
	for id, w := range want {
		if got[id] != w {
			t.Errorf("task %s: %q, want %q", id, got[id], w)
		}
	}
	if len(b.Tasks) != len(want) {
		t.Errorf("%d tasks, want %d", len(b.Tasks), len(want))
	}
	// Sorted by service name, slot, then task ID.
	var names []string
	for _, tk := range b.Tasks {
		names = append(names, tk.Service+"/"+fmt.Sprint(tk.Slot))
	}
	if !slices.IsSorted(names) {
		t.Errorf("tasks are %v, want them sorted by service then slot", names)
	}
	if b.Action != "drain" || b.Hostname != "host-w1" || len(b.Warnings) != 0 || b.NeedsConfirm {
		t.Errorf("body = %+v, want no warnings and no typed confirm", b)
	}
}

func TestNodeImpactDrainOfANodeWithNoTasksHasAnEmptyListAndNoWarnings(t *testing.T) {
	t.Parallel()
	w1 := niNode("w1", "worker")
	b := nodeImpact(nodeImpactInput{Action: "drain", Node: w1, Nodes: []dockerx.NodeRow{w1}})
	if b.Tasks == nil || len(b.Tasks) != 0 || b.Warnings == nil || len(b.Warnings) != 0 || b.NeedsConfirm {
		t.Errorf("body = %+v, want empty non-nil lists and no confirm", b)
	}
}

func TestNodeImpactDrainNeedsTheNameForTierPlansNodeAndTheOnlyManagerAgent(t *testing.T) {
	t.Parallel()
	m := niNode("m1", "manager")
	for _, tc := range []struct {
		name          string
		runs, onlyAgt bool
		warnings      []string
		confirm       bool
	}{
		{"neither", false, false, nil, false},
		{"TierPlan's node", true, false, []string{warnRunsTierPlan}, true},
		{"the only manager agent", false, true, []string{warnOnlyManagerAgent}, true},
		{"both", true, true, []string{warnRunsTierPlan, warnOnlyManagerAgent}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := nodeImpact(nodeImpactInput{Action: "drain", Node: m, Nodes: []dockerx.NodeRow{m}, RunsTierPlan: tc.runs, OnlyManagerAgent: tc.onlyAgt})
			if !sameWarnings(b.Warnings, tc.warnings) {
				t.Errorf("warnings = %v, want %v", b.Warnings, tc.warnings)
			}
			if b.NeedsConfirm != tc.confirm {
				t.Errorf("needsConfirm = %v, want %v", b.NeedsConfirm, tc.confirm)
			}
		})
	}
}

func TestNodeImpactPauseKeepsTheTasksAndNeverNeedsTheName(t *testing.T) {
	t.Parallel()
	w := niNode("w1", "worker")
	b := nodeImpact(nodeImpactInput{
		Action: "pause", Node: w, Nodes: []dockerx.NodeRow{w},
		Tasks:    []dockerx.TaskRow{niTask("t1", "web", 1), niTask("t2", "agent", 0)},
		Services: niServices(dockerx.ServiceRow{ID: "web", Name: "web", Mode: "replicated"}, dockerx.ServiceRow{ID: "agent", Name: "agent", Mode: "global"}),
		// Pausing sends nothing away, so even TierPlan's own node needs no warning.
		RunsTierPlan: true, OnlyManagerAgent: true,
	})
	for _, tk := range b.Tasks {
		if tk.Outcome != taskStays {
			t.Errorf("task %s: %q, want %q", tk.ID, tk.Outcome, taskStays)
		}
	}
	if len(b.Tasks) != 2 || len(b.Warnings) != 0 || b.NeedsConfirm {
		t.Errorf("body = %+v, want two staying tasks, no warnings, no confirm", b)
	}
}

func TestNodeImpactDemoteCountsManagersAndWarns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		reach     []string // the reachability of m1.. ; the node demoted is m1
		total     int
		reachable int
		after     int
		warnings  []string
		confirm   bool
	}{
		{"three all reachable", []string{"reachable", "reachable", "reachable"}, 3, 3, 2, []string{warnEvenCount}, false},
		{"three with another unreachable", []string{"reachable", "reachable", "unreachable"}, 3, 2, 2, []string{warnQuorumAtRisk, warnEvenCount}, true},
		{"three, the unreachable one demoted", []string{"unreachable", "reachable", "reachable"}, 3, 2, 2, []string{warnEvenCount}, false},
		{"five all reachable", []string{"reachable", "reachable", "reachable", "reachable", "reachable"}, 5, 5, 4, []string{warnEvenCount}, false},
		{"five with two unreachable", []string{"reachable", "reachable", "reachable", "unreachable", "unreachable"}, 5, 3, 4, []string{warnQuorumAtRisk, warnEvenCount}, true},
		{"five with one unreachable", []string{"reachable", "reachable", "reachable", "reachable", "unreachable"}, 5, 4, 4, []string{warnEvenCount}, false},
		{"four all reachable", []string{"reachable", "reachable", "reachable", "reachable"}, 4, 4, 3, nil, false},
		{"the last manager", []string{"reachable"}, 1, 1, 0, []string{warnQuorumAtRisk}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := niManagers(tc.reach...)
			b := nodeImpact(nodeImpactInput{Action: "demote", Node: nodes[0], Nodes: nodes})
			if b.Managers != (NodeImpactManagers{Total: tc.total, Reachable: tc.reachable, After: tc.after}) {
				t.Errorf("managers = %+v, want total %d reachable %d after %d", b.Managers, tc.total, tc.reachable, tc.after)
			}
			if !sameWarnings(b.Warnings, tc.warnings) {
				t.Errorf("warnings = %v, want %v", b.Warnings, tc.warnings)
			}
			if b.NeedsConfirm != tc.confirm {
				t.Errorf("needsConfirm = %v, want %v", b.NeedsConfirm, tc.confirm)
			}
			if len(b.Tasks) != 0 {
				t.Errorf("a demote lists tasks: %v", b.Tasks)
			}
		})
	}
}

func TestNodeImpactDemoteOfTierPlansNodeOrTheOnlyManagerAgentNeedsTheName(t *testing.T) {
	t.Parallel()
	nodes := niManagers("reachable", "reachable", "reachable", "reachable")
	for name, in := range map[string]nodeImpactInput{
		"TierPlan's node":        {RunsTierPlan: true},
		"the only manager agent": {OnlyManagerAgent: true},
	} {
		in.Action, in.Node, in.Nodes = "demote", nodes[0], nodes
		b := nodeImpact(in)
		if !b.NeedsConfirm || len(b.Warnings) != 1 {
			t.Errorf("%s: body = %+v, want one warning and the typed confirm", name, b)
		}
	}
}

func TestNodeImpactDemoteWithManagersWhoseStateIsUnknownCannotTellAndNeedsTheName(t *testing.T) {
	t.Parallel()
	nodes := niManagers("reachable", "unknown", "reachable", "reachable")
	b := nodeImpact(nodeImpactInput{Action: "demote", Node: nodes[0], Nodes: nodes})
	if !slices.Contains(b.Warnings, warnFactsUnreadable) || !b.NeedsConfirm {
		t.Errorf("body = %+v, want facts_unreadable and the typed confirm", b)
	}
	// A drain does not depend on quorum, so unknown managers do not warn there.
	d := nodeImpact(nodeImpactInput{Action: "drain", Node: nodes[0], Nodes: nodes})
	if slices.Contains(d.Warnings, warnFactsUnreadable) || d.NeedsConfirm {
		t.Errorf("drain body = %+v, want no facts_unreadable", d)
	}
}

func TestNodeImpactPromoteAddsAManagerAndWarnsOfNothing(t *testing.T) {
	t.Parallel()
	nodes := append(niManagers("reachable", "reachable", "unreachable"), niNode("w1", "worker"))
	b := nodeImpact(nodeImpactInput{Action: "promote", Node: nodes[3], Nodes: nodes})
	if b.Managers != (NodeImpactManagers{Total: 3, Reachable: 2, After: 4}) {
		t.Errorf("managers = %+v, want 3 now, 2 reachable, 4 after", b.Managers)
	}
	if len(b.Warnings) != 0 || b.NeedsConfirm || len(b.Tasks) != 0 {
		t.Errorf("body = %+v, want no warnings", b)
	}
}

func TestNodeImpactDemotingAWorkerDoesNotChangeTheManagers(t *testing.T) {
	t.Parallel()
	nodes := append(niManagers("reachable", "reachable", "reachable"), niNode("w1", "worker"))
	b := nodeImpact(nodeImpactInput{Action: "demote", Node: nodes[3], Nodes: nodes})
	if b.Managers.After != 3 || len(b.Warnings) != 0 || b.NeedsConfirm {
		t.Errorf("body = %+v, want 3 managers after and no warnings", b)
	}
}
