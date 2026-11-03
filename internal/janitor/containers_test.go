// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/stacks"
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

// The rules for a container that is not the container of a swarm task (plan 11-12, D-05, D-11, D-18,
// RESEARCH F16, JAN-06). Every case judges at t0 with every container finished two days ago unless
// it says otherwise.

// box makes an exited container of node n1 with the labels given. idx orders containers: a larger
// idx is newer.
func box(id string, idx int, labels map[string]string) Container {
	return Container{
		ID: id, Name: "/" + id, NodeID: "n1-id", Node: "n1", State: "exited",
		Created: t0.Add(-72 * time.Hour).Add(time.Duration(idx) * time.Hour), Labels: labels,
	}
}

// inState is c in another state of the engine.
func inState(c Container, state string) Container {
	c.State = state
	return c
}

// composed is the labels docker compose puts on a container of project's service.
func composed(project, service string, oneoff bool) map[string]string {
	l := map[string]string{stacks.LabelComposeProject: project, stacks.LabelComposeService: service, stacks.LabelComposeOneoff: "False"}
	if oneoff {
		l[stacks.LabelComposeOneoff] = "True"
	}
	return l
}

// withStack adds the label that says a TierPlan stack made the container.
func withStack(l map[string]string, id string) map[string]string {
	l[stacks.LabelStackID] = id
	return l
}

// hostOf is a standalone host of one node holding cs, whose TierPlan stacks were read and are the
// stacks named. A stack is its name and, with the same text, its ID: stk-{name}.
func hostOf(stackNames []string, cs ...Container) Snapshot {
	names, ids := map[string]bool{}, map[string]bool{}
	for _, n := range stackNames {
		names[n], ids["stk-"+n] = true, true
	}
	return Snapshot{
		Nodes: []Node{{ID: "n1-id", Name: "n1"}}, Containers: cs, ReadAt: t0, ServicesRead: true,
		StackNames: names, StackIDs: ids, StacksRead: true,
	}
}

// optIn is the default policy with the stopped-containers opt-in as given and no container kept
// by keep-N.
func optIn(on bool) Policy {
	p := DefaultPolicy()
	p.KeepN, p.RemoveStoppedContainers = 0, on
	return p
}

func TestStandaloneExistingProjectNeverTouched(t *testing.T) {
	t.Parallel()
	web, db := box("web", 0, composed("shop", "web", false)), box("db", 1, composed("shop", "db", false))
	for name, cs := range map[string][]Container{
		"a running sibling":         {web, inState(db, "running")},
		"a paused sibling":          {web, inState(db, "paused")},
		"a restarting sibling":      {web, inState(db, "restarting")},
		"a created sibling":         {web, inState(db, "created")},
		"an exited sibling":         {web, db},
		"an exited one-off":         {web, box("run1", 2, composed("shop", "web", true))},
		"a sibling on another node": {web, func() Container { c := inState(db, "running"); c.NodeID, c.Node = "n2-id", "n2"; return c }()},
	} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, opt-in %v", name, on), func(t *testing.T) {
				t.Parallel()
				p := optIn(on)
				p.KeepN = 5 // a one-off sibling stays by keep-N; nothing else may be listed either
				s := hostOf(nil, cs...)
				got := planOf(s, p)
				if len(got.Items) != 0 || len(got.NotRemoved) != 0 {
					t.Fatalf("plan = %+v, want nothing: the project still exists", got)
				}
				if c := Candidates(s, p); len(c) != 0 {
					t.Fatalf("candidates = %v, want none: nothing of it is inspected", c)
				}
			})
		}
	}
}

func TestStandaloneOwnedProjectOneOffs(t *testing.T) {
	t.Parallel()
	svc := func(id string, idx int) Container { return box(id, idx, composed("shop", "web", false)) }
	oneoff := func(id string, idx int) Container { return box(id, idx, composed("shop", "migrate", true)) }
	t.Run("a stack named as the project: its service containers are left, its old one-offs beyond keep-N go", func(t *testing.T) {
		t.Parallel()
		p := DefaultPolicy()
		got := planOf(hostOf([]string{"shop"}, svc("web1", 0), svc("web2", 1), svc("web3", 2), oneoff("r1", 0), oneoff("r2", 1), oneoff("r3", 2)), p)
		if !reflect.DeepEqual(refs(got.Items), []string{"r1"}) || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want only the oldest one-off", got)
		}
		it := got.Items[0]
		if it.Class != ClassRemovable || it.Reason.Code != ReasonComposeOneoff || it.Reason.Project != "shop" || it.Reason.Service != "migrate" || it.Reason.Rank != 1 {
			t.Errorf("item = %+v, want removable compose_oneoff of (shop, migrate), rank 1", it)
		}
	})
	t.Run("a stack whose ID the container carries owns it, whatever the project is called", func(t *testing.T) {
		t.Parallel()
		c := box("web1", 0, withStack(composed("renamed", "web", false), "stk-shop"))
		if got := planOf(hostOf([]string{"shop"}, c), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want nothing: a TierPlan stack owns it", got)
		}
	})
	t.Run("a removed stack's ID on a project another stack is named after: the project is owned, so left", func(t *testing.T) {
		t.Parallel()
		c := box("web1", 0, withStack(composed("shop", "web", false), "stk-removed"))
		if got := planOf(hostOf([]string{"shop"}, c), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want nothing", got)
		}
	})
}

func TestStandaloneLoneComposeReportOnly(t *testing.T) {
	t.Parallel()
	lone := box("tmp1", 0, composed("scratch", "app", false))
	t.Run("opt-in off: reported, never removable", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf(nil, lone), optIn(false))
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 {
			t.Fatalf("plan = %+v, want the container reported and not removable", got)
		}
		it := got.NotRemoved[0]
		if it.Class != ClassReported || it.Reason.Code != ReasonLoneComposeContainer || it.Reason.Project != "scratch" || it.Reason.Service != "app" || it.Age != 48*time.Hour {
			t.Errorf("item = %+v, want reported lone_compose_container of scratch/app, 48h old", it)
		}
	})
	t.Run("opt-in on: removable as compose_project_gone", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf(nil, lone), optIn(true))
		if len(got.Items) != 1 || len(got.NotRemoved) != 0 || got.Items[0].Class != ClassRemovable || got.Items[0].Reason.Code != ReasonComposeProjectGone {
			t.Fatalf("plan = %+v, want removable compose_project_gone", got)
		}
	})
	t.Run("kept by label whatever the opt-in", func(t *testing.T) {
		t.Parallel()
		kept := box("tmp1", 0, composed("scratch", "app", false))
		kept.Labels[keepLabel] = "true"
		got := planOf(hostOf(nil, kept), optIn(true))
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassProtected || got.NotRemoved[0].Reason.Code != ReasonKeptLabel {
			t.Fatalf("plan = %+v, want protected kept_label", got)
		}
	})
	t.Run("not inspected: cant_tell, never removable", func(t *testing.T) {
		t.Parallel()
		got := Build(Input{Snapshot: hostOf(nil, lone)}, optIn(true), t0)
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 || got.NotRemoved[0].Class != ClassCantTell || got.NotRemoved[0].Reason.Code != ReasonInspectCapped {
			t.Fatalf("plan = %+v, want cant_tell inspect_capped", got)
		}
	})
}

func TestStandalonePlainNeedsOptIn(t *testing.T) {
	t.Parallel()
	plain := box("p1", 0, map[string]string{})
	t.Run("opt-in off: reported opt_in_off_containers", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf(nil, plain), optIn(false))
		if len(got.Items) != 0 || len(got.NotRemoved) != 1 {
			t.Fatalf("plan = %+v, want reported and not removable", got)
		}
		if it := got.NotRemoved[0]; it.Class != ClassReported || it.Reason.Code != ReasonOptInOffContainers || it.Name != "p1" || it.Node != "n1" {
			t.Errorf("item = %+v, want reported opt_in_off_containers", it)
		}
	})
	t.Run("opt-in on: removable stopped_standalone", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf(nil, plain), optIn(true))
		if len(got.Items) != 1 || len(got.NotRemoved) != 0 || got.Items[0].Class != ClassRemovable || got.Items[0].Reason.Code != ReasonStoppedStandalone {
			t.Fatalf("plan = %+v, want removable stopped_standalone", got)
		}
	})
	t.Run("a nil label map is a plain container too", func(t *testing.T) {
		t.Parallel()
		c := plain
		c.Labels = nil
		if got := planOf(hostOf(nil, c), optIn(false)); len(got.NotRemoved) != 1 || got.NotRemoved[0].Reason.Code != ReasonOptInOffContainers {
			t.Fatalf("plan = %+v", got)
		}
	})
	t.Run("a container that is not exited is not the janitor's business", func(t *testing.T) {
		t.Parallel()
		for _, state := range []string{"running", "paused", "restarting", "created", "removing", "dead"} {
			if got := planOf(hostOf(nil, inState(plain, state)), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
				t.Errorf("a %s container: plan = %+v, want nothing", state, got)
			}
		}
	})
}

func TestStandaloneGrace(t *testing.T) {
	t.Parallel()
	gone := func() Container { return box("g1", 0, withStack(composed("oldshop", "web", false), "stk-oldshop")) }
	cases := []struct {
		name   string
		cs     []Container
		stacks []string
		p      Policy
		after  Class // the class once the grace period has passed
		code   ReasonCode
	}{
		{"stack_gone", []Container{gone()}, nil, optIn(false), ClassRemovable, ReasonStackGone},
		{"compose_oneoff", []Container{box("r1", 0, composed("shop", "migrate", true))}, []string{"shop"}, optIn(false), ClassRemovable, ReasonComposeOneoff},
		{"lone compose container, opt-in off", []Container{box("l1", 0, composed("tmp", "app", false))}, nil, optIn(false), ClassReported, ReasonLoneComposeContainer},
		{"lone compose container, opt-in on", []Container{box("l1", 0, composed("tmp", "app", false))}, nil, optIn(true), ClassRemovable, ReasonComposeProjectGone},
		{"plain container, opt-in off", []Container{box("p1", 0, nil)}, nil, optIn(false), ClassReported, ReasonOptInOffContainers},
		{"plain container, opt-in on", []Container{box("p1", 0, nil)}, nil, optIn(true), ClassRemovable, ReasonStoppedStandalone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := func(ago time.Duration) Plan {
				ends := map[string]ContainerEnd{}
				for _, c := range tc.cs {
					ends[c.ID] = ContainerEnd{FinishedAt: t0.Add(-ago), SizeRw: -1}
				}
				return Build(Input{Snapshot: hostOf(tc.stacks, tc.cs...), Ends: ends}, tc.p, t0)
			}
			young := at(24*time.Hour - time.Minute)
			if len(young.Items) != 0 || len(young.NotRemoved) != 1 {
				t.Fatalf("23h59m: plan = %+v, want one container under grace", young)
			}
			if it := young.NotRemoved[0]; it.Class != ClassGrace || it.Reason.Code != ReasonYoungerThanGrace || it.Reason.Remaining != time.Minute {
				t.Errorf("23h59m: item = %+v, want grace with 1m remaining", it)
			}
			old := at(24 * time.Hour)
			var it Item
			switch {
			case tc.after == ClassRemovable && len(old.Items) == 1 && len(old.NotRemoved) == 0:
				it = old.Items[0]
			case tc.after == ClassReported && len(old.Items) == 0 && len(old.NotRemoved) == 1:
				it = old.NotRemoved[0]
			default:
				t.Fatalf("24h: plan = %+v, want one %s item", old, tc.after)
			}
			if it.Class != tc.after || it.Reason.Code != tc.code || it.Age != 24*time.Hour {
				t.Errorf("24h: item = %+v, want %s %s at exactly the grace period", it, tc.after, tc.code)
			}
		})
	}
	t.Run("a container beside swarm tasks on a swarm node is judged the same way", func(t *testing.T) {
		t.Parallel()
		s := swarmOf(task("t1", "1", 0), box("p1", 0, nil))
		s.StacksRead, s.StackNames, s.StackIDs = true, map[string]bool{}, map[string]bool{}
		p := optIn(true)
		got := planOf(s, p)
		if !reflect.DeepEqual(refs(got.Items), []string{"p1", "t1"}) {
			t.Fatalf("items = %v, want the plain container and the task container", refs(got.Items))
		}
	})
}

func TestStandaloneStackGoneRunningSibling(t *testing.T) {
	t.Parallel()
	leftover := func(id, service string) Container {
		return box(id, 0, withStack(composed("oldshop", service, false), "stk-oldshop"))
	}
	t.Run("every one exited: all of them are leftovers", func(t *testing.T) {
		t.Parallel()
		got := planOf(hostOf(nil, leftover("a", "web"), leftover("b", "db")), optIn(false))
		if !reflect.DeepEqual(refs(got.Items), []string{"a", "b"}) || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want both removable", got)
		}
		for _, it := range got.Items {
			if it.Reason.Code != ReasonStackGone || it.Reason.Project != "oldshop" {
				t.Errorf("item = %+v, want stack_gone of oldshop", it)
			}
		}
	})
	for _, state := range []string{"running", "paused", "restarting", "created", "removing", "dead"} {
		t.Run("a "+state+" container in the project: none is listed", func(t *testing.T) {
			t.Parallel()
			sibling := inState(leftover("c", "db"), state)
			s := hostOf(nil, leftover("a", "web"), sibling)
			if got := planOf(s, optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
				t.Fatalf("plan = %+v, want nothing: the project still has a %s container", got, state)
			}
		})
	}
	t.Run("a running container of the same stack under another project name: none is listed", func(t *testing.T) {
		t.Parallel()
		other := inState(box("c", 0, withStack(composed("elsewhere", "db", false), "stk-oldshop")), "running")
		if got := planOf(hostOf(nil, leftover("a", "web"), other), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want nothing", got)
		}
	})
	t.Run("a running container of another project does not hold it", func(t *testing.T) {
		t.Parallel()
		other := inState(box("c", 0, composed("shop", "db", false)), "running")
		got := planOf(hostOf(nil, leftover("a", "web"), other), optIn(false))
		if !reflect.DeepEqual(refs(got.Items), []string{"a"}) {
			t.Fatalf("plan = %+v, want the leftover", got)
		}
	})
}

func TestStandaloneStacksUnread(t *testing.T) {
	t.Parallel()
	cs := []Container{
		box("gone", 0, withStack(composed("oldshop", "web", false), "stk-oldshop")),
		box("lone", 0, composed("tmp", "app", false)),
		box("oneoff", 0, composed("shop", "migrate", true)),
		box("kept", 0, composed("shop", "web", false)),
	}
	cs[3].Labels[keepLabel] = "true"
	plain := box("plain", 0, nil)
	s := hostOf(nil, append(cs, plain)...)
	s.StacksRead, s.StackNames, s.StackIDs = false, nil, nil
	p := optIn(true)
	if c := Candidates(s, p); !reflect.DeepEqual(c, []ContainerRef{{ID: "plain", NodeID: "n1-id"}}) {
		t.Fatalf("candidates = %v, want only the plain container: who owns the compose ones is not known", c)
	}
	got := planOf(s, p)
	for _, it := range append(append([]Item{}, got.Items...), got.NotRemoved...) {
		switch it.Ref {
		case "gone", "lone", "oneoff":
			if it.Class != ClassCantTell || it.Reason.Code != ReasonStackUnknown {
				t.Errorf("%s = %+v, want cant_tell stack_unknown", it.Ref, it)
			}
		case "kept":
			if it.Class != ClassProtected {
				t.Errorf("kept = %+v, want protected: the label is checked first", it)
			}
		case "plain":
			if it.Class != ClassRemovable || it.Reason.Code != ReasonStoppedStandalone {
				t.Errorf("plain = %+v, want a plain container judged as before", it)
			}
		default:
			t.Errorf("unexpected item %+v", it)
		}
	}
	if !reflect.DeepEqual(refs(got.Items), []string{"plain"}) || len(got.NotRemoved) != 4 {
		t.Fatalf("plan = %+v, want only the plain container removable and four others left", got)
	}
}

func TestStandaloneScaledServiceUntouched(t *testing.T) {
	t.Parallel()
	replicas := func(n int, labelOf func(i int) map[string]string) []Container {
		var cs []Container
		for i := range n {
			cs = append(cs, box(fmt.Sprintf("web%d", i+1), i, labelOf(i)))
		}
		return cs
	}
	t.Run("three exited replicas of a project nobody manages", func(t *testing.T) {
		t.Parallel()
		for _, on := range []bool{false, true} {
			cs := replicas(3, func(int) map[string]string { return composed("shop", "web", false) })
			if got := planOf(hostOf(nil, cs...), optIn(on)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
				t.Fatalf("opt-in %v: plan = %+v, want none listed", on, got)
			}
		}
	})
	t.Run("three exited replicas of a project a stack owns", func(t *testing.T) {
		t.Parallel()
		cs := replicas(3, func(int) map[string]string { return withStack(composed("shop", "web", false), "stk-shop") })
		if got := planOf(hostOf([]string{"shop"}, cs...), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want none listed", got)
		}
	})
	t.Run("one of three running", func(t *testing.T) {
		t.Parallel()
		cs := replicas(3, func(int) map[string]string { return composed("shop", "web", false) })
		cs[2] = inState(cs[2], "running")
		if got := planOf(hostOf(nil, cs...), optIn(true)); len(got.Items) != 0 || len(got.NotRemoved) != 0 {
			t.Fatalf("plan = %+v, want none listed", got)
		}
	})
}
