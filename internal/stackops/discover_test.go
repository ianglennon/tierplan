// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Discovery (plan 08-08, STK-12, D-07): stacks Bayplan did not make, found by their labels in what the lists
// already hold.

func foreignService(stack, name string, running, desired uint64, labels map[string]string) dockerx.ServiceRow {
	return dockerx.ServiceRow{ID: "id-" + name, Name: name, Mode: "replicated", Running: running, Desired: desired, Image: "img:1", Stack: stack, Labels: labels}
}

func foreignContainer(id, project, service, state string, labels map[string]string) dockerx.ContainerRow {
	l := map[string]string{stacks.LabelComposeProject: project, stacks.LabelComposeService: service}
	for k, v := range labels {
		l[k] = v
	}
	return dockerx.ContainerRow{ID: id, Name: "/" + project + "-" + service + "-1", State: state, Image: "img:1", Labels: l}
}

func TestDiscoverSwarmGroupsByNamespace(t *testing.T) {
	t.Parallel()
	h := stackops.Held{Services: []dockerx.ServiceRow{
		foreignService("legacy", "legacy_web", 2, 2, nil),
		foreignService("legacy", "legacy_db", 0, 1, nil),
		foreignService("blog", "blog_app", 0, 1, nil),
		foreignService("", "loose", 1, 1, nil),
		foreignService("up", "up_app", 1, 1, nil),
	}}

	got := stackops.Discover(h, nil, stacks.TargetSwarm, stackops.SelfMarks{})

	if len(got) != 3 {
		t.Fatalf("Discover = %+v, want three groups: blog, legacy, up (the unlabelled service is no stack)", got)
	}
	byName := map[string]stackops.Discovered{}
	for _, d := range got {
		byName[d.Name] = d
	}
	legacy := byName["legacy"]
	if legacy.Target != stacks.TargetSwarm || legacy.Total != 2 || legacy.Running != 1 || legacy.State != stackops.StateDegraded {
		t.Errorf("legacy = %+v, want two services, one running: degraded", legacy)
	}
	if len(legacy.Rows) != 2 || legacy.Rows[0].Short != "db" || legacy.Rows[1].Short != "web" {
		t.Errorf("legacy rows = %+v, want db then web", legacy.Rows)
	}
	if blog := byName["blog"]; blog.State != stackops.StateNotRunning || blog.Running != 0 {
		t.Errorf("blog = %+v, want not running", blog)
	}
	if up := byName["up"]; up.State != stackops.StateRunning {
		t.Errorf("up = %+v, want running", up)
	}
}

func TestDiscoverStandaloneGroupsByProject(t *testing.T) {
	t.Parallel()
	h := stackops.Held{Containers: []dockerx.ContainerRow{
		foreignContainer("c1", "shop", "web", "running", nil),
		foreignContainer("c2", "shop", "db", "exited", nil),
		foreignContainer("c3", "shop", "web", "running", map[string]string{stacks.LabelComposeOneoff: "True"}),
		foreignContainer("c4", "jobs", "once", "running", map[string]string{stacks.LabelComposeOneoff: "True"}),
		{ID: "c5", Name: "/loose", State: "running", Labels: map[string]string{}},
	}}

	got := stackops.Discover(h, nil, stacks.TargetStandalone, stackops.SelfMarks{})

	if len(got) != 1 || got[0].Name != "shop" {
		t.Fatalf("Discover = %+v, want shop alone: a project of one-off containers and a loose container are no stacks", got)
	}
	shop := got[0]
	if shop.Target != stacks.TargetStandalone || shop.Total != 2 || shop.Running != 1 || shop.State != stackops.StateDegraded {
		t.Errorf("shop = %+v, want two services, one running: degraded", shop)
	}
	for _, r := range shop.Rows {
		if r.Name == "web" && len(r.Containers) != 1 {
			t.Errorf("web has %d containers, want 1: the one-off container is left out", len(r.Containers))
		}
	}
}

func TestDiscoverSkipsManagedNames(t *testing.T) {
	t.Parallel()
	managed := []stackops.Stack{{ID: "stack-web", Name: "web", Target: stacks.TargetSwarm}}
	h := stackops.Held{Services: []dockerx.ServiceRow{
		// A stack Bayplan is deploying for the first time: its namespace is the managed stack's name and no
		// label carries its ID yet.
		foreignService("web", "web_app", 0, 1, nil),
		foreignService("old", "old_app", 1, 1, nil),
	}}

	got := stackops.Discover(h, managed, stacks.TargetSwarm, stackops.SelfMarks{})

	if len(got) != 1 || got[0].Name != "old" {
		t.Fatalf("Discover = %+v, want old alone: a group named like a managed stack is that stack", got)
	}
}

// TestDiscoverFoldsCaseOnASwarm: swarm looks object names up regardless of case, so an outside Foo beside a managed
// foo is not a second stack to adopt (T-08-95, WR-07). It is still listed, read-only with the reason, and not dropped;
// a name that differs by more than case is untouched, and a standalone host compares exactly.
func TestDiscoverFoldsCaseOnASwarm(t *testing.T) {
	t.Parallel()
	managed := []stackops.Stack{{ID: "stack-foo", Name: "foo", Target: stacks.TargetSwarm}}
	h := stackops.Held{Services: []dockerx.ServiceRow{
		foreignService("Foo", "Foo_app", 1, 1, nil),
		foreignService("foo", "foo_app", 0, 1, nil),
		foreignService("other", "other_app", 1, 1, nil),
	}}

	got := stackops.Discover(h, managed, stacks.TargetSwarm, stackops.SelfMarks{})

	by := map[string]stackops.Discovered{}
	for _, d := range got {
		by[d.Name] = d
	}
	if _, ok := by["foo"]; ok {
		t.Errorf("Discover lists foo, which is the managed stack: %+v", got)
	}
	clash, ok := by["Foo"]
	if !ok {
		t.Fatalf("Discover = %+v, want Foo listed read-only, not dropped", got)
	}
	if clash.Adoptable() || !strings.Contains(clash.NameReason, "foo") || !strings.Contains(clash.NameReason, "case") {
		t.Errorf("Foo = adoptable %v reason %q, want it not adoptable, naming the managed stack and the case rule", clash.Adoptable(), clash.NameReason)
	}
	if clash.Total != 1 || len(clash.Rows) != 1 {
		t.Errorf("Foo = %+v, want its one service shown", clash)
	}
	if other := by["other"]; !other.Adoptable() || other.NameReason != "" {
		t.Errorf("other = %+v, want it adoptable", other)
	}

	// A standalone host compares exactly: a project that differs in case is not the managed one and gets no clash reason.
	standalone := stackops.Held{Containers: []dockerx.ContainerRow{foreignContainer("c1", "Foo", "web", "running", nil)}}
	managedHere := []stackops.Stack{{ID: "stack-foo", Name: "foo", Target: stacks.TargetStandalone}}
	for _, d := range stackops.Discover(standalone, managedHere, stacks.TargetStandalone, stackops.SelfMarks{}) {
		if strings.Contains(d.NameReason, "already managed") {
			t.Errorf("%s on a standalone host = reason %q, want no case clash there", d.Name, d.NameReason)
		}
	}
}

func TestDiscoverFlagsOrphanPortainerSelfAgent(t *testing.T) {
	t.Parallel()
	t.Run("an orphan Bayplan ID is the smallest one and never a link", func(t *testing.T) {
		t.Parallel()
		h := stackops.Held{Services: []dockerx.ServiceRow{
			foreignService("lost", "lost_a", 1, 1, map[string]string{stacks.LabelStackID: "stack-zzz"}),
			foreignService("lost", "lost_b", 1, 1, map[string]string{stacks.LabelStackID: "stack-aaa"}),
			foreignService("plain", "plain_a", 1, 1, nil),
		}}
		got := stackops.Discover(h, nil, stacks.TargetSwarm, stackops.SelfMarks{})
		if len(got) != 2 || got[0].Name != "lost" || got[0].OrphanID != "stack-aaa" || got[1].OrphanID != "" {
			t.Errorf("Discover = %+v, want lost flagged with stack-aaa and plain not", got)
		}
	})
	t.Run("Portainer by label key or by working directory", func(t *testing.T) {
		t.Parallel()
		h := stackops.Held{Containers: []dockerx.ContainerRow{
			foreignContainer("c1", "viaLabel", "web", "running", map[string]string{"io.portainer.accesscontrol.teams": "x"}),
			foreignContainer("c2", "viaDir", "web", "running", map[string]string{stacks.LabelComposeWorkingDir: "/data/compose/12"}),
			foreignContainer("c3", "other", "web", "running", map[string]string{stacks.LabelComposeWorkingDir: "/srv/other"}),
		}}
		got := stackops.Discover(h, nil, stacks.TargetStandalone, stackops.SelfMarks{})
		flags := map[string]bool{}
		for _, d := range got {
			flags[d.Name] = d.Portainer
		}
		if !reflect.DeepEqual(flags, map[string]bool{"viaLabel": true, "viaDir": true, "other": false}) {
			t.Errorf("portainer flags = %v", flags)
		}
		if !stackops.PortainerMarked(map[string]string{"io.portainer.x": ""}) || stackops.PortainerMarked(nil) {
			t.Error("PortainerMarked disagrees with the markers")
		}
	})
	t.Run("Bayplan's own project by its container, the agent's by its label", func(t *testing.T) {
		t.Parallel()
		self := "0123456789abcdef0123456789abcdef"
		h := stackops.Held{Containers: []dockerx.ContainerRow{
			foreignContainer(self, "bayplan", "server", "running", nil),
			foreignContainer("agentcontainer0001", "edge", "agent", "running", map[string]string{"bayplan.agent": "true"}),
			foreignContainer("someoneelse00001", "blog", "web", "running", nil),
		}}
		got := stackops.Discover(h, nil, stacks.TargetStandalone, stackops.SelfMarks{ContainerID: self[:12]})
		owners := map[string]string{}
		for _, d := range got {
			owners[d.Name] = d.Owner
		}
		if !reflect.DeepEqual(owners, map[string]string{"bayplan": stackops.OwnerBayplan, "edge": stackops.OwnerAgent, "blog": ""}) {
			t.Errorf("owners = %v", owners)
		}
	})
	t.Run("a swarm's own namespace and its agent service", func(t *testing.T) {
		t.Parallel()
		agent := foreignService("agents", "agents_bayplan-agent", 3, 3, nil)
		agent.Agent = true
		h := stackops.Held{Services: []dockerx.ServiceRow{
			foreignService("tools", "tools_bayplan", 1, 1, nil),
			agent,
			foreignService("blog", "blog_app", 1, 1, nil),
		}}
		got := stackops.Discover(h, nil, stacks.TargetSwarm, stackops.SelfMarks{Namespace: "tools"})
		owners := map[string]string{}
		for _, d := range got {
			owners[d.Name] = d.Owner
		}
		if !reflect.DeepEqual(owners, map[string]string{"tools": stackops.OwnerBayplan, "agents": stackops.OwnerAgent, "blog": ""}) {
			t.Errorf("owners = %v", owners)
		}
	})
	t.Run("a short ID is not taken to be Bayplan's own", func(t *testing.T) {
		t.Parallel()
		h := stackops.Held{Containers: []dockerx.ContainerRow{foreignContainer("abc123456789", "p", "s", "running", nil)}}
		got := stackops.Discover(h, nil, stacks.TargetStandalone, stackops.SelfMarks{ContainerID: "abc"})
		if len(got) != 1 || got[0].Owner != "" {
			t.Errorf("Discover = %+v, want no owner for a three-character self ID", got)
		}
	})
}

func TestDiscoverIsDeterministic(t *testing.T) {
	t.Parallel()
	rows := []dockerx.ServiceRow{
		foreignService("zeta", "zeta_a", 1, 1, nil),
		foreignService("alpha", "alpha_b", 1, 2, nil),
		foreignService("alpha", "alpha_a", 1, 1, nil),
		foreignService("mid", "mid_a", 0, 0, nil),
	}
	forward := stackops.Discover(stackops.Held{Services: rows}, nil, stacks.TargetSwarm, stackops.SelfMarks{})
	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	backward := stackops.Discover(stackops.Held{Services: reversed}, nil, stacks.TargetSwarm, stackops.SelfMarks{})
	again := stackops.Discover(stackops.Held{Services: rows}, nil, stacks.TargetSwarm, stackops.SelfMarks{})

	if !reflect.DeepEqual(forward, again) || !reflect.DeepEqual(forward, backward) {
		t.Fatalf("Discover differs between reads of the same data:\n%+v\n%+v\n%+v", forward, again, backward)
	}
	var names []string
	for _, d := range forward {
		names = append(names, d.Name)
	}
	if !slices.Equal(names, []string{"alpha", "mid", "zeta"}) {
		t.Errorf("names = %v, want them by name", names)
	}
}

func TestManagedRowsUnchanged(t *testing.T) {
	t.Parallel()
	swarm := stackops.SwarmRows(stackops.Stack{ID: "stack-web", Name: "web", Target: stacks.TargetSwarm}, []dockerx.ServiceRow{
		foreignService("web", "web_app", 1, 2, map[string]string{stacks.LabelStackID: "stack-web"}),
		foreignService("web", "web_foreign", 1, 1, nil),
		foreignService("blog", "blog_app", 1, 1, map[string]string{stacks.LabelStackID: "stack-web"}),
	})
	if len(swarm) != 1 || swarm[0].Name != "web_app" || swarm[0].Short != "app" || swarm[0].State != stackops.ServiceDegraded {
		t.Errorf("SwarmRows = %+v, want the one service the stack owns", swarm)
	}

	owned := map[string]string{stacks.LabelStackID: "stack-shop"}
	host := stackops.StandaloneRows(stackops.Stack{ID: "stack-shop", Name: "shop", Target: stacks.TargetStandalone}, []dockerx.ContainerRow{
		foreignContainer("c1", "shop", "web", "running", owned),
		foreignContainer("c2", "shop", "web", "running", map[string]string{stacks.LabelStackID: "stack-shop", stacks.LabelComposeOneoff: "true"}),
		foreignContainer("c3", "shop", "db", "exited", nil),
		foreignContainer("c4", "other", "web", "running", owned),
	})
	if len(host) != 1 || host[0].Name != "web" || len(host[0].Containers) != 1 || host[0].State != stackops.ServiceRunning {
		t.Errorf("StandaloneRows = %+v, want web alone with its one container", host)
	}
}

func TestDiscoverUnusualNames(t *testing.T) {
	t.Parallel()
	t.Run("a swarm lists a mixed-case name as adoptable and the others with the swarm reason", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("a", 62)
		h := stackops.Held{Services: []dockerx.ServiceRow{
			foreignService("MyApp", "MyApp_web", 1, 1, nil),
			foreignService("My App", "My App_web", 1, 1, nil),
			foreignService("Shop.Front", "Shop.Front_web", 1, 1, nil),
			foreignService(long, long+"_web", 1, 1, nil),
		}}
		got := stackops.Discover(h, nil, stacks.TargetSwarm, stackops.SelfMarks{})
		by := map[string]stackops.Discovered{}
		for _, d := range got {
			by[d.Name] = d
		}
		if len(got) != 4 {
			t.Fatalf("Discover = %d groups, want 4: a name the rule refuses is still listed", len(got))
		}
		if d := by["MyApp"]; !d.Adoptable() || d.NameReason != "" {
			t.Errorf("MyApp = %+v, want adoptable", d)
		}
		for _, name := range []string{"My App", "Shop.Front", long} {
			want := "Bayplan can't adopt " + name + ": a stack name on a swarm takes only " + stacks.NameRuleText(stacks.TargetSwarm) + "."
			if d := by[name]; d.Adoptable() || d.NameReason != want {
				t.Errorf("%q = adoptable %v reason %q, want not adoptable with %q", name, d.Adoptable(), d.NameReason, want)
			}
		}
	})
	t.Run("a standalone host refuses a dotted project", func(t *testing.T) {
		t.Parallel()
		h := stackops.Held{Containers: []dockerx.ContainerRow{
			foreignContainer("c1", "shop.front", "web", "running", nil),
			foreignContainer("c2", "shop-front", "web", "running", nil),
		}}
		got := stackops.Discover(h, nil, stacks.TargetStandalone, stackops.SelfMarks{})
		by := map[string]stackops.Discovered{}
		for _, d := range got {
			by[d.Name] = d
		}
		want := "Bayplan can't adopt shop.front: a stack name on a standalone host takes only " + stacks.NameRuleText(stacks.TargetStandalone) + "."
		if d := by["shop.front"]; d.Adoptable() || d.NameReason != want {
			t.Errorf("shop.front = adoptable %v reason %q, want not adoptable with %q", d.Adoptable(), d.NameReason, want)
		}
		if d := by["shop-front"]; !d.Adoptable() {
			t.Errorf("shop-front = %+v, want adoptable", d)
		}
	})
	t.Run("an owner's reason comes first", func(t *testing.T) {
		t.Parallel()
		h := stackops.Held{Services: []dockerx.ServiceRow{foreignService("Bay.Plan", "Bay.Plan_srv", 1, 1, nil)}}
		got := stackops.Discover(h, nil, stacks.TargetSwarm, stackops.SelfMarks{Namespace: "Bay.Plan"})
		if len(got) != 1 || got[0].Owner != stackops.OwnerBayplan || got[0].Adoptable() {
			t.Fatalf("Discover = %+v, want Bayplan's own stack, not adoptable", got)
		}
	})
}
