// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
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
