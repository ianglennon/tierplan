// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestKeepNReplicatedSlot(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	for _, tc := range []struct {
		keep int
		want []string
	}{
		{keep: 2, want: []string{"c1", "c2", "c3"}},
		{keep: 0, want: []string{"c1", "c2", "c3", "c4", "c5"}},
		{keep: -3, want: []string{"c1", "c2", "c3", "c4", "c5"}},
		{keep: 5, want: []string{}},
		{keep: 20, want: []string{}},
	} {
		t.Run(fmt.Sprintf("keep=%d", tc.keep), func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.KeepN = tc.keep
			got := planOf(swarmOf(cs...), p)
			if !reflect.DeepEqual(refs(got.Items), tc.want) {
				t.Fatalf("items = %v, want %v", refs(got.Items), tc.want)
			}
			for i, it := range got.Items {
				if it.Reason.Rank != i+1 {
					t.Errorf("item %s rank = %d, want %d (1 is the oldest)", it.Ref, it.Reason.Rank, i+1)
				}
			}
		})
	}
	t.Run("slots are separate groups", func(t *testing.T) {
		t.Parallel()
		var all []Container
		for i := range 3 {
			all = append(all, task(fmt.Sprintf("a%d", i), "1", i), task(fmt.Sprintf("b%d", i), "2", i))
		}
		got := planOf(swarmOf(all...), DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"a0", "b0"}) {
			t.Fatalf("items = %v, want the oldest of each slot", refs(got.Items))
		}
		if got.Items[0].Reason.Slot != "1" || got.Items[1].Reason.Slot != "2" {
			t.Errorf("slots = %q %q", got.Items[0].Reason.Slot, got.Items[1].Reason.Slot)
		}
	})
	t.Run("a container that is not exited does not count towards the newest", func(t *testing.T) {
		t.Parallel()
		running := task("live", "1", 9)
		running.State = "running"
		got := planOf(swarmOf(task("c1", "1", 0), task("c2", "1", 1), task("c3", "1", 2), running), DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"c1"}) {
			t.Fatalf("items = %v, want only c1: two exited ones are kept besides the running one", refs(got.Items))
		}
	})
	t.Run("the same second is ordered by ID", func(t *testing.T) {
		t.Parallel()
		a, b, c := task("b", "1", 0), task("a", "1", 0), task("c", "1", 0)
		p := DefaultPolicy()
		p.KeepN = 1
		got := planOf(swarmOf(a, b, c), p)
		if !reflect.DeepEqual(refs(got.Items), []string{"a", "b"}) {
			t.Fatalf("items = %v, want a and b: c is the newest of equal times", refs(got.Items))
		}
	})
}

// globalTask makes an exited task container of the global service web on node node.
func globalTask(id, node string, idx int) Container {
	c := task(id, node, idx)
	c.NodeID, c.Node = node+"-id", node
	c.Labels[labelSwarmTaskName] = "web." + node + "-id." + "task-" + id
	return c
}

func TestKeepNGlobalPerNode(t *testing.T) {
	t.Parallel()
	cs := []Container{
		globalTask("a1", "A", 0), globalTask("a2", "A", 1), globalTask("a3", "A", 2),
		globalTask("b1", "B", 0),
	}
	for name, mutate := range map[string]func(s *Snapshot){
		"the manager lists the service as global": func(s *Snapshot) { s.Services["svc-web"] = Service{Name: "web", Global: true} },
		"no manager answered":                     func(s *Snapshot) { s.ManagerRead, s.Services = false, nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := swarmOf(cs...)
			s.Nodes = []Node{{ID: "A-id", Name: "A"}, {ID: "B-id", Name: "B"}}
			mutate(&s)
			got := planOf(s, DefaultPolicy())
			if !reflect.DeepEqual(refs(got.Items), []string{"a1"}) {
				t.Fatalf("items = %v, want only the oldest on node A", refs(got.Items))
			}
			it := got.Items[0]
			if it.Reason.Slot != "" || it.Reason.Node != "A" || it.Reason.Rank != 1 || it.Reason.Service != "web" {
				t.Errorf("reason = %+v, want no slot, node A, rank 1", it.Reason)
			}
		})
	}
	t.Run("a global service whose node ID is all digits is still per node", func(t *testing.T) {
		t.Parallel()
		c := globalTask("d1", "A", 0)
		c.Labels[labelSwarmTaskName] = "web.1234567890.task-d1"
		s := swarmOf(c)
		s.Services["svc-web"] = Service{Name: "web", Global: true}
		key, slot, ok := taskGroup(c, true)
		if !ok || slot != "" || key != "svc-web/node/A-id" {
			t.Fatalf("group = %q %q %v", key, slot, ok)
		}
	})
}

func TestTaskGroupParsing(t *testing.T) {
	t.Parallel()
	mk := func(svc, name string) Container {
		c := task("c1", "1", 0)
		c.Labels[labelSwarmServiceName] = svc
		c.Labels[labelSwarmTaskName] = name
		return c
	}
	for _, tc := range []struct {
		name         string
		c            Container
		key, slot    string
		wantOK       bool
		globalForced bool
	}{
		{name: "replicated", c: mk("web", "web.3.task-c1"), key: "svc-web/slot/3", slot: "3", wantOK: true},
		{name: "a service name with dots", c: mk("my.web.app", "my.web.app.12.task-c1"), key: "svc-web/slot/12", slot: "12", wantOK: true},
		{name: "global (node ID)", c: mk("web", "web.x8f2kq.task-c1"), key: "svc-web/node/n1-id", wantOK: true},
		{name: "wrong prefix", c: mk("web", "db.3.task-c1")},
		{name: "wrong suffix", c: mk("web", "web.3.task-zz")},
		{name: "no middle", c: mk("web", "web..task-c1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key, slot, ok := taskGroup(tc.c, tc.globalForced)
			if ok != tc.wantOK || key != tc.key || slot != tc.slot {
				t.Fatalf("taskGroup = %q %q %v, want %q %q %v", key, slot, ok, tc.key, tc.slot, tc.wantOK)
			}
		})
	}
}

func TestCandidatesAreOnlyThoseBeyondKeep(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	kept := task("k", "1", -1)
	kept.Labels[keepLabel] = "true"
	cs = append(cs, kept)
	live := task("live", "1", 9)
	live.State = "running"
	cs = append(cs, live)
	got := Candidates(swarmOf(cs...), DefaultPolicy())
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	// The kept one is excluded from the inspect, and the running one is not ranked. The oldest
	// first, then keep-N from the newest: c1..c3 are beyond the newest two of the five.
	if !reflect.DeepEqual(ids, []string{"c1", "c2", "c3"}) {
		t.Fatalf("candidates = %v, want c1 c2 c3 only", ids)
	}
}

func TestKeepNOneOffs(t *testing.T) {
	t.Parallel()
	oneoff := func(id string, idx int, project, service string) Container {
		return box(id, idx, composed(project, service, true))
	}
	var cs []Container
	for i := range 4 {
		cs = append(cs, oneoff(fmt.Sprintf("m%d", i+1), i, "shop", "migrate"))
	}
	// The same project's other service, another project's service of the same name and the same
	// service on another node are groups of their own.
	other := []Container{oneoff("s1", 0, "shop", "seed"), oneoff("o1", 0, "blog", "migrate")}
	elsewhere := oneoff("e1", 0, "shop", "migrate")
	elsewhere.NodeID, elsewhere.Node = "n2-id", "n2"
	for _, tc := range []struct {
		keep int
		want []string
	}{
		{keep: 2, want: []string{"m1", "m2"}},
		{keep: 0, want: []string{"e1", "m1", "m2", "m3", "m4", "o1", "s1"}},
		{keep: 1, want: []string{"m1", "m2", "m3"}},
		{keep: 4, want: []string{}},
		{keep: -1, want: []string{"e1", "m1", "m2", "m3", "m4", "o1", "s1"}},
	} {
		t.Run(fmt.Sprintf("keep=%d", tc.keep), func(t *testing.T) {
			t.Parallel()
			p := DefaultPolicy()
			p.KeepN = tc.keep
			s := hostOf([]string{"shop"}, append(append([]Container{}, cs...), append(other, elsewhere)...)...)
			s.Nodes = append(s.Nodes, Node{ID: "n2-id", Name: "n2"})
			got := planOf(s, p)
			ids := refs(got.Items)
			slices.Sort(ids)
			if !reflect.DeepEqual(ids, tc.want) || len(got.NotRemoved) != 0 {
				t.Fatalf("items = %v notRemoved = %+v, want %v", ids, got.NotRemoved, tc.want)
			}
			for _, it := range got.Items {
				if it.Reason.Code != ReasonComposeOneoff || it.Class != ClassRemovable {
					t.Errorf("item = %+v, want removable compose_oneoff", it)
				}
			}
		})
	}
	t.Run("the oldest are the ones removed, with their rank", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf([]string{"shop"}, cs...), DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"m1", "m2"}) {
			t.Fatalf("items = %v, want the two oldest", refs(got.Items))
		}
		for i, it := range got.Items {
			if it.Reason.Rank != i+1 || it.Reason.Project != "shop" || it.Reason.Service != "migrate" {
				t.Errorf("item = %+v, want rank %d of (shop, migrate)", it, i+1)
			}
		}
	})
	t.Run("a one-off still running does not count towards keep-N", func(t *testing.T) {
		t.Parallel()
		live := append([]Container{}, cs...)
		live = append(live, inState(oneoff("m5", 9, "shop", "migrate"), "running"))
		if got := planOf(hostOf([]string{"shop"}, live...), DefaultPolicy()); !reflect.DeepEqual(refs(got.Items), []string{"m1", "m2"}) {
			t.Fatalf("items = %v, want the two oldest exited ones", refs(got.Items))
		}
	})
	t.Run("a one-off in a project nobody manages goes the same way, even beside a service container", func(t *testing.T) {
		t.Parallel()
		svc := box("web1", 0, composed("shop", "web", false))
		got := planOf(hostOf(nil, append([]Container{svc}, cs...)...), DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"m1", "m2"}) || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want the two oldest one-offs and nothing said of the service container", got)
		}
	})
}

// planWithNodes is planOf where the swarm nodes in nodes have settings of their own.
func planWithNodes(s Snapshot, p Policy, nodes map[string]NodeOverrides) Plan {
	ends := map[string]ContainerEnd{}
	for _, r := range CandidatesFor(s, p, nodes) {
		ends[r.ID] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}
	}
	return Build(Input{Snapshot: s, Ends: ends, Nodes: nodes}, p, t0)
}

// onNode puts a task container on the node name, whose snapshot ID is name+"-id".
func onNode(c Container, name string) Container {
	c.NodeID, c.Node = name+"-id", name
	return c
}

// twoNodes is a swarm snapshot of the nodes A and B, whose snapshot IDs map to the swarm node IDs swarm-a and
// swarm-b the way the server's engine maps an agent to its node.
func twoNodes(cs ...Container) Snapshot {
	s := swarmOf(cs...)
	s.Nodes = []Node{{ID: "A-id", Name: "A"}, {ID: "B-id", Name: "B"}}
	s.SwarmNodeOf = map[string]string{"A-id": "swarm-a", "B-id": "swarm-b"}
	return s
}

// sortedRefs is refs in ID order: a plan lists by node first, and these tests are about which, not where.
func sortedRefs(items []Item) []string {
	out := refs(items)
	slices.Sort(out)
	return out
}

func intp(i int) *int    { return &i }
func boolp(b bool) *bool { return &b }

// D-04: a node's keep-N replaces the environment's for the containers on that node. The slot is ranked across
// the nodes it ran on, and each container is judged against the N of its own node.
func TestKeepNNodeOverride(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 6 {
		node := "B"
		if i >= 3 {
			node = "A"
		}
		cs = append(cs, onNode(task(fmt.Sprintf("c%d", i), "1", i), node))
	}
	t.Run("a node that keeps more keeps its newest, the other follows the environment", func(t *testing.T) {
		t.Parallel()
		got := planWithNodes(twoNodes(cs...), DefaultPolicy(), map[string]NodeOverrides{"swarm-a": {KeepN: intp(5)}})
		if want := []string{"c0", "c1", "c2"}; !reflect.DeepEqual(refs(got.Items), want) {
			t.Fatalf("items = %v, want %v: node A's three are among the newest 5 of the slot, node B's three are not among the newest 2", refs(got.Items), want)
		}
	})
	t.Run("a node that keeps fewer lets its containers go", func(t *testing.T) {
		t.Parallel()
		got := planWithNodes(twoNodes(cs...), DefaultPolicy(), map[string]NodeOverrides{"swarm-a": {KeepN: intp(0)}})
		if want := []string{"c0", "c1", "c2", "c3", "c4", "c5"}; !reflect.DeepEqual(sortedRefs(got.Items), want) {
			t.Fatalf("items = %v, want %v: node A keeps none, so its three newest go; node B's three are past the newest 2", sortedRefs(got.Items), want)
		}
	})
	t.Run("without overrides the environment's N is every node's", func(t *testing.T) {
		t.Parallel()
		got := planWithNodes(twoNodes(cs...), DefaultPolicy(), nil)
		if want := []string{"c0", "c1", "c2", "c3"}; !reflect.DeepEqual(sortedRefs(got.Items), want) {
			t.Fatalf("items = %v, want %v: the newest two of six stay", sortedRefs(got.Items), want)
		}
	})
	t.Run("a node the snapshot cannot map is looked up by its own ID", func(t *testing.T) {
		t.Parallel()
		s := twoNodes(cs...)
		s.SwarmNodeOf = nil
		got := planWithNodes(s, DefaultPolicy(), map[string]NodeOverrides{"A-id": {KeepN: intp(5)}})
		if want := []string{"c0", "c1", "c2"}; !reflect.DeepEqual(refs(got.Items), want) {
			t.Fatalf("items = %v, want %v", refs(got.Items), want)
		}
	})
	t.Run("a node's keep-N is not inspected beyond what it lets go", func(t *testing.T) {
		t.Parallel()
		var ids []string
		for _, r := range CandidatesFor(twoNodes(cs...), DefaultPolicy(), map[string]NodeOverrides{"swarm-a": {KeepN: intp(5)}}) {
			ids = append(ids, r.ID)
		}
		if want := []string{"c0", "c1", "c2"}; !reflect.DeepEqual(ids, want) {
			t.Fatalf("candidates = %v, want %v", ids, want)
		}
	})
}

// A global service's containers are grouped per node, so a node's keep-N is that group's N.
func TestKeepNGlobalNodeOverride(t *testing.T) {
	t.Parallel()
	cs := []Container{
		globalTask("a1", "A", 0), globalTask("a2", "A", 1), globalTask("a3", "A", 2),
		globalTask("b1", "B", 0), globalTask("b2", "B", 1), globalTask("b3", "B", 2),
	}
	s := twoNodes(cs...)
	s.Services["svc-web"] = Service{Name: "web", Global: true}
	got := planWithNodes(s, DefaultPolicy(), map[string]NodeOverrides{"swarm-a": {KeepN: intp(0)}})
	if want := []string{"a1", "a2", "a3", "b1"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v: node A keeps none, node B keeps its newest two", refs(got.Items), want)
	}
	for _, it := range got.Items {
		if it.Reason.Slot != "" || it.Reason.Node == "" {
			t.Errorf("item %s reason = %+v, want a node group", it.Ref, it.Reason)
		}
	}
}

// A node that sets nothing about keep-N, or no override at all, follows the environment: a changed environment
// N reaches it.
func TestKeepNNodeNullFollows(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 5 {
		cs = append(cs, onNode(task(fmt.Sprintf("c%d", i), "1", i), "A"))
	}
	for name, nodes := range map[string]map[string]NodeOverrides{
		"no override":               nil,
		"an empty override":         {"swarm-a": {}},
		"only enabled is set":       {"swarm-a": {Enabled: boolp(true)}},
		"an override of another":    {"swarm-b": {KeepN: intp(0)}},
		"keep-N null, enabled null": {"swarm-a": {Enabled: nil, KeepN: nil}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, keep := range []int{1, 2, 4} {
				p := DefaultPolicy()
				p.KeepN = keep
				got := planWithNodes(twoNodes(cs...), p, nodes)
				want := planWithNodes(twoNodes(cs...), p, nil)
				if !reflect.DeepEqual(refs(got.Items), refs(want.Items)) || len(got.Items) != 5-keep {
					t.Fatalf("keep %d: items = %v, want %v (the newest %d stay)", keep, refs(got.Items), refs(want.Items), keep)
				}
			}
		})
	}
}

// D-04: a node whose janitor is switched off has what the rules would have judged listed as protected
// (node_disabled) and nothing of it inspected, while the other nodes are cleaned as before.
func TestBuildNodeDisabledProtects(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 4 {
		cs = append(cs, onNode(task(fmt.Sprintf("a%d", i), "1", i), "A"), onNode(task(fmt.Sprintf("b%d", i), "2", i), "B"))
	}
	kept := onNode(task("bk", "2", -1), "B")
	kept.Labels[keepLabel] = "true"
	cs = append(cs, kept)
	img := func(id, node string) Image {
		i := bareImage(id, 3*24*time.Hour)
		i.NodeID, i.Node = node+"-id", node
		return i
	}
	s := twoNodes(cs...)
	s.Images = []Image{img("ia", "A"), img("ib", "B")}
	s.ServicesRead = true
	nodes := map[string]NodeOverrides{"swarm-b": {Enabled: boolp(false)}}

	for _, r := range CandidatesFor(s, DefaultPolicy(), nodes) {
		if r.NodeID == "B-id" {
			t.Errorf("candidate %+v is on the node that is switched off: nothing of it is inspected", r)
		}
	}
	got := planWithNodes(s, DefaultPolicy(), nodes)
	if want := []string{"a0", "a1", "sha256:ia"}; !reflect.DeepEqual(refs(got.Items), want) {
		t.Fatalf("items = %v, want %v: only node A's, its two oldest and its dangling image", refs(got.Items), want)
	}
	protected := map[string]Item{}
	for _, it := range got.NotRemoved {
		if it.NodeID == "B-id" {
			protected[it.Ref] = it
		}
	}
	for _, id := range []string{"b0", "b1", "sha256:ib"} {
		it, ok := protected[id]
		if !ok || it.Class != ClassProtected || it.Reason.Code != ReasonNodeDisabled || it.Age != 0 {
			t.Errorf("%s = %+v (listed %v), want protected node_disabled", id, it, ok)
		}
	}
	if it := protected["b0"]; it.Reason.Service != "web" || it.Reason.Slot != "2" {
		t.Errorf("b0 reason = %+v, want it to keep naming its service and slot", it.Reason)
	}
	if it := protected["bk"]; it.Class != ClassProtected || it.Reason.Code != ReasonKeptLabel {
		t.Errorf("bk = %+v, want the keep label's own, more specific reason", it)
	}
	if _, listed := protected["b2"]; listed {
		t.Errorf("b2 is among the newest two of its slot: it is not the janitor's business, node or no node")
	}
	t.Run("a node that sets itself on, or follows, is cleaned", func(t *testing.T) {
		t.Parallel()
		for _, o := range []NodeOverrides{{}, {Enabled: boolp(true)}, {KeepN: intp(1)}} {
			p := DefaultPolicy()
			p.Enabled = false // the schedule is off; Clean now still cleans every node that is not switched off
			got := planWithNodes(s, p, map[string]NodeOverrides{"swarm-b": o})
			if n := slices.Index(refs(got.Items), "b0"); n < 0 {
				t.Errorf("override %+v: items = %v, want b0 removable", o, refs(got.Items))
			}
		}
	})
}
