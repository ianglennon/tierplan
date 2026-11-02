// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"slices"
	"testing"

	mswarm "github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Who uses a network (plan 10-04, NET-04, RESEARCH F6, Pitfall 7). The agents are real sessions
// of the real hub answering from fake daemons, as in aggregate_test.go.

func swarmNet(id, name string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{ID: id, Name: name, Driver: "overlay", Scope: "swarm", Attachable: true}
}

func localNet(id, name string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{ID: id, Name: name, Driver: "bridge", Scope: "local"}
}

func onNet(id, name, net, netID string) dockertest.FakeContainer {
	c := fakeContainer(id, name)
	c.Networks = map[string]dockertest.FakeEndpoint{net: {NetworkID: netID}}
	return c
}

func taskOn(id, service, node string, desired, state mswarm.TaskState, networks ...string) mswarm.Task {
	t := mswarm.Task{ID: id, ServiceID: service, NodeID: node, DesiredState: desired}
	t.Status.State = state
	for _, n := range networks {
		t.NetworksAttachments = append(t.NetworksAttachments, mswarm.NetworkAttachment{Network: mswarm.Network{ID: n}})
	}
	return t
}

// netRow finds the row of the network with this ID on that node (any node for a swarm-scoped one).
func netRow(t *testing.T, agg Aggregate, id, node string) NetworkRow {
	t.Helper()
	for _, n := range agg.Networks {
		if n.ID == id && (n.Scope == dockerx.ScopeSwarm || n.Node == node) {
			return n
		}
	}
	t.Fatalf("no network %s on %s in %+v", id, node, agg.Networks)
	return NetworkRow{}
}

func hasUser(u Usage, row NetworkRow, want string) bool { return slices.Contains(u.UsersOf(row), want) }

func TestResolveUsageSources(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		if i == 4 {
			// The worker: a standalone container on an attachable overlay, and one on a local
			// network that a manager has too, under the same ID.
			c.Containers = []dockertest.FakeContainer{
				onNet("wx", "x", "f", "net-f"),
				onNet("wy", "y", "same", "local-same"),
			}
			c.Networks = []dockertest.FakeNetwork{localNet("local-same", "same")}
			return
		}
		c.Networks = []dockertest.FakeNetwork{
			swarmNet("net-a", "a"), swarmNet("net-b", "b"), swarmNet("net-c", "c"), swarmNet("net-d", "d"),
			swarmNet("net-e", "e"), swarmNet("net-f", "f"), swarmNet("net-g", "g"),
			localNet("local-same", "same"),
		}
		c.ServiceRows = []dockertest.FakeService{
			// Scaled to zero: no task, yet the spec names A.
			{ID: "svc-s", Name: "s", Mode: "replicated", Running: 0, Desired: 0, Networks: []string{"a"}},
			// Updated away from B: only the previous spec names it.
			{ID: "svc-p", Name: "p", Mode: "replicated", Running: 1, Desired: 1,
				PreviousSpec: &mswarm.ServiceSpec{TaskTemplate: mswarm.TaskSpec{Networks: []mswarm.NetworkAttachmentConfig{{Target: "b"}}}}},
			// The endpoint still holds a virtual IP on C that no spec names.
			{ID: "svc-v", Name: "v", Mode: "replicated", Running: 1, Desired: 1},
		}
		c.ServiceVIPNetworks = map[string][]string{"v": {"net-c"}}
		c.Tasks = []mswarm.Task{
			// An update dropped D, but this task is still attached to it, on the worker.
			taskOn("t-alive", "svc-s", "node-wrk-1", mswarm.TaskStateShutdown, mswarm.TaskStateRunning, "net-d"),
			// The swarm has finished with this one.
			taskOn("t-gone", "svc-s", "node-wrk-1", mswarm.TaskStateShutdown, mswarm.TaskStateShutdown, "net-e"),
		}
	})
	u, agg, err := ResolveUsage(testsupport.Context(t), f.agg, f.c, f.env.ID)
	if err != nil {
		t.Fatalf("ResolveUsage: %v", err)
	}
	if !u.Swarm || !u.ManagerRead || len(u.Missing) != 0 {
		t.Fatalf("usage = swarm %v manager %v missing %v, want a complete answer", u.Swarm, u.ManagerRead, u.Missing)
	}
	for _, c := range []struct {
		id, user string
	}{
		{"net-a", "service s"},
		{"net-b", "service p (previous spec)"},
		{"net-c", "service v (virtual IP)"},
		{"net-d", "task of service s on wrk-1"},
		{"net-f", "container x"},
	} {
		row := netRow(t, agg, c.id, "")
		if !u.Known(row) || !u.InUse(row) || !hasUser(u, row, c.user) {
			t.Errorf("%s: known %v in use %v users %v, want %q", c.id, u.Known(row), u.InUse(row), u.UsersOf(row), c.user)
		}
	}
	if row := netRow(t, agg, "net-e", ""); !u.Known(row) || u.InUse(row) {
		t.Errorf("net-e: users %v, want none: its only task is finished", u.UsersOf(row))
	}
	if row := netRow(t, agg, "net-g", ""); !u.Known(row) || u.InUse(row) || len(u.UsersOf(row)) != 0 {
		t.Errorf("net-g: users %v, want none", u.UsersOf(row))
	}
	// A local network belongs to its node: the worker's container uses the worker's, not the managers'.
	if row := netRow(t, agg, "local-same", "wrk-1"); !u.InUse(row) || !hasUser(u, row, "container y") {
		t.Errorf("local-same on wrk-1: users %v, want container y", u.UsersOf(row))
	}
	if row := netRow(t, agg, "local-same", "mgr-1"); u.InUse(row) {
		t.Errorf("local-same on mgr-1: users %v, want none: another node's network is a different one", u.UsersOf(row))
	}
}

func TestResolveUsageNotReporting(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		if i != 4 {
			c.Networks = []dockertest.FakeNetwork{swarmNet("net-f", "f"), localNet("br-local", "mine")}
		}
	})
	f.kick(t, f.w)
	if err := f.c.Refresh(testsupport.Context(t), f.env.ID); err != nil {
		t.Fatal(err)
	}
	u, agg, err := ResolveUsage(testsupport.Context(t), f.agg, f.c, f.env.ID)
	if err != nil {
		t.Fatalf("ResolveUsage: %v", err)
	}
	if !slices.Equal(u.Missing, []string{"wrk-1"}) {
		t.Fatalf("missing = %v, want the node that did not answer", u.Missing)
	}
	if row := netRow(t, agg, "net-f", ""); u.Known(row) {
		t.Errorf("the swarm-scoped network is Known although wrk-1 could hold an attachment")
	}
	if row := netRow(t, agg, "br-local", "mgr-1"); !u.Known(row) {
		t.Errorf("a local network on a node that answered is not Known")
	}
	// The worker's own local network is out of reach: nothing is Known about a node that did not report.
	if u.Known(NetworkRow{NetworkRow: dockerx.NetworkRow{ID: "br-w", Scope: "local"}, NodeID: f.w.id}) {
		t.Errorf("a local network on wrk-1 is Known although wrk-1 did not answer")
	}
}

func TestResolveUsageManagerReadFails(t *testing.T) {
	f := newFanRig(t, nil, func(i int, c *dockertest.FakeConfig) {
		if i != 4 {
			c.Networks = []dockertest.FakeNetwork{swarmNet("net-f", "f"), localNet("br-local", "mine")}
		}
	})
	// Every manager loses its quorum after the nodes were listed: the services cannot be read.
	for _, m := range f.m {
		m.fake.LoseQuorum()
	}
	u, agg, err := ResolveUsage(testsupport.Context(t), f.agg, f.c, f.env.ID)
	if err != nil {
		t.Fatalf("ResolveUsage: %v (a manager that cannot answer is not an error)", err)
	}
	if !u.Swarm || u.ManagerRead {
		t.Fatalf("swarm %v manager read %v, want a swarm whose manager read failed", u.Swarm, u.ManagerRead)
	}
	if row := netRow(t, agg, "net-f", ""); u.Known(row) {
		t.Errorf("the swarm-scoped network is Known without the services")
	}
	if row := netRow(t, agg, "br-local", "mgr-1"); !u.Known(row) {
		t.Errorf("a local network needs no manager read to be Known")
	}
}
