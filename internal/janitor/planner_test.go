// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/stacks"
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
	if len(seen) != 25 {
		t.Fatalf("%d reason codes, want the 25 the API enum promises", len(seen))
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

// D-12: a container or image with a keep row is never removable, whatever the rules say, and is listed
// protected (kept_row) with its place in the group kept; a label's own, more specific, reason stays, and a
// keep of another kind or another ref changes nothing. A keep stored against another node ID still holds
// (WR-06): a container's ID is unique across hosts and an image's ID is its content, and an agent enrolled
// again gives its node a new ID, so a keep that stopped matching would lapse in silence and the next run
// would remove what a person kept. Candidates does not inspect it.
func TestBuildKeptRowProtects(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	labelled := task("c0", "1", -1)
	labelled.Labels[keepLabel] = "true"
	cs = append(cs, labelled)
	s := swarmOf(cs...)
	img := bareImage("ia", 3*24*time.Hour)
	img2 := bareImage("ib", 3*24*time.Hour)
	s.Images = []Image{img, img2}
	s.ServicesRead = true
	s.Keeps = map[KeepKey]bool{
		{Kind: KindContainer, NodeID: "n1-id", Ref: "c1"}:          true,
		{Kind: KindContainer, NodeID: "n1-id", Ref: "c0"}:          true,
		{Kind: KindContainer, NodeID: "re-enrolled", Ref: "c2"}:    true, // c2, kept under its node's old agent ID
		{Kind: KindImage, NodeID: "re-enrolled", Ref: "sha256:ia"}: true,
		{Kind: KindNetwork, NodeID: "n1-id", Ref: "c3"}:            true, // a network, not the container
		{Kind: KindImage, NodeID: img.NodeID, Ref: "c4"}:           true, // an image, not the container
	}

	for _, r := range Candidates(s, DefaultPolicy()) {
		if r.ID == "c1" || r.ID == "c2" {
			t.Errorf("candidate %+v is kept: nothing of it is inspected", r)
		}
	}
	got := planOf(s, DefaultPolicy())
	if want := []string{"c3", "sha256:ib"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v: the kept c1 and c2 and the kept image are left", refs(got.Items), want)
	}
	byRef := map[string]Item{}
	for _, it := range got.NotRemoved {
		byRef[it.Ref] = it
	}
	for _, id := range []string{"c1", "c2", "sha256:ia"} {
		it, ok := byRef[id]
		if !ok || it.Class != ClassProtected || it.Reason.Code != ReasonKeptRow || it.Age != 0 {
			t.Errorf("%s = %+v (listed %v), want protected kept_row", id, it, ok)
		}
	}
	if it := byRef["c1"]; it.Reason.Service != "web" || it.Reason.Slot != "1" {
		t.Errorf("c1 reason = %+v, want it to keep naming its service and slot", it.Reason)
	}
	if it := byRef["c0"]; it.Class != ClassProtected || it.Reason.Code != ReasonKeptLabel {
		t.Errorf("c0 = %+v, want the keep label's own, more specific reason", it)
	}
}

// D-10: the objects a stack excluded from the janitor owns are never removable. Its old one-offs (the only
// containers of a stack the janitor would otherwise take) are listed protected (stack_excluded), found by its ID or
// by its name as the compose project, and none of them is inspected; a stack that is not excluded, and a one-off
// of a project nobody manages, are judged as before.
func TestPlannerExcludedOneOffs(t *testing.T) {
	t.Parallel()
	oneoff := func(id, project string, idx int) Container { return box(id, idx, composed(project, "migrate", true)) }
	// q1 to q3 carry the stack's ID under a project name it does not own: the ID finds them.
	byID := func(id string, idx int) Container {
		return box(id, idx, withStack(composed("renamed", "migrate", true), "stk-shop"))
	}
	s := hostOf([]string{"shop", "blog"},
		oneoff("r1", "shop", 0), oneoff("r2", "shop", 1), oneoff("r3", "shop", 2),
		byID("q1", 0), byID("q2", 1), byID("q3", 2),
		oneoff("b1", "blog", 0), oneoff("b2", "blog", 1), oneoff("b3", "blog", 2))
	s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Excluded: true}, {ID: "stk-blog", Name: "blog"}}
	p := DefaultPolicy()

	for _, c := range Candidates(s, p) {
		if c.ID[0] == 'r' || c.ID[0] == 'q' {
			t.Errorf("candidate %+v belongs to an excluded stack: nothing of it is inspected", c)
		}
	}
	got := planOf(s, p)
	if want := []string{"b1"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v: only the stack that is not excluded loses its oldest one-off", refs(got.Items), want)
	}
	byRef := map[string]Item{}
	for _, it := range got.NotRemoved {
		byRef[it.Ref] = it
	}
	for _, id := range []string{"r1", "q1"} {
		it, ok := byRef[id]
		if !ok || it.Class != ClassProtected || it.Reason.Code != ReasonStackExcluded || it.Age != 0 {
			t.Errorf("%s = %+v (listed %v), want protected stack_excluded", id, it, ok)
		}
	}
	if it := byRef["r1"]; it.Reason.Project != "shop" || it.Reason.Service != "migrate" {
		t.Errorf("r1 reason = %+v, want it to keep naming its project and service", it.Reason)
	}
	// The newest two of each group (keep-N) are not the janitor's business, excluded or not.
	for _, id := range []string{"r2", "r3", "q2", "q3"} {
		if it, ok := byRef[id]; ok {
			t.Errorf("%s is listed %+v, want it left out as one of the newest", id, it)
		}
	}

	// Including the stack makes the same one-offs removable again.
	s.StackFacts[0].Excluded = false
	if got := planOf(s, p); !reflect.DeepEqual(refs(got.Items), []string{"b1", "q1", "r1"}) {
		t.Fatalf("items = %v, want r1, q1 and b1 removable once the stack is included", refs(got.Items))
	}
}

// A person's own keep and a label stay more specific than the stack's exclusion, and a swarm task container is found
// by the stack namespace the same way.
func TestPlannerExcludedSwarmTasks(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		c := task(fmt.Sprintf("c%d", i+1), "1", i)
		c.Labels[stacks.LabelSwarmNamespace] = "shop"
		cs = append(cs, c)
	}
	s := swarmOf(cs...)
	s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Excluded: true}}
	s.Keeps = map[KeepKey]bool{{Kind: KindContainer, NodeID: "n1-id", Ref: "c1"}: true}
	got := planOf(s, DefaultPolicy())
	if len(got.Items) != 0 {
		t.Fatalf("items = %v, want none: the stack is excluded", refs(got.Items))
	}
	want := map[string]ReasonCode{"c1": ReasonKeptRow, "c2": ReasonStackExcluded, "c3": ReasonStackExcluded}
	if len(got.NotRemoved) != len(want) {
		t.Fatalf("notRemoved = %+v, want %d protected", got.NotRemoved, len(want))
	}
	for _, it := range got.NotRemoved {
		if it.Class != ClassProtected || it.Reason.Code != want[it.Ref] {
			t.Errorf("%s = %+v, want protected %s", it.Ref, it, want[it.Ref])
		}
	}
}

// D-10: the networks of a stack are owned by it, whether or not it is excluded, so the orphan check never lists them
// for the janitor; excluding a stack adds nothing to the networks and takes nothing from them.
func TestPlannerExcludedStackNetworksOwned(t *testing.T) {
	t.Parallel()
	rep := &NetworkReport{Complete: true, Rows: []NetworkRow{
		{ID: "net-own", Name: "shop_default", NodeID: "n1-id", Node: "n1", Scope: "local", Class: netspace.ClassOwned, FormerOwner: "shop", FormerOwnerKind: "stack"},
		{ID: "net-old", Name: "old_default", NodeID: "n1-id", Node: "n1", Scope: "local", Class: netspace.ClassOrphaned, FormerOwner: "old", FormerOwnerKind: "stack", IdleSince: t0.Add(-48 * time.Hour)},
	}}
	for _, excluded := range []bool{false, true} {
		s := hostOf([]string{"shop"})
		s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Excluded: excluded}}
		got := Build(Input{Snapshot: s, Networks: rep}, DefaultPolicy(), t0)
		if !reflect.DeepEqual(refs(got.Items), []string{"net-old"}) || len(got.NotRemoved) != 0 {
			t.Errorf("excluded %v: plan = %+v, want only the orphaned network listed and the stack's own never", excluded, got)
		}
	}
}

// D-10, D-19: the objects of a stack whose compose file says x-tierplan: {keep: true} are never removable, exactly
// as if a person had excluded it, but with the reason stack_declared_keep. Its old one-offs and its swarm task
// containers are listed protected and none is inspected; a stack that does not declare is judged as before.
func TestPlannerDeclaredStack(t *testing.T) {
	t.Parallel()
	oneoff := func(id, project string, idx int) Container { return box(id, idx, composed(project, "migrate", true)) }
	s := hostOf([]string{"shop", "blog"},
		oneoff("r1", "shop", 0), oneoff("r2", "shop", 1), oneoff("r3", "shop", 2),
		oneoff("b1", "blog", 0), oneoff("b2", "blog", 1), oneoff("b3", "blog", 2))
	s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Declared: true}, {ID: "stk-blog", Name: "blog"}}
	p := DefaultPolicy()

	for _, c := range Candidates(s, p) {
		if c.ID[0] == 'r' {
			t.Errorf("candidate %+v belongs to a declared stack: nothing of it is inspected", c)
		}
	}
	got := planOf(s, p)
	if want := []string{"b1"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v: only the stack that does not declare loses its oldest one-off", refs(got.Items), want)
	}
	if len(got.NotRemoved) != 1 {
		t.Fatalf("notRemoved = %+v, want only r1", got.NotRemoved)
	}
	it := got.NotRemoved[0]
	if it.Ref != "r1" || it.Class != ClassProtected || it.Reason.Code != ReasonStackDeclaredKeep || it.Age != 0 || it.Reason.Project != "shop" || it.Reason.Service != "migrate" {
		t.Errorf("r1 = %+v, want protected stack_declared_keep still naming its project and service", it)
	}

	// The same for the task containers of a swarm stack found by its namespace, with a person's keep staying more specific.
	var cs []Container
	for i := range 5 {
		c := task(fmt.Sprintf("c%d", i+1), "1", i)
		c.Labels[stacks.LabelSwarmNamespace] = "shop"
		cs = append(cs, c)
	}
	sw := swarmOf(cs...)
	sw.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Declared: true}}
	sw.Keeps = map[KeepKey]bool{{Kind: KindContainer, NodeID: "n1-id", Ref: "c1"}: true}
	got = planOf(sw, p)
	if len(got.Items) != 0 {
		t.Fatalf("swarm items = %v, want none: the stack declares keep", refs(got.Items))
	}
	want := map[string]ReasonCode{"c1": ReasonKeptRow, "c2": ReasonStackDeclaredKeep, "c3": ReasonStackDeclaredKeep}
	if len(got.NotRemoved) != len(want) {
		t.Fatalf("swarm notRemoved = %+v, want %d protected", got.NotRemoved, len(want))
	}
	for _, it := range got.NotRemoved {
		if it.Class != ClassProtected || it.Reason.Code != want[it.Ref] {
			t.Errorf("%s = %+v, want protected %s", it.Ref, it, want[it.Ref])
		}
	}

	// Taking the declaration away makes the one-off removable again.
	s.StackFacts[0].Declared = false
	if got := planOf(s, p); !reflect.DeepEqual(refs(got.Items), []string{"b1", "r1"}) {
		t.Fatalf("items = %v, want r1 and b1 removable once the stack no longer declares keep", refs(got.Items))
	}
}

// A stack whose definition did not load is left alone: what it owns is cant_tell stack_unknown, never removable,
// and nothing of it is inspected. The stack beside it that loaded is judged as usual (T-11-83).
func TestPlannerStackUnknown(t *testing.T) {
	t.Parallel()
	oneoff := func(id, project string, idx int) Container { return box(id, idx, composed(project, "migrate", true)) }
	s := hostOf([]string{"shop", "blog"},
		oneoff("r1", "shop", 0), oneoff("r2", "shop", 1), oneoff("r3", "shop", 2),
		oneoff("b1", "blog", 0), oneoff("b2", "blog", 1), oneoff("b3", "blog", 2))
	s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Unknown: true}, {ID: "stk-blog", Name: "blog"}}
	p := DefaultPolicy()

	for _, c := range Candidates(s, p) {
		if c.ID[0] == 'r' {
			t.Errorf("candidate %+v belongs to a stack that did not load: nothing of it is inspected", c)
		}
	}
	got := planOf(s, p)
	if want := []string{"b1"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v", refs(got.Items), want)
	}
	if len(got.NotRemoved) != 1 {
		t.Fatalf("notRemoved = %+v, want only r1", got.NotRemoved)
	}
	if it := got.NotRemoved[0]; it.Ref != "r1" || it.Class != ClassCantTell || it.Reason.Code != ReasonStackUnknown || it.Reason.Project != "shop" {
		t.Errorf("r1 = %+v, want cant_tell stack_unknown naming its project", it)
	}

	// The swarm task containers of an unreadable stack too, and a label that protects one keeps its own reason.
	var cs []Container
	for i := range 5 {
		c := task(fmt.Sprintf("c%d", i+1), "1", i)
		c.Labels[stacks.LabelSwarmNamespace] = "shop"
		if i == 0 {
			c.Labels[stacks.LabelKeep] = "true"
		}
		cs = append(cs, c)
	}
	sw := swarmOf(cs...)
	sw.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Unknown: true}}
	got = planOf(sw, p)
	if len(got.Items) != 0 {
		t.Fatalf("swarm items = %v, want none: the stack did not load", refs(got.Items))
	}
	for _, it := range got.NotRemoved {
		switch it.Ref {
		case "c1":
			if it.Class != ClassProtected || it.Reason.Code != ReasonKeptLabel {
				t.Errorf("c1 = %+v, want the keep label's own protected reason", it)
			}
		default:
			if it.Class != ClassCantTell || it.Reason.Code != ReasonStackUnknown {
				t.Errorf("%s = %+v, want cant_tell stack_unknown", it.Ref, it)
			}
		}
	}
}

// A stack both excluded by the switch and declared by its file is worded stack_excluded: the switch is named
// first. A stack that is declared and another that is unreadable, matching one object between them, give the
// declared one, which is a statement and not a failure.
func TestPlannerExcludedAndDeclared(t *testing.T) {
	t.Parallel()
	oneoff := func(id, project string, idx int) Container { return box(id, idx, composed(project, "migrate", true)) }
	s := hostOf([]string{"shop"}, oneoff("r1", "shop", 0), oneoff("r2", "shop", 1), oneoff("r3", "shop", 2))
	p := DefaultPolicy()
	for name, facts := range map[string][]StackFact{
		"excluded and declared": {{ID: "stk-shop", Name: "shop", Excluded: true, Declared: true}},
		"two stacks":            {{ID: "stk-shop", Name: "shop", Declared: true}, {ID: "stk-other", Name: "shop", Excluded: true}},
	} {
		s.StackFacts = facts
		got := planOf(s, p)
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 {
			t.Fatalf("%s: plan = %+v, want nothing removable and r1 protected", name, got)
		}
		if it := got.NotRemoved[0]; it.Class != ClassProtected || it.Reason.Code != ReasonStackExcluded {
			t.Errorf("%s: r1 = %+v, want protected stack_excluded (the switch is named first)", name, it)
		}
	}
	s.StackFacts = []StackFact{{ID: "stk-shop", Name: "shop", Declared: true}, {ID: "stk-bad", Name: "shop", Unknown: true}}
	got := planOf(s, p)
	if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Reason.Code != ReasonStackDeclaredKeep {
		t.Errorf("declared beside unknown: plan = %+v, want r1 protected stack_declared_keep", got)
	}
}
