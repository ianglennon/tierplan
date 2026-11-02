// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
)

// What a change of a node's labels does to placement (plan 06-16, RES-05, D-21). The judgement is
// pure: the node, the labels asked for, the services and the node's tasks go in and the
// constraints the change touches come out. Label names are compared byte for byte and constraint
// values without regard to case, as the swarm does.

func TestReservedLabel(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"com.docker.x":             true,
		"COM.Docker.x":             true,
		"com.docker.":              true,
		"io.docker.y":              true,
		"Io.DOCKER.y":              true,
		"org.dockerproject.z":      true,
		"ORG.DockerProject.z":      true,
		"com.dockerish":            false,
		"com.docker":               false,
		"disk":                     false,
		"x.com.docker.y":           false,
		"org.dockerprojects.z":     false,
		"io.dockerx":               false,
		"":                         false,
		"com.example.com.docker.a": false,
	} {
		if got := reservedLabel(key); got != want {
			t.Errorf("reservedLabel(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestLabelChanges(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		before, after           map[string]string
		added, changed, removed int
	}{
		"nothing":                      {nil, nil, 0, 0, 0},
		"an empty map is nothing":      {map[string]string{}, nil, 0, 0, 0},
		"one added":                    {nil, map[string]string{"a": "1"}, 1, 0, 0},
		"one removed":                  {map[string]string{"a": "1"}, map[string]string{}, 0, 0, 1},
		"one changed":                  {map[string]string{"a": "1"}, map[string]string{"a": "2"}, 0, 1, 0},
		"the same":                     {map[string]string{"a": "1"}, map[string]string{"a": "1"}, 0, 0, 0},
		"names compare byte for byte":  {map[string]string{"Disk": "ssd"}, map[string]string{"disk": "ssd"}, 1, 0, 1},
		"values compare byte for byte": {map[string]string{"disk": "SSD"}, map[string]string{"disk": "ssd"}, 0, 1, 0},
		"all three": {
			map[string]string{"keep": "x", "move": "a", "drop": "z"},
			map[string]string{"keep": "x", "move": "b", "new": "n"},
			1, 1, 1,
		},
	} {
		added, changed, removed := labelChanges(tc.before, tc.after)
		if added != tc.added || changed != tc.changed || removed != tc.removed {
			t.Errorf("%s: added %d changed %d removed %d, want %d %d %d", name, added, changed, removed, tc.added, tc.changed, tc.removed)
		}
	}
}

func nlNode(labels map[string]string) dockerx.NodeRow {
	return dockerx.NodeRow{
		ID: "w1", Hostname: "wrk-1", Role: "worker", Availability: "active", State: "ready", OS: "linux", Arch: "x86_64",
		Labels: labels, EngineLabels: map[string]string{"engine": "e1"},
	}
}

func nlTask(id, service string, slot int) dockerx.TaskRow {
	return dockerx.TaskRow{ID: id, ServiceID: service, NodeID: "w1", Slot: slot, State: "running", DesiredState: "running"}
}

type nlRowKey struct {
	service, constraint string
	before, after       bool
}

func nlKeys(b NodeLabelImpactBody) []nlRowKey {
	out := []nlRowKey{}
	for _, r := range b.Rows {
		out = append(out, nlRowKey{r.Service, r.Constraint, r.Before, r.After})
	}
	return out
}

func TestNodeLabelImpact(t *testing.T) {
	t.Parallel()
	svcs := []dockerx.ServiceRow{
		{ID: "s-db", Name: "db", Mode: "replicated", Constraints: []string{"node.labels.role==db"}},
		{ID: "s-cache", Name: "cache", Mode: "replicated", Constraints: []string{"node.labels.disk==ssd", "node.role==worker"}},
		{ID: "s-web", Name: "web", Mode: "replicated", Constraints: []string{"node.role==worker"}},
		{ID: "s-free", Name: "free", Mode: "replicated"},
		{ID: "s-eng", Name: "eng", Mode: "replicated", Constraints: []string{"engine.labels.disk==ssd"}},
		{ID: "s-not", Name: "not", Mode: "replicated", Constraints: []string{"node.labels.zone!=b"}},
	}

	t.Run("gaining a label lists the constraint that now matches", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{}), Version: 9, Labels: map[string]string{"disk": "ssd"}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-cache", 1)},
		})
		want := []nlRowKey{{"cache", "node.labels.disk==ssd", false, true}}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
		if b.Version != 9 || b.NoLongerMatches != 0 || b.TasksMayMove != 0 || b.Added != 1 || b.Changed != 0 || b.Removed != 0 {
			t.Errorf("body = %+v, want version 9, nothing unmatched, one added", b)
		}
	})

	t.Run("removing a label lists the constraint that no longer matches and counts its tasks", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"role": "db", "zone": "a"}), Labels: map[string]string{"zone": "a"}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-db", 1), nlTask("t2", "s-db", 2), nlTask("t3", "s-web", 1)},
		})
		want := []nlRowKey{{"db", "node.labels.role==db", true, false}}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
		if b.NoLongerMatches != 1 || b.TasksMayMove != 2 || b.Removed != 1 {
			t.Errorf("body = %+v, want one service, two tasks that may move, one removed", b)
		}
	})

	t.Run("a service whose constraints name no changed label is not listed", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"owner": "me"}), Labels: map[string]string{"owner": "you"}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-web", 1)},
		})
		if len(b.Rows) != 0 || b.Rows == nil || b.TasksMayMove != 0 || b.Changed != 1 {
			t.Errorf("body = %+v, want an empty (never null) list and one changed", b)
		}
	})

	t.Run("a constraint on a label that is changed but still met is listed as still matching", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"disk": "ssd"}), Labels: map[string]string{"disk": "SSD"}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-cache", 1)},
		})
		want := []nlRowKey{{"cache", "node.labels.disk==ssd", true, true}}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v: constraint values compare without regard to case", got, want)
		}
		if b.NoLongerMatches != 0 || b.TasksMayMove != 0 {
			t.Errorf("body = %+v, want nothing that moves", b)
		}
	})

	t.Run("label names compare byte for byte", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"disk": "ssd"}), Labels: map[string]string{"Disk": "ssd"}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-cache", 1)},
		})
		// disk is gone (the constraint no longer matches) and Disk is new but no constraint names it.
		want := []nlRowKey{{"cache", "node.labels.disk==ssd", true, false}}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
		if b.Added != 1 || b.Removed != 1 {
			t.Errorf("body = %+v, want one added and one removed", b)
		}
	})

	t.Run("a != constraint now matches when its label goes", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"zone": "b"}), Labels: map[string]string{}, Services: svcs,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-not", 1)},
		})
		want := []nlRowKey{{"not", "node.labels.zone!=b", false, true}}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
		if b.TasksMayMove != 0 {
			t.Errorf("tasksMayMove = %d, want 0: nothing unmatched", b.TasksMayMove)
		}
	})

	t.Run("a constraint that does not parse is left out", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{}), Labels: map[string]string{"disk": "ssd"},
			Services: []dockerx.ServiceRow{{ID: "s-bad", Name: "bad", Constraints: []string{"node.labels.disk>=ssd"}}},
		})
		if len(b.Rows) != 0 {
			t.Errorf("rows = %+v, want none", b.Rows)
		}
	})

	t.Run("rows come by service name then constraint, and a service counts once", func(t *testing.T) {
		two := []dockerx.ServiceRow{
			{ID: "s-b", Name: "b", Constraints: []string{"node.labels.y==1", "node.labels.x==1"}},
			{ID: "s-a", Name: "a", Constraints: []string{"node.labels.x==1"}},
		}
		b := nodeLabelImpact(labelImpactInput{
			Node: nlNode(map[string]string{"x": "1", "y": "1"}), Labels: map[string]string{}, Services: two,
			Tasks: []dockerx.TaskRow{nlTask("t1", "s-b", 1), nlTask("t2", "s-a", 1)},
		})
		want := []nlRowKey{
			{"a", "node.labels.x==1", true, false},
			{"b", "node.labels.x==1", true, false},
			{"b", "node.labels.y==1", true, false},
		}
		if got := nlKeys(b); !slices.Equal(got, want) {
			t.Fatalf("rows = %+v, want %+v", got, want)
		}
		if b.NoLongerMatches != 2 || b.TasksMayMove != 2 || b.Removed != 2 {
			t.Errorf("body = %+v, want two services, two tasks, two removed", b)
		}
	})

	t.Run("no change lists nothing", func(t *testing.T) {
		b := nodeLabelImpact(labelImpactInput{Node: nlNode(map[string]string{"a": "1"}), Labels: map[string]string{"a": "1"}, Services: svcs})
		if len(b.Rows) != 0 || b.Rows == nil || b.Added+b.Changed+b.Removed != 0 {
			t.Errorf("body = %+v", b)
		}
	})
}
