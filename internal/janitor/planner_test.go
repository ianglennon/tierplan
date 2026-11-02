// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// t0 is the moment every test judges at.
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// task makes an exited task container of slot slot of service web on node n1. idx orders the
// containers of a group: a larger idx is newer.
func task(id, slot string, idx int) Container {
	taskID := "task-" + id
	return Container{
		ID: id, Name: "/web." + slot + "." + taskID, NodeID: "n1-id", Node: "n1", State: "exited",
		Created: t0.Add(-72 * time.Hour).Add(time.Duration(idx) * time.Hour),
		Labels: map[string]string{
			labelSwarmServiceID: "svc-web", labelSwarmServiceName: "web",
			labelSwarmTaskID: taskID, labelSwarmTaskName: "web." + slot + "." + taskID,
		},
	}
}

// swarmOf is a swarm snapshot of one node and the service web, with the containers cs.
func swarmOf(cs ...Container) Snapshot {
	return Snapshot{
		Swarm: true, ManagerRead: true, Nodes: []Node{{ID: "n1-id", Name: "n1"}},
		Services: map[string]Service{"svc-web": {Name: "web"}}, Containers: cs, ReadAt: t0,
	}
}

// planOf builds the plan of a snapshot under the default policy at t0, inspecting what Candidates
// names the way the server does, with every container finished two days ago.
func planOf(s Snapshot, p Policy) Plan {
	ends := map[string]ContainerEnd{}
	for _, r := range Candidates(s, p) {
		ends[r.ID] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}
	}
	return Build(Input{Snapshot: s, Ends: ends}, p, t0)
}

func refs(items []Item) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Ref)
	}
	return out
}

func TestBuildEmptyPlanHasNonNilLists(t *testing.T) {
	t.Parallel()
	got := Build(Input{Snapshot: Snapshot{}}, DefaultPolicy(), t0)
	if got.Items == nil || got.NotRemoved == nil || got.WithheldNodes == nil {
		t.Fatalf("plan = %+v, want empty lists and not nil, so they serialise as []", got)
	}
}

func TestBuildListsTheOldestBeyondKeepWithRanks(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	got := planOf(swarmOf(cs...), DefaultPolicy())
	if !reflect.DeepEqual(refs(got.Items), []string{"c1", "c2", "c3"}) {
		t.Fatalf("items = %v, want the three oldest", refs(got.Items))
	}
	for i, it := range got.Items {
		if it.Class != ClassRemovable || it.Reason.Code != ReasonTaskBeyondKeep || it.Reason.Rank != i+1 || it.Reason.Slot != "1" || it.Reason.Service != "web" {
			t.Errorf("item %d = %+v", i, it)
		}
	}
	if len(got.NotRemoved) != 0 {
		t.Errorf("notRemoved = %+v, want none", got.NotRemoved)
	}
}

func TestBuildDefaultPolicy(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	if !p.Enabled || p.Schedule != "0 3 * * *" || p.KeepN != 2 || p.Grace != 24*time.Hour || p.RemoveUnusedNetworks || p.RemoveStoppedContainers {
		t.Fatalf("default policy = %+v", p)
	}
}

func TestBuildIsDeterministicAndOrdered(t *testing.T) {
	t.Parallel()
	mk := func(id, name, node string) Container {
		c := task(id, "1", 0)
		c.Name, c.Node = name, node
		c.Labels[labelSwarmServiceID] = "svc-" + id
		c.Labels[labelSwarmTaskName] = "web.1.task-" + id
		return c
	}
	cs := []Container{mk("z9", "b", "n2"), mk("a1", "b", "n1"), mk("a2", "a", "n1"), mk("a0", "b", "n1")}
	s := swarmOf(cs...)
	s.Services = map[string]Service{}
	for _, c := range cs {
		s.Services[c.Labels[labelSwarmServiceID]] = Service{Name: "web"}
	}
	p := DefaultPolicy()
	p.KeepN = 0
	first := planOf(s, p)
	// Same objects, other order of reading.
	rev := swarmOf(cs[3], cs[2], cs[1], cs[0])
	rev.Services = s.Services
	second := planOf(rev, p)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("plans differ with the reading order:\n%+v\n%+v", first.Items, second.Items)
	}
	if got, want := refs(first.Items), []string{"a2", "a0", "a1", "z9"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (node, then name, then ID)", got, want)
	}
}

func TestBuildNamesTheNodesThatDidNotReport(t *testing.T) {
	t.Parallel()
	s := swarmOf(task("c1", "1", 0))
	s.NotReporting = []string{"w2", "w1"}
	s.Nodes = []Node{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}, {ID: "c", Name: "c"}}
	got := Build(Input{Snapshot: s}, DefaultPolicy(), t0)
	if !reflect.DeepEqual(got.WithheldNodes, []string{"w1", "w2"}) || got.NodesChecked != 3 {
		t.Fatalf("withheld = %v checked = %d, want w1 w2 and 3", got.WithheldNodes, got.NodesChecked)
	}
}

func TestPlanSummaryEstimate(t *testing.T) {
	t.Parallel()
	p := Plan{Items: []Item{
		{Type: KindContainer, Size: 100}, {Type: KindContainer, Size: -1}, {Type: KindNetwork, Size: -1}, {Type: KindImage, Size: 50},
	}}
	c, n, i, est, known := p.Summary()
	if c != 2 || n != 1 || i != 1 || est != 150 || !known {
		t.Fatalf("summary = %d/%d/%d est %d known %v, want 2/1/1 and 150", c, n, i, est, known)
	}
	if _, _, _, est, known := (Plan{Items: []Item{{Type: KindContainer, Size: -1}}}).Summary(); known || est != 0 {
		t.Fatalf("est = %d known = %v, want unknown when no size is", est, known)
	}
	if _, _, _, est, known := (Plan{Items: []Item{{Type: KindContainer, Size: 0}}}).Summary(); !known || est != 0 {
		t.Fatalf("est = %d known = %v, want a known zero", est, known)
	}
}

func TestReasonCodesAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[ReasonCode]bool{}
	for _, c := range Reasons() {
		if c == "" || seen[c] {
			t.Fatalf("reason code %q is empty or listed twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 24 {
		t.Fatalf("%d reason codes, want the 24 the API enum promises", len(seen))
	}
}

// The clock skew rail (plan 11-12, RESEARCH Pitfall 11, A9): a node whose engine clock differs from
// the server's by more than five minutes has its container and image candidates left alone.
func TestClockSkewRail(t *testing.T) {
	t.Parallel()
	// Each of two nodes holds a plain stopped container and a dangling image, all two days old.
	build := func(n1 time.Time, mutate func(*Snapshot)) Plan {
		c1, c2 := box("c1", 0, nil), box("c2", 0, nil)
		c2.NodeID, c2.Node = "n2-id", "n2"
		i1, i2 := bareImage("a1", 48*time.Hour), bareImage("a2", 48*time.Hour)
		i2.NodeID, i2.Node = "n2-id", "n2"
		s := hostOf(nil, c1, c2)
		s.Nodes = append(s.Nodes, Node{ID: "n2-id", Name: "n2"})
		s.Images = []Image{i1, i2}
		s.NodeTimes = map[string]time.Time{"n2-id": t0}
		if !n1.IsZero() {
			s.NodeTimes["n1-id"] = n1
		}
		if mutate != nil {
			mutate(&s)
		}
		return planOf(s, optIn(true))
	}
	byNode := func(items []Item) map[string][]string {
		out := map[string][]string{}
		for _, it := range items {
			out[it.Node] = append(out[it.Node], string(it.Type)+":"+it.Name)
		}
		return out
	}
	t.Run("six minutes ahead: its container and image are cant_tell clock_skew, the other node is judged normally", func(t *testing.T) {
		t.Parallel()
		got := build(t0.Add(6*time.Minute), nil)
		if !reflect.DeepEqual(byNode(got.Items), map[string][]string{"n2": {"container:c2", "image:a2"}}) {
			t.Fatalf("items = %+v, want only the other node's container and image", got.Items)
		}
		if len(got.NotRemoved) != 2 {
			t.Fatalf("notRemoved = %+v, want the skewed node's container and image", got.NotRemoved)
		}
		for _, it := range got.NotRemoved {
			if it.Node != "n1" || it.Class != ClassCantTell || it.Reason.Code != ReasonClockSkew || it.Age != 0 || it.Reason.Age != 0 || it.Reason.Remaining != 0 {
				t.Errorf("item = %+v, want cant_tell clock_skew with no age on n1", it)
			}
		}
	})
	t.Run("six minutes behind is as wrong as six minutes ahead", func(t *testing.T) {
		t.Parallel()
		got := build(t0.Add(-6*time.Minute), nil)
		if len(got.Items) != 2 || len(got.NotRemoved) != 2 || got.NotRemoved[0].Reason.Code != ReasonClockSkew {
			t.Fatalf("plan = %+v, want n1's two items held back", got)
		}
	})
	t.Run("four minutes out, exactly five and an unknown clock are judged normally", func(t *testing.T) {
		t.Parallel()
		for name, n1 := range map[string]time.Time{"4m ahead": t0.Add(4 * time.Minute), "4m behind": t0.Add(-4 * time.Minute), "exactly 5m": t0.Add(ClockSkewTolerance), "unknown": {}} {
			got := build(n1, nil)
			if len(got.Items) != 4 || len(got.NotRemoved) != 0 {
				t.Errorf("%s: plan = %+v, want all four removable", name, got)
			}
		}
	})
	t.Run("a snapshot with no read time cannot tell", func(t *testing.T) {
		t.Parallel()
		got := build(t0.Add(time.Hour), func(s *Snapshot) { s.ReadAt = time.Time{} })
		if len(got.Items) != 4 {
			t.Fatalf("plan = %+v, want all four judged normally", got)
		}
	})
	t.Run("a kept container stays protected while the rest of the node is cant_tell", func(t *testing.T) {
		t.Parallel()
		got := build(t0.Add(time.Hour), func(s *Snapshot) {
			s.Containers = append(s.Containers, box("c4", 0, map[string]string{keepLabel: "true"}))
		})
		classes := map[string]Class{}
		for _, it := range got.NotRemoved {
			if it.Node == "n1" {
				classes[it.Name] = it.Class
			}
		}
		if want := map[string]Class{"c1": ClassCantTell, "a1": ClassCantTell, "c4": ClassProtected}; !reflect.DeepEqual(classes, want) {
			t.Fatalf("classes of n1 = %v, want %v", classes, want)
		}
	})
	t.Run("a container held back by grace is cant_tell too: its age is the wrong clock's", func(t *testing.T) {
		t.Parallel()
		c := box("c1", 0, nil)
		s := hostOf(nil, c)
		s.NodeTimes = map[string]time.Time{"n1-id": t0.Add(time.Hour)}
		got := Build(Input{Snapshot: s, Ends: map[string]ContainerEnd{"c1": {FinishedAt: t0.Add(-time.Hour), SizeRw: -1}}}, optIn(true), t0)
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassCantTell || got.NotRemoved[0].Reason.Code != ReasonClockSkew || got.NotRemoved[0].Reason.Remaining != 0 {
			t.Fatalf("plan = %+v, want cant_tell clock_skew in place of grace", got)
		}
	})
	t.Run("opt-in off: the reported container is held back as well", func(t *testing.T) {
		t.Parallel()
		c := box("c1", 0, nil)
		s := hostOf(nil, c)
		s.NodeTimes = map[string]time.Time{"n1-id": t0.Add(time.Hour)}
		got := planOf(s, optIn(false))
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassCantTell || got.NotRemoved[0].Reason.Code != ReasonClockSkew {
			t.Fatalf("plan = %+v, want cant_tell clock_skew in place of reported", got)
		}
	})
	t.Run("a swarm task container on a skewed node is held back too", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(task("c1", "1", 0), task("c2", "1", 1), task("c3", "1", 2))
		s.NodeTimes = map[string]time.Time{"n1-id": t0.Add(10 * time.Minute)}
		p := DefaultPolicy()
		p.KeepN = 0
		got := planOf(s, p)
		if len(got.Items) != 0 || len(got.NotRemoved) != 3 {
			t.Fatalf("plan = %+v, want all three cant_tell", got)
		}
		for _, it := range got.NotRemoved {
			if it.Reason.Code != ReasonClockSkew || it.Reason.Service != "web" || it.Reason.Slot != "1" {
				t.Errorf("item = %+v, want clock_skew keeping its service and slot", it)
			}
		}
	})
}
