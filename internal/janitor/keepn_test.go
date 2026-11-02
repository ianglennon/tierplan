// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"testing"
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
