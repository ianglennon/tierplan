// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// The rules for a swarm task container, in the order ClassifyContainer tries them (plan 11-01,
// D-03, D-05, D-19, JAN-06). Every case judges at t0.

func TestClassifyContainerNeverLiveStates(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"running", "paused", "restarting", "created", "removing", "dead", ""} {
		t.Run("state="+state, func(t *testing.T) {
			t.Parallel()
			c := task("c1", "1", 0)
			c.State = state
			in := ContainerInput{Container: c, Group: "svc-web/slot/1", Slot: "1", Rank: 1, Total: 1, KeepN: 0, Grace: DefaultGrace, Inspected: true,
				End: ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}}
			if item, listed := ClassifyContainer(in, t0); listed {
				t.Fatalf("a %q container was listed: %+v", state, item)
			}
			// A kept one is not listed either: it is not the janitor's business in the first place.
			c.Labels[keepLabel] = "true"
			in.Container = c
			if item, listed := ClassifyContainer(in, t0); listed {
				t.Fatalf("a kept %q container was listed: %+v", state, item)
			}
			// Nor through a whole plan, whatever else is going on.
			s := swarmOf(c)
			p := DefaultPolicy()
			p.KeepN = 0
			got := planOf(s, p)
			if len(got.Items) != 0 || len(got.NotRemoved) != 0 {
				t.Fatalf("plan = %+v, want nothing for a %q container", got, state)
			}
		})
	}
}

const keepLabel = "tierplan.keep"

func TestServiceGone(t *testing.T) {
	t.Parallel()
	var cs []Container
	for i := range 4 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	t.Run("a manager answered and the service is gone: every one past grace goes, keep-N or not", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(cs...)
		s.Services = map[string]Service{"svc-other": {Name: "other"}}
		got := planOf(s, DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"c1", "c2", "c3", "c4"}) {
			t.Fatalf("items = %v, want all four", refs(got.Items))
		}
		for _, it := range got.Items {
			if it.Reason.Code != ReasonTaskServiceGone || it.Class != ClassRemovable {
				t.Errorf("item = %+v, want removable task_service_gone", it)
			}
		}
	})
	t.Run("a gone service's young container waits for grace", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(cs[0])
		s.Services = map[string]Service{}
		got := Build(Input{Snapshot: s, Ends: map[string]ContainerEnd{"c1": {FinishedAt: t0.Add(-time.Hour), SizeRw: -1}}}, DefaultPolicy(), t0)
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassGrace {
			t.Fatalf("plan = %+v, want one container under grace", got)
		}
	})
	t.Run("no manager answered: nothing is judged by whether the service exists, keep-N applies", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(cs...)
		s.ManagerRead, s.Services = false, nil
		got := planOf(s, DefaultPolicy())
		if !reflect.DeepEqual(refs(got.Items), []string{"c1", "c2"}) {
			t.Fatalf("items = %v, want only the two beyond keep-N", refs(got.Items))
		}
		for _, it := range got.Items {
			if it.Reason.Code != ReasonTaskBeyondKeep {
				t.Errorf("item = %+v, want task_beyond_keep", it)
			}
		}
	})
}

func TestDesiredRunningDropped(t *testing.T) {
	t.Parallel()
	c := task("c1", "1", 0)
	p := DefaultPolicy()
	p.KeepN = 0
	t.Run("by task ID", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(c)
		s.Tasks = []Task{{ID: "task-c1", DesiredState: "running"}}
		if got := planOf(s, p); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want nothing: the swarm wants this task running", got)
		}
		if refs := Candidates(s, p); len(refs) != 0 {
			t.Fatalf("candidates = %v, want none: it is not inspected either", refs)
		}
	})
	t.Run("by container ID", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(c)
		s.Tasks = []Task{{ID: "other", ContainerID: "c1", DesiredState: "running"}}
		if got := planOf(s, p); len(got.Items) != 0 {
			t.Fatalf("plan = %+v, want nothing", got)
		}
	})
	t.Run("a task the swarm wants shut down does not protect it", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(c)
		s.Tasks = []Task{{ID: "task-c1", DesiredState: "shutdown"}}
		if got := planOf(s, p); len(got.Items) != 1 {
			t.Fatalf("plan = %+v, want the container listed", got)
		}
	})
}

func TestLabelsMissingIsCantTell(t *testing.T) {
	t.Parallel()
	cases := map[string]func(c *Container){
		"no service ID":    func(c *Container) { delete(c.Labels, labelSwarmServiceID) },
		"no service name":  func(c *Container) { delete(c.Labels, labelSwarmServiceName) },
		"no task name":     func(c *Container) { delete(c.Labels, labelSwarmTaskName) },
		"no task ID":       func(c *Container) { delete(c.Labels, labelSwarmTaskID); c.Labels["com.docker.swarm.node.id"] = "n" },
		"name of another":  func(c *Container) { c.Labels[labelSwarmTaskName] = "db.1.task-c1" },
		"no middle part":   func(c *Container) { c.Labels[labelSwarmTaskName] = "web.task-c1" },
		"task ID mismatch": func(c *Container) { c.Labels[labelSwarmTaskName] = "web.1.someone-else" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := task("c1", "1", 0)
			mutate(&c)
			p := DefaultPolicy()
			p.KeepN = 0
			got := planOf(swarmOf(c), p)
			if len(got.Items) != 0 || len(got.NotRemoved) != 1 {
				t.Fatalf("plan = %+v, want the container left with a reason", got)
			}
			if it := got.NotRemoved[0]; it.Class != ClassCantTell || it.Reason.Code != ReasonTaskLabelsMissing {
				t.Fatalf("item = %+v, want cant_tell task_labels_missing", it)
			}
		})
	}
}

func TestGraceFromFinishedAt(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	build := func(c Container, end ContainerEnd) Plan {
		return Build(Input{Snapshot: swarmOf(c), Ends: map[string]ContainerEnd{c.ID: end}}, p, t0)
	}
	t.Run("23h59m ago: grace with a minute left", func(t *testing.T) {
		t.Parallel()
		got := build(task("c1", "1", 0), ContainerEnd{FinishedAt: t0.Add(-(24*time.Hour - time.Minute)), SizeRw: -1})
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 {
			t.Fatalf("plan = %+v", got)
		}
		it := got.NotRemoved[0]
		if it.Class != ClassGrace || it.Reason.Code != ReasonYoungerThanGrace || it.Reason.Remaining != time.Minute {
			t.Fatalf("item = %+v, want grace with 1m remaining", it)
		}
	})
	t.Run("exactly 24h ago: removable", func(t *testing.T) {
		t.Parallel()
		got := build(task("c1", "1", 0), ContainerEnd{FinishedAt: t0.Add(-24 * time.Hour), SizeRw: -1})
		if len(got.Items) != 1 || got.Items[0].Class != ClassRemovable || got.Items[0].Age != 24*time.Hour {
			t.Fatalf("plan = %+v, want removable at exactly the grace period", got)
		}
	})
	t.Run("never finished falls back to created", func(t *testing.T) {
		t.Parallel()
		young := task("c1", "1", 0)
		young.Created = t0.Add(-time.Hour)
		if got := build(young, ContainerEnd{SizeRw: -1}); len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassGrace {
			t.Fatalf("young plan = %+v, want grace", got)
		}
		old := task("c2", "1", 0) // created 72h ago
		if got := build(old, ContainerEnd{SizeRw: -1}); len(got.Items) != 1 {
			t.Fatalf("old plan = %+v, want removable", got)
		}
	})
	t.Run("a policy grace is honoured", func(t *testing.T) {
		t.Parallel()
		q := p
		q.Grace = 72 * time.Hour
		c := task("c1", "1", 0)
		got := Build(Input{Snapshot: swarmOf(c), Ends: map[string]ContainerEnd{"c1": {FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}}}, q, t0)
		if len(got.Items) != 0 || got.NotRemoved[0].Reason.Remaining != 24*time.Hour {
			t.Fatalf("plan = %+v, want 24h of 72h left", got)
		}
	})
}

func TestClockSkewIsCantTell(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	c := task("c1", "1", 0)
	end := func(ahead time.Duration) Plan {
		return Build(Input{Snapshot: swarmOf(c), Ends: map[string]ContainerEnd{"c1": {FinishedAt: t0.Add(ahead), SizeRw: -1}}}, p, t0)
	}
	if got := end(time.Hour); len(got.NotRemoved) != 1 || got.NotRemoved[0].Reason.Code != ReasonClockSkew || got.NotRemoved[0].Class != ClassCantTell {
		t.Fatalf("an hour ahead: %+v, want cant_tell clock_skew", got)
	}
	// A few seconds ahead is a clock a little out of step, and the container is young.
	if got := end(10 * time.Second); len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassGrace || got.NotRemoved[0].Age != 0 {
		t.Fatalf("10s ahead: %+v, want grace with age 0", got)
	}
}

func TestKeepLabelExact(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	for value, protected := range map[string]bool{"true": true, "True": false, "TRUE": false, "1": false, "yes": false, "": false, " true": false} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Parallel()
			kept, other := task("kept", "1", 0), task("other", "2", 0)
			kept.Labels[keepLabel] = value
			kept.Labels[labelSwarmTaskName] = "web.1.task-kept"
			other.Labels[labelSwarmTaskName] = "web.2.task-other"
			got := planOf(swarmOf(kept, other), p)
			if protected {
				if len(got.NotRemoved) != 1 || got.NotRemoved[0].Ref != "kept" || got.NotRemoved[0].Class != ClassProtected || got.NotRemoved[0].Reason.Code != ReasonKeptLabel {
					t.Fatalf("not removed = %+v, want kept protected kept_label", got.NotRemoved)
				}
				// The label protects only that container.
				if !reflect.DeepEqual(refs(got.Items), []string{"other"}) {
					t.Fatalf("items = %v, want the unlabelled one still listed", refs(got.Items))
				}
				return
			}
			if !reflect.DeepEqual(refs(got.Items), []string{"kept", "other"}) || len(got.NotRemoved) != 0 {
				t.Fatalf("plan = %+v, want %q not to protect", got, value)
			}
		})
	}
}

func TestInspectCap(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	const extra = 5
	var cs []Container
	for i := range MaxInspectsPerRun + extra {
		c := task(fmt.Sprintf("c%04d", i), "1", 0)
		c.Created = t0.Add(-72 * time.Hour).Add(time.Duration(i) * time.Second)
		cs = append(cs, c)
	}
	s := swarmOf(cs...)
	cands := Candidates(s, p)
	if len(cands) != MaxInspectsPerRun {
		t.Fatalf("candidates = %d, want the cap of %d", len(cands), MaxInspectsPerRun)
	}
	if cands[0].ID != "c0000" || cands[len(cands)-1].ID != fmt.Sprintf("c%04d", MaxInspectsPerRun-1) || cands[0].NodeID != "n1-id" {
		t.Fatalf("candidates run from %+v to %+v, want the oldest first on their node", cands[0], cands[len(cands)-1])
	}
	ends := map[string]ContainerEnd{}
	for _, r := range cands {
		ends[r.ID] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1}
	}
	got := Build(Input{Snapshot: s, Ends: ends}, p, t0)
	if len(got.Items) != MaxInspectsPerRun || len(got.NotRemoved) != extra {
		t.Fatalf("items/notRemoved = %d/%d, want %d/%d", len(got.Items), len(got.NotRemoved), MaxInspectsPerRun, extra)
	}
	for _, it := range got.NotRemoved {
		if it.Class != ClassCantTell || it.Reason.Code != ReasonInspectCapped {
			t.Errorf("item = %+v, want cant_tell inspect_capped, never removable", it)
		}
	}
}

func TestAContainerThatCouldNotBeInspectedIsNeverRemoved(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	got := Build(Input{Snapshot: swarmOf(task("c1", "1", 0))}, p, t0)
	if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Reason.Code != ReasonInspectCapped {
		t.Fatalf("plan = %+v, want cant_tell inspect_capped", got)
	}
}

func TestOfflineNodeLeavesTheRestUnchanged(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	var cs []Container
	for i := range 3 {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), fmt.Sprint(i+1), i))
	}
	whole := swarmOf(cs...)
	missing := swarmOf(cs...)
	missing.NotReporting = []string{"n2"}
	a, b := planOf(whole, p), planOf(missing, p)
	if !reflect.DeepEqual(a.Items, b.Items) || !reflect.DeepEqual(a.NotRemoved, b.NotRemoved) {
		t.Fatalf("a missing node changed the listing of the others:\n%+v\n%+v", a.Items, b.Items)
	}
	if !reflect.DeepEqual(b.WithheldNodes, []string{"n2"}) {
		t.Fatalf("withheld = %v, want n2", b.WithheldNodes)
	}
}

func TestSizeAndAgeOfARemovableContainer(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	p.KeepN = 0
	c := task("c1", "1", 0)
	got := Build(Input{Snapshot: swarmOf(c), Ends: map[string]ContainerEnd{"c1": {FinishedAt: t0.Add(-48 * time.Hour), SizeRw: 4096}}}, p, t0)
	if len(got.Items) != 1 {
		t.Fatalf("plan = %+v", got)
	}
	it := got.Items[0]
	if it.Size != 4096 || it.Age != 48*time.Hour || it.Reason.Age != 48*time.Hour || it.Name != "web.1.task-c1" || it.Node != "n1" || it.NodeID != "n1-id" {
		t.Fatalf("item = %+v", it)
	}
	if _, _, _, est, known := got.Summary(); est != 4096 || !known {
		t.Fatalf("estimate = %d known %v, want 4096", est, known)
	}
}
