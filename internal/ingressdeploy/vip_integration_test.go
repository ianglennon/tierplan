// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The deployer's virtual IP regression on dind (D-29, plan 26-31): the real keepalived image, run from the spec the deployer
// builds, over real proxies that enrolled and read ready. EnableVIP, DisableVIP, the ingress switch-off and the reader
// (RunVIPReader) are the production code; only the agents and the HTTP layer are the harness's, as in the other tests of the
// package. The bounds are failure deadlines, never pauses: every wait returns the moment its condition holds.
const (
	// vipHolderBound is how long the first holder may take once the job is done. S3 measured about 5.5 s for the election
	// itself; the rest is the images' start and the reader's first pass.
	vipHolderBound = 60 * time.Second
	// vipMoveBound is how long the address may take to be on the other node after the holder's proxy is stopped (VIP-02;
	// S3 measured 3.0 to 3.8 s).
	vipMoveBound = 15 * time.Second
	// vipReadBound is how long the reader may take to show on the status or in the history what the nodes already did: one
	// pass of VIPReadInterval and the reads of the nodes' logs.
	vipReadBound = 60 * time.Second
	// vipProxyBack is how long swarm may take to run a new proxy task on the node whose proxy was stopped, and the helper
	// to say BACKUP again once the proxy answers its readiness probe: the proxy's image start, its enrolment and the
	// next bundle.
	vipProxyBack = 6 * time.Minute
)

// vipEnv is a swarm with ingress on, its proxies ready and the virtual IP on, waiting for a holder.
type vipEnv struct {
	*swarmEnv
	addr netip.Prefix
	// ids is the swarm node ID of each node.
	ids map[*dockertest.Node]string
}

// startVIPEnv starts the swarm, switches ingress on, waits for both proxies and switches the virtual IP on. The jobs are
// done when it returns, and nothing has waited for a holder yet.
func startVIPEnv(t *testing.T) *vipEnv {
	t.Helper()
	e := &vipEnv{swarmEnv: startSwarmEnv(t), ids: map[*dockertest.Node]string{}}
	for _, n := range e.sw.Nodes() {
		e.ids[n] = e.swarmNodeID(n)
	}
	requireJobDone(t, e.enable())
	e.awaitReady()
	e.addr = e.vipAddress()
	requireJobDone(t, e.enableVIP(e.addr))
	return e
}

// nodeOf is the node with the swarm node ID, nil when none has it.
func (e *vipEnv) nodeOf(id string) *dockertest.Node {
	for n, nid := range e.ids {
		if nid == id {
			return n
		}
	}
	return nil
}

// otherThan is the node that is not n; the harness swarm has two.
func (e *vipEnv) otherThan(n *dockertest.Node) *dockertest.Node {
	for _, o := range e.sw.Nodes() {
		if o != n {
			return o
		}
	}
	e.t.Fatalf("the swarm has no node but %s", n.Hostname)
	return nil
}

// candidate is the status's row for the node, and false when it has none.
func candidate(st ingressdeploy.VIPStatus, id string) (ingressdeploy.VIPCandidate, bool) {
	for _, c := range st.Candidates {
		if c.NodeID == id {
			return c, true
		}
	}
	return ingressdeploy.VIPCandidate{}, false
}

// awaitHeld waits until the status names exactly one holder whose proxy is ready, that node's interface lists the address
// and no other node's does, and every node is a candidate with its VRRP state and its task state filled in. It returns that
// status and the holder.
func (e *vipEnv) awaitHeld() (ingressdeploy.VIPStatus, *dockertest.Node) {
	t := e.t
	t.Helper()
	var st ingressdeploy.VIPStatus
	var holder *dockertest.Node
	dockertest.Wait(t, vipHolderBound, "one holder whose proxy is ready, listing the address, and every candidate's states", func() (bool, string) {
		var err error
		if st, err = e.vipStatus(); err != nil {
			return false, err.Error()
		}
		if st.Holder.State != ingressdeploy.VIPHolderOne || len(st.Holder.NodeIDs) != 1 {
			return false, fmt.Sprintf("holder %s %v", st.Holder.State, st.Holder.NodeIDs)
		}
		holder = e.nodeOf(st.Holder.NodeIDs[0])
		if holder == nil {
			return false, "the holder " + st.Holder.NodeIDs[0] + " is not a node of the swarm"
		}
		if got := e.holderNodes(e.addr.Addr()); len(got) != 1 || got[0] != holder {
			return false, "interfaces: holders " + vhNames(got) + ", status says " + holder.Hostname
		}
		if len(st.Candidates) != len(e.sw.Nodes()) {
			return false, fmt.Sprintf("%d candidates", len(st.Candidates))
		}
		for _, c := range st.Candidates {
			if c.VRRPState == "" || c.TaskState == "" {
				return false, fmt.Sprintf("candidate %s: vrrp %q task %q", c.Hostname, c.VRRPState, c.TaskState)
			}
		}
		if c, _ := candidate(st, st.Holder.NodeIDs[0]); !c.ProxyReady {
			return false, "the holder's proxy is not ready"
		}
		return true, ""
	})
	return st, holder
}

// proxyContainer is the ID of the node's running proxy container, "" while it has none.
func (e *vipEnv) proxyContainer(n *dockertest.Node) string {
	e.t.Helper()
	out, err := n.Exec(testsupport.Context(e.t), "docker", "ps", "-q", "--no-trunc", "--filter", "status=running",
		"--filter", "label=com.docker.swarm.service.name="+ingress.ServiceName)
	if err != nil {
		e.t.Fatalf("list the proxy containers of %s: %v", n.Hostname, err)
	}
	if ids := strings.Fields(out); len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// keepalivedLeft reports what the virtual IP leaves on the swarm: the keepalived service and the key secrets by their names,
// and the nodes whose interfaces list the address. Each is a count, so a failure says which.
func (e *vipEnv) keepalivedLeft() (services, secrets int, holders []*dockertest.Node) {
	e.t.Helper()
	if err := e.cluster(func(ctx context.Context, c *dockerx.Client) error {
		if _, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived); err == nil {
			services++
		} else if !errors.Is(err, dockerx.ErrServiceNotFound) {
			return err
		}
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if strings.HasPrefix(r.Name, ingress.SecretPrefixKeepalived) {
				secrets++
			}
		}
		return nil
	}); err != nil {
		e.t.Fatalf("read the swarm's keepalived objects: %v", err)
	}
	return services, secrets, e.holderNodes(e.addr.Addr())
}

// requireVIPGone fails the test unless nothing of the virtual IP is left: no service, no key secret, no row, no status and
// no address on any node's interface (PROH-26-06).
func (e *vipEnv) requireVIPGone() {
	t := e.t
	t.Helper()
	if svc, sec, held := e.keepalivedLeft(); svc != 0 || sec != 0 || len(held) != 0 {
		t.Errorf("the swarm still holds %d keepalived service(s), %d key secret(s) and the address on %s", svc, sec, vhNames(held))
	}
	if _, err := e.istore.GetVIP(testsupport.Context(t), e.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("the virtual IP row after the removal: err %v, want ErrVIPNotFound", err)
	}
	if _, err := e.vipStatus(); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
		t.Errorf("the virtual IP status after the removal: err %v, want ErrVIPNotEnabled", err)
	}
}

// moves is the environment's stored moves, newest first.
func (e *vipEnv) moves() []ingressstore.VIPMove {
	e.t.Helper()
	rows, _, err := e.deploy.VIPMoves(testsupport.Context(e.t), e.envID, nil, 50)
	if err != nil {
		e.t.Fatalf("read the moves: %v", err)
	}
	return rows
}

// TestVIPEnableHoldsOnSwarm is D-29 and SC 1 and 2, the tracer: on a swarm of a manager and a worker with real proxies that
// enrolled and read ready, EnableVIP with an address of the nodes' network runs its job to done, and one holder, whose proxy
// is ready, takes the address: the status names it, its interface lists the address and no other node's does, and every
// candidate's VRRP state and task state are the running reader's and the swarm's.
func TestVIPEnableHoldsOnSwarm(t *testing.T) {
	e := startVIPEnv(t)

	st, holder := e.awaitHeld()
	if !st.Enabled || st.Address != e.addr || st.KeyGeneration != 1 || !st.RouterDerived {
		t.Errorf("status = enabled %v address %s generation %d derived %v, want on, %s, generation 1 and a derived router ID", st.Enabled, st.Address, st.KeyGeneration, st.RouterDerived, e.addr)
	}
	states := map[string]int{}
	for _, c := range st.Candidates {
		states[c.VRRPState]++
		if c.TaskState != "running" {
			t.Errorf("%s: the keepalived task is %q, want running", c.Hostname, c.TaskState)
		}
		if c.LastHeard.IsZero() {
			t.Errorf("%s: the reader has not heard the node", c.Hostname)
		}
	}
	if states[ingressbundle.VIPStateMaster] != 1 || states[ingressbundle.VIPStateBackup] != len(e.sw.Nodes())-1 {
		t.Errorf("candidate states %v, want one MASTER and the rest BACKUP", states)
	}
	t.Logf("the virtual IP %s is held by %s on engine %s", e.addr, holder.Hostname, e.sw.Engine)
}

// TestVIPMovesWhenTheHolderProxyStops is D-29, VIP-02 and VIP-03: stopping the holder's proxy container makes the other node
// hold the address within vipMoveBound, the reader stores the move with the reason probe_failing, and when swarm runs a proxy
// on the old holder again the old holder stays BACKUP and the address stays where it moved (nopreempt). The moves stored
// before the proxy is stopped are counted and left out: two helpers that both claim the address at the first election are
// stored as a move, which is not what this test is about.
func TestVIPMovesWhenTheHolderProxyStops(t *testing.T) {
	e := startVIPEnv(t)
	_, holder := e.awaitHeld()
	other := e.otherThan(holder)
	first := len(e.moves())

	stopped := e.proxyContainer(holder)
	if stopped == "" {
		t.Fatalf("%s runs no proxy container", holder.Hostname)
	}
	e.sh(holder, "docker stop "+stopped)

	dockertest.Wait(t, vipMoveBound, other.Hostname+" to hold the address alone", func() (bool, string) {
		got := e.holderNodes(e.addr.Addr())
		return len(got) == 1 && got[0] == other, "holders " + vhNames(got)
	})

	dockertest.Wait(t, vipReadBound, "the reader to store the move from "+holder.Hostname+" to "+other.Hostname, func() (bool, string) {
		rows := e.moves()
		return len(rows) == first+1, fmt.Sprintf("%d moves, %d before the proxy was stopped", len(rows), first)
	})
	mv := e.moves()[0]
	if mv.FromNodeID != e.ids[holder] || mv.ToNodeID != e.ids[other] || mv.FromNodeName != holder.Hostname || mv.ToNodeName != other.Hostname || mv.Reason != "probe_failing" {
		t.Errorf("the move is %s (%s) -> %s (%s) for %q, want %s (%s) -> %s (%s) for probe_failing",
			mv.FromNodeName, mv.FromNodeID, mv.ToNodeName, mv.ToNodeID, mv.Reason, holder.Hostname, e.ids[holder], other.Hostname, e.ids[other])
	}

	// Swarm restarts the proxy on the old holder; once it answers, the helper there leaves FAULT for BACKUP and stays there.
	dockertest.Wait(t, vipProxyBack, "swarm to run a new proxy task on "+holder.Hostname, func() (bool, string) {
		id := e.proxyContainer(holder)
		return id != "" && id != stopped, "container " + id
	})
	dockertest.Wait(t, vipProxyBack, holder.Hostname+" to report BACKUP and "+other.Hostname+" to be the one holder", func() (bool, string) {
		st, err := e.vipStatus()
		if err != nil {
			return false, err.Error()
		}
		c, ok := candidate(st, e.ids[holder])
		return ok && c.VRRPState == ingressbundle.VIPStateBackup && st.Holder.State == ingressdeploy.VIPHolderOne && slices.Equal(st.Holder.NodeIDs, []string{e.ids[other]}),
			fmt.Sprintf("old holder %q, holders %v", c.VRRPState, st.Holder.NodeIDs)
	})
	if v := vhWatch(vhHold, func() string {
		switch got := e.holderNodes(e.addr.Addr()); {
		case slices.Contains(got, holder):
			return holder.Hostname + " took the address back"
		case len(got) != 1 || got[0] != other:
			return "holders " + vhNames(got)
		}
		if st, err := e.vipStatus(); err != nil {
			return err.Error()
		} else if c, _ := candidate(st, e.ids[holder]); c.VRRPState == ingressbundle.VIPStateMaster {
			return holder.Hostname + " reports MASTER"
		}
		return ""
	}); v != "" {
		t.Fatalf("nopreempt violated: %s", v)
	}
	if rows := e.moves(); len(rows) != first+1 {
		t.Errorf("%d moves after the old holder came back, want %d: the one the stop made", len(rows), first+1)
	}
}

// TestVIPDisableLeavesNothing is D-16 and PROH-26-06: DisableVIP runs to done and leaves no keepalived service, no key secret,
// no stored row and the address on no node's interface. A move the reader stored while the switch-off ran has the reason
// disabled.
//
// Whether any move is stored during the switch-off is not asserted: the service's removal stops every node's keepalived
// together, so the other node is sometimes stopped before the holder's priority-0 advert promotes it, and the reader records
// only moves between two nodes (D-23), never the address leaving for no node. What is asserted is that a move that does land
// in the window is never given another reason. A move stored before the switch-off began (two helpers that both claimed the
// address at the first election) is not the switch-off's and is left out.
func TestVIPDisableLeavesNothing(t *testing.T) {
	e := startVIPEnv(t)
	_, holder := e.awaitHeld()
	before := map[int64]bool{}
	for _, m := range e.moves() {
		before[m.ID] = true
		t.Logf("before the switch-off: move %d from %s to %s (%s) at %s", m.ID, m.FromNodeName, m.ToNodeName, m.Reason, m.At.Format(time.RFC3339Nano))
	}

	job := e.disableVIP()
	requireJobDone(t, job)
	e.requireVIPGone()

	for _, m := range e.moves() {
		if before[m.ID] {
			continue
		}
		t.Logf("during the switch-off (holder was %s): move %d from %s to %s (%s)", holder.Hostname, m.ID, m.FromNodeName, m.ToNodeName, m.Reason)
		if m.Reason != "disabled" {
			t.Errorf("a move from %s to %s stored during the switch-off has the reason %q, want disabled", m.FromNodeName, m.ToNodeName, m.Reason)
		}
	}
}

// TestIngressDisableRemovesVIPOnSwarm is D-17: switching ingress off with the virtual IP on removes the virtual IP first and
// then the proxies, and nothing of either is left. The order is the order the job's items ended in, read from the job's own
// events.
func TestIngressDisableRemovesVIPOnSwarm(t *testing.T) {
	e := startVIPEnv(t)
	e.awaitHeld()

	job := e.disable()
	requireJobDone(t, job)

	ended := e.itemsEnded(job.ID)
	vip, svc := ended[ingressdeploy.ItemVirtualIP], ended[ingressdeploy.ItemService]
	if vip == 0 || svc == 0 || vip >= svc {
		t.Errorf("the item %s ended at event %d and the item %s at event %d, want the virtual IP's first", ingressdeploy.ItemVirtualIP, vip, ingressdeploy.ItemService, svc)
	}

	e.requireVIPGone()
	if svc, net, secrets := e.objects(); svc != 0 || net != 0 || secrets != 0 {
		t.Errorf("the swarm still holds %d proxy service(s), %d network(s) and %d join token secret(s)", svc, net, secrets)
	}
}

// itemsEnded is, by item, the number of the job's event in which the item was first reported done. It reads the events the
// hub kept of the finished job, so it holds the order the job ran in and no clock.
func (e *vipEnv) itemsEnded(jobID string) map[string]uint64 {
	t := e.t
	t.Helper()
	ctx, cancel := context.WithTimeout(testsupport.Context(t), time.Minute)
	defer cancel()
	// The events after the first one: the job's first event is the first item starting.
	_, events, stop, err := e.jobs.Subscribe(ctx, jobID, 1)
	if err != nil {
		t.Fatalf("read the events of job %s: %v", jobID, err)
	}
	defer stop()
	ended := map[string]uint64{}
	for ev := range events {
		if ev.Type == jobs.EventItem && ev.Item != nil && ev.Item.State == jobs.ItemDone {
			if _, seen := ended[ev.Item.ID]; !seen {
				ended[ev.Item.ID] = ev.ID
			}
		}
	}
	return ended
}
