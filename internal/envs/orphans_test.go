// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// orphanEpoch is the instant the rigs' clocks start at; a network made before it by more than a day is
// past its grace period once the rig has advanced a couple of minutes.
var orphanEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func oldNet(n dockertest.FakeNetwork) dockertest.FakeNetwork {
	n.Created = orphanEpoch.Add(-48 * time.Hour)
	return n
}

func freshNet(n dockertest.FakeNetwork) dockertest.FakeNetwork {
	n.Created = orphanEpoch.Add(-time.Hour)
	return n
}

// fakeFacts is a StackFacts the tests set.
type fakeFacts struct {
	names, ids, externals map[string]bool
	unknown               []string
	busy                  string
	err                   error
}

func (f *fakeFacts) Stacks(context.Context, string) (map[string]bool, map[string]bool, error) {
	return f.names, f.ids, f.err
}

func (f *fakeFacts) ExternalNetworks(context.Context, string) (map[string]bool, []string, error) {
	return f.externals, f.unknown, f.err
}

func (f *fakeFacts) Busy(string) (string, bool) { return f.busy, f.busy != "" }

// rowLines projects the rows to name, node and class.
func rowLines(rep OrphanReport) []string {
	out := []string{}
	for _, r := range rep.Rows {
		out = append(out, fmt.Sprintf("%s@%s=%s", r.Name, r.Node, r.Class))
	}
	return out
}

// forgetReport drops the report the reader keeps for its TTL, so the next call judges again.
func forgetReport(as *AddressSpaces, envID string) {
	e := as.entry(envID)
	e.omu.Lock()
	defer e.omu.Unlock()
	e.haveOrphans = false
}

func orphans(t *testing.T, as *AddressSpaces, envID string) OrphanReport {
	t.Helper()
	rep, err := as.Orphans(testsupport.Context(t), envID)
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	return rep
}

func wantRows(t *testing.T, rep OrphanReport, want ...string) {
	t.Helper()
	if got := rowLines(rep); !slices.Equal(got, append([]string{}, want...)) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

// fanOrphans is the fan rig (three managers and a worker) and its reader; cfg tunes each node.
func fanOrphans(t *testing.T, cfg func(i int, c *dockertest.FakeConfig)) (*fanRig, *AddressSpaces) {
	t.Helper()
	f := newFanRig(t, nil, cfg)
	return f, NewAddressSpaces(f.svc, f.agg, f.c, AddressSpaceOptions{Now: f.clk.Now})
}

// The orphan check (plan 10-10, NET-04, D-08 to D-10, D-13). The agents are real sessions of the
// real hub answering from fake daemons, as in addressing_test.go; the clock is the rig's.

// standaloneOrphans is a standalone host holding the networks nets makes (given the rig's now), and
// the reader over it.
func standaloneOrphans(t *testing.T, nets func(now time.Time) []dockertest.FakeNetwork, containers ...dockertest.FakeContainer) (*tunRig, string, *AddressSpaces) {
	t.Helper()
	r := newTunRig(t, nil)
	r.pastGrace()
	env := r.srv.NewEnvironment(t, "Edge")
	r.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Networks: nets(r.clk.Now()), Containers: containers}, nil)
	r.whenOnline(t, env.ID)
	c := NewClusters(r.svc, r.snaps, ClusterOptions{Now: r.clk.Now})
	return r, env.ID, NewAddressSpaces(r.svc, r.agg, c, AddressSpaceOptions{Now: r.clk.Now})
}

func TestOrphansUnusedAfterGrace(t *testing.T) {
	var created time.Time
	r, envID, as := standaloneOrphans(t, func(now time.Time) []dockertest.FakeNetwork {
		created = now.Add(-25 * time.Hour)
		return []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Created: created},
			{ID: "idle", Name: "idle", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}, Created: created},
		}
	})
	rep, err := as.Orphans(testsupport.Context(t), envID)
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v, want only the idle network: bridge is the engine's own", rep.Rows)
	}
	row := rep.Rows[0]
	if row.Name != "idle" || row.Class != netspace.ClassUnused || !row.IdleSince.Equal(created) || row.IdleFromSeen || row.FormerOwner != "" {
		t.Errorf("row = %+v, want idle, unused, idle since its creation and not from a time seen", row)
	}
	if !rep.Complete || len(rep.WithheldNodes) != 0 || rep.ManagerMissing {
		t.Errorf("report = %+v, want a complete check with nothing withheld", rep)
	}
	seen, err := r.st.RQ.ListNetworkSeen(testsupport.Context(t), envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("network_seen = %+v, want a clock for each network of the host", seen)
	}
	for _, s := range seen {
		if s.LastInUseAt != 0 || s.FirstSeenAt != r.clk.Now().UnixNano() {
			t.Errorf("clock = %+v, want first seen now and never in use", s)
		}
	}
}

func TestOrphansClassesOnSwarm(t *testing.T) {
	ns := func(v string) map[string]string { return map[string]string{"com.docker.stack.namespace": v} }
	f, as := fanOrphans(t, func(i int, c *dockertest.FakeConfig) {
		if i == 4 {
			// A stopped container of a stack that is gone, on the worker.
			c.Containers = []dockertest.FakeContainer{{ID: "ghost", Names: []string{"/ghost_job"}, State: "exited", Labels: ns("ghost")}}
			return
		}
		c.Networks = []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true}),
			oldNet(dockertest.FakeNetwork{ID: "n-gone", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: ns("shop")}),
			oldNet(dockertest.FakeNetwork{ID: "n-live", Name: "live_default", Driver: "overlay", Scope: "swarm", Labels: ns("live")}),
			oldNet(dockertest.FakeNetwork{ID: "n-rec", Name: "recorded_default", Driver: "overlay", Scope: "swarm", Labels: ns("recorded")}),
			oldNet(dockertest.FakeNetwork{ID: "n-ghost", Name: "ghost_default", Driver: "overlay", Scope: "swarm", Labels: ns("ghost")}),
			oldNet(dockertest.FakeNetwork{ID: "n-loose", Name: "loose", Driver: "overlay", Scope: "swarm"}),
			oldNet(dockertest.FakeNetwork{ID: "n-used", Name: "used", Driver: "overlay", Scope: "swarm"}),
			freshNet(dockertest.FakeNetwork{ID: "n-new", Name: "fresh_default", Driver: "overlay", Scope: "swarm", Labels: ns("fresh")}),
		}
		c.ServiceRows = []dockertest.FakeService{
			{ID: "svc-l", Name: "live_web", Mode: "replicated", Running: 1, Desired: 1, Labels: ns("live")},
			{ID: "svc-u", Name: "user", Mode: "replicated", Running: 1, Desired: 1, Networks: []string{"n-used"}},
		}
	})
	as.WithStackFacts(&fakeFacts{names: map[string]bool{"recorded": true}})
	rep := orphans(t, as, f.env.ID)
	wantRows(t, rep, "shop_default@Cluster=orphaned", "loose@Cluster=unused")
	o := rep.Rows[0]
	if o.FormerOwner != "shop" || o.FormerOwnerKind != "stack" || o.Scope != "swarm" {
		t.Errorf("orphaned row = %+v, want the former stack shop", o)
	}
	if !rep.Complete || len(rep.WithheldNodes) != 0 || len(rep.WithheldStacks) != 0 || rep.ManagerMissing {
		t.Errorf("report = %+v, want a complete check with nothing withheld", rep)
	}
}

func TestOrphansSpecialNetworksExcluded(t *testing.T) {
	_, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "gw", Name: "docker_gwbridge", Driver: "bridge", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "mesh", Name: "renamed_mesh", Driver: "overlay", Scope: "swarm", Ingress: true}),
			oldNet(dockertest.FakeNetwork{ID: "tpl", Name: "macvlan_tpl", Driver: "macvlan", Scope: "local", ConfigOnly: true}),
			oldNet(dockertest.FakeNetwork{ID: "mv", Name: "macvlan_lan", Driver: "macvlan", Scope: "local", ConfigFrom: "macvlan_tpl"}),
			oldNet(dockertest.FakeNetwork{ID: "tpl2", Name: "referenced_only", Driver: "macvlan", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "mv2", Name: "macvlan_two", Driver: "macvlan", Scope: "local", ConfigFrom: "referenced_only"}),
			oldNet(dockertest.FakeNetwork{ID: "plain", Name: "plain", Driver: "bridge", Scope: "local"}),
		}
	})
	rep := orphans(t, as, envID)
	// The configured macvlan networks are not special, only the templates they name are: they are idle and unlabelled.
	wantRows(t, rep, "macvlan_lan@edge-1=unused", "macvlan_two@edge-1=unused", "plain@edge-1=unused")
}

func TestOrphansKeepLabelAndRow(t *testing.T) {
	r, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "k1", Name: "kept_by_label", Driver: "bridge", Scope: "local", Labels: map[string]string{"coaming.keep": "true"}}),
			oldNet(dockertest.FakeNetwork{ID: "k2", Name: "kept_by_row", Driver: "bridge", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "k3", Name: "label_not_true", Driver: "bridge", Scope: "local", Labels: map[string]string{"coaming.keep": "false"}}),
			oldNet(dockertest.FakeNetwork{ID: "k4", Name: "other", Driver: "bridge", Scope: "local"}),
		}
	})
	agg, _, err := r.agg.Snapshot(testsupport.Context(t), envID)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.Q.InsertNetworkKeep(testsupport.Context(t), sqlc.InsertNetworkKeepParams{
		EnvID: envID, NodeID: agg.Networks[0].NodeID, Name: "kept_by_row", KeptBy: "u1", KeptAt: orphanEpoch.UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	wantRows(t, orphans(t, as, envID), "label_not_true@edge-1=unused", "other@edge-1=unused")
}

func TestOrphansExternalAndBusy(t *testing.T) {
	_, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "e1", Name: "traefik_public", Driver: "bridge", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "e2", Name: "web_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "web"}}),
			oldNet(dockertest.FakeNetwork{ID: "e3", Name: "db_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "db"}}),
		}
	})
	facts := &fakeFacts{externals: map[string]bool{"traefik_public": true}, busy: "web"}
	as.WithStackFacts(facts)
	// A declared external network is excluded, and the network of the stack that holds the lock is skipped.
	wantRows(t, orphans(t, as, envID), "db_default@edge-1=orphaned")
	facts.busy = ""
	forgetReport(as, envID)
	wantRows(t, orphans(t, as, envID), "db_default@edge-1=orphaned", "web_default@edge-1=orphaned")
}

func TestOrphansLocalPerNode(t *testing.T) {
	f, as := fanOrphans(t, func(i int, c *dockertest.FakeConfig) {
		switch i {
		case 1:
			c.Networks = []dockertest.FakeNetwork{oldNet(dockertest.FakeNetwork{ID: "work-m", Name: "work", Driver: "bridge", Scope: "local"})}
			c.Containers = append(c.Containers, dockertest.FakeContainer{
				ID: "cw", Names: []string{"/uses-work"}, State: "running",
				Networks: map[string]dockertest.FakeEndpoint{"work": {NetworkID: "work-m"}},
			})
		case 4:
			c.Networks = []dockertest.FakeNetwork{oldNet(dockertest.FakeNetwork{ID: "work-w", Name: "work", Driver: "bridge", Scope: "local"})}
		}
	})
	rep := orphans(t, as, f.env.ID)
	wantRows(t, rep, "work@wrk-1=unused")
	if rep.Rows[0].Scope != "local" || rep.Rows[0].NodeID == "" {
		t.Errorf("row = %+v, want a local network with the node it is on", rep.Rows[0])
	}
}

func TestOrphansStandaloneProject(t *testing.T) {
	project := func(v string) map[string]string { return map[string]string{"com.docker.compose.project": v} }
	_, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "p1", Name: "shop_default", Driver: "bridge", Scope: "local", Labels: project("shop")}),
			oldNet(dockertest.FakeNetwork{ID: "p2", Name: "blog_default", Driver: "bridge", Scope: "local", Labels: project("blog")}),
		}
	}, dockertest.FakeContainer{ID: "b1", Names: []string{"/blog-db-1"}, State: "exited", Labels: project("blog")})
	rep := orphans(t, as, envID)
	// blog still has a (stopped) container, so its stack exists; shop has nothing left.
	wantRows(t, rep, "shop_default@edge-1=orphaned")
	if rep.Rows[0].FormerOwner != "shop" || rep.Rows[0].FormerOwnerKind != "project" {
		t.Errorf("row = %+v, want the former project shop", rep.Rows[0])
	}
}

func TestOrphansNodeNotReporting(t *testing.T) {
	ns := map[string]string{"com.docker.stack.namespace": "shop"}
	f, as := fanOrphans(t, func(i int, c *dockertest.FakeConfig) {
		if i == 4 {
			return
		}
		c.Networks = []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "n-loose", Name: "loose", Driver: "overlay", Scope: "swarm"}),
			oldNet(dockertest.FakeNetwork{ID: "n-gone", Name: "shop_default", Driver: "overlay", Scope: "swarm", Labels: ns}),
		}
		if i == 1 {
			c.Networks = append(c.Networks, oldNet(dockertest.FakeNetwork{ID: "mine", Name: "mine", Driver: "bridge", Scope: "local"}))
		}
	})
	f.kick(t, f.w)
	if err := f.c.Refresh(testsupport.Context(t), f.env.ID); err != nil {
		t.Fatal(err)
	}
	rep := orphans(t, as, f.env.ID)
	// Nothing is called orphaned or unused while wrk-1 could still hold an attachment; the local network of a node that answered is still judged.
	wantRows(t, rep, "mine@mgr-1=unused", "loose@Cluster=cant_tell", "shop_default@Cluster=cant_tell")
	if rep.Complete || !slices.Equal(rep.WithheldNodes, []string{"wrk-1"}) || rep.ManagerMissing || len(rep.WithheldStacks) != 0 {
		t.Errorf("report = %+v, want the check incomplete and wrk-1 named", rep)
	}
	// The grace clock does not forget networks while a node is out: nothing was pruned.
	seen, err := f.st.RQ.ListNetworkSeen(testsupport.Context(t), f.env.ID)
	if err != nil || len(seen) < 3 {
		t.Errorf("network_seen = %+v, %v, want the clocks kept", seen, err)
	}
}

func TestOrphansManagerMissing(t *testing.T) {
	f, as := fanOrphans(t, func(i int, c *dockertest.FakeConfig) {
		if i != 4 {
			c.Networks = []dockertest.FakeNetwork{
				oldNet(dockertest.FakeNetwork{ID: "n-loose", Name: "loose", Driver: "overlay", Scope: "swarm"}),
				oldNet(dockertest.FakeNetwork{ID: "mine", Name: "mine", Driver: "bridge", Scope: "local"}),
			}
		}
	})
	for _, m := range f.m {
		m.fake.LoseQuorum()
	}
	rep := orphans(t, as, f.env.ID)
	// No manager read the services, so no swarm network is judged; a local network needs no manager.
	if got := rowLines(rep); !slices.Contains(got, "loose@Cluster=cant_tell") || slices.Contains(got, "loose@Cluster=unused") {
		t.Fatalf("rows = %v, want the swarm network held back", got)
	}
	if !rep.ManagerMissing || rep.Complete || len(rep.WithheldNodes) != 0 {
		t.Errorf("report = %+v, want a missing manager and no node named", rep)
	}
}

func TestOrphansUnloadableStackWithholdsEverythingItMightDeclare(t *testing.T) {
	_, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{
			oldNet(dockertest.FakeNetwork{ID: "u1", Name: "loose", Driver: "bridge", Scope: "local"}),
			oldNet(dockertest.FakeNetwork{ID: "u2", Name: "shop_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "shop"}}),
			oldNet(dockertest.FakeNetwork{ID: "u3", Name: "live_default", Driver: "bridge", Scope: "local", Labels: map[string]string{"com.docker.compose.project": "live"}}),
		}
	})
	as.WithStackFacts(&fakeFacts{names: map[string]bool{"live": true}, unknown: []string{"broken"}})
	rep := orphans(t, as, envID)
	wantRows(t, rep, "loose@edge-1=cant_tell", "shop_default@edge-1=cant_tell")
	if !slices.Equal(rep.WithheldStacks, []string{"broken"}) || len(rep.WithheldNodes) != 0 || rep.ManagerMissing || !rep.Complete {
		t.Errorf("report = %+v, want the unloadable stack named and nothing else withheld", rep)
	}
}

func TestOrphansStaleClocksArePrunedOnlyAfterACompletePass(t *testing.T) {
	r, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{oldNet(dockertest.FakeNetwork{ID: "u1", Name: "loose", Driver: "bridge", Scope: "local"})}
	})
	ctx := testsupport.Context(t)
	if err := r.st.Q.UpsertNetworkSeen(ctx, sqlc.UpsertNetworkSeenParams{EnvID: envID, NodeID: "elsewhere", NetworkID: "vanished", Name: "vanished", FirstSeenAt: 1, LastInUseAt: 1}); err != nil {
		t.Fatal(err)
	}
	orphans(t, as, envID)
	seen, err := r.st.RQ.ListNetworkSeen(ctx, envID)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].NetworkID != "u1" {
		t.Fatalf("network_seen = %+v, want only the clock of the network that exists", seen)
	}
}

func TestOrphansInUseStampsAtMostEveryFiveMinutes(t *testing.T) {
	var used dockertest.FakeContainer
	r, envID, as := standaloneOrphans(t, func(time.Time) []dockertest.FakeNetwork {
		return []dockertest.FakeNetwork{oldNet(dockertest.FakeNetwork{ID: "u1", Name: "app", Driver: "bridge", Scope: "local"})}
	}, func() dockertest.FakeContainer {
		used = dockertest.FakeContainer{ID: "c1", Names: []string{"/app-1"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"app": {NetworkID: "u1"}}}
		return used
	}())
	ctx := testsupport.Context(t)
	last := func() int64 {
		seen, err := r.st.RQ.ListNetworkSeen(ctx, envID)
		if err != nil || len(seen) != 1 {
			t.Fatalf("network_seen = %+v, %v", seen, err)
		}
		return seen[0].LastInUseAt
	}
	setLast := func(at time.Time) {
		if _, err := r.st.Write.ExecContext(ctx, `UPDATE network_seen SET last_in_use_at = ? WHERE env_id = ?`, at.UnixNano(), envID); err != nil {
			t.Fatal(err)
		}
		forgetReport(as, envID)
	}
	wantRows(t, orphans(t, as, envID))
	if got, now := last(), r.clk.Now().UnixNano(); got != now {
		t.Fatalf("last in use = %d, want now %d for a network seen in use for the first time", got, now)
	}
	// Seen in use again within five minutes of the stored time: nothing is written.
	recent := r.clk.Now().Add(-4 * time.Minute)
	setLast(recent)
	orphans(t, as, envID)
	if got := last(); got != recent.UnixNano() {
		t.Errorf("last in use moved to %d, want it left at %d: it is under five minutes old", got, recent.UnixNano())
	}
	// Over five minutes old: it moves to now.
	setLast(r.clk.Now().Add(-6 * time.Minute))
	orphans(t, as, envID)
	if got, now := last(), r.clk.Now().UnixNano(); got != now {
		t.Errorf("last in use = %d, want now %d: the stored time was six minutes old", got, now)
	}
}
