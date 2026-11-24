// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// statusNodes lists the proxy node rows a test sets.
type statusNodes struct {
	rows []ingresshub.Node
	err  error
}

func (f *statusNodes) ListNodes(context.Context, string) ([]ingresshub.Node, error) {
	return f.rows, f.err
}

// statusManagers answers the manager table a test sets.
type statusManagers struct {
	rows []envs.ManagerPolicyRow
	err  error
}

func (f *statusManagers) ManagerPolicies(context.Context, string) ([]envs.ManagerPolicyRow, error) {
	return f.rows, f.err
}

const statusBundle = "bundle-1"

// statusRig is a swarm with a virtual IP row, a keepalived service whose tasks a test lists, and the proxy node rows and
// manager table the status reads. Nothing is read from a log: the reader's memory is written by hand.
type statusRig struct {
	*rig
	nodes    *statusNodes
	managers *statusManagers
}

// newStatusRig builds the rig over tasks, the keepalived tasks of the fake swarm.
func newStatusRig(t *testing.T, tasks ...swarm.Task) *statusRig {
	t.Helper()
	s := &statusRig{nodes: &statusNodes{}, managers: &statusManagers{}}
	s.rig = newRig(t, rigOptions{
		config: func(c *dockertest.FakeConfig) {
			c.ServiceRows = []dockertest.FakeService{{ID: "ka-svc", Name: ingress.ServiceNameKeepalived, Mode: "global"}}
			for i := range tasks {
				tasks[i].ServiceID = "ka-svc"
			}
			c.Tasks = tasks
		},
		deps: func(d *ingressdeploy.Deps) {
			d.Nodes = s.nodes
			d.Managers = s.managers
			d.CurrentBundle = func(string) (string, bool) { return statusBundle, true }
		},
	})
	err := s.istore.PutVIP(testsupport.Context(t), ingressstore.VIPConfig{
		EnvID: s.envID, Address: netip.MustParseAddr("192.168.10.250"), PrefixLen: 24, RouterID: 51,
		RouterDerived: true, KeySealed: "sealed", KeyGeneration: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("PutVIP: %v", err)
	}
	return s
}

func (s *statusRig) status() (ingressdeploy.VIPStatus, error) {
	s.t.Helper()
	return s.svc.VIPStatus(testsupport.Context(s.t), s.envID)
}

func (s *statusRig) mustStatus() ingressdeploy.VIPStatus {
	s.t.Helper()
	st, err := s.status()
	if err != nil {
		s.t.Fatalf("VIPStatus: %v", err)
	}
	return st
}

// keepalivedTask is a keepalived task on the node, in the state, made at the instant.
func keepalivedTask(id, node, state string, at time.Time) swarm.Task {
	t := swarm.Task{ID: id, NodeID: node, DesiredState: swarm.TaskStateRunning}
	t.CreatedAt = at
	t.Status = swarm.TaskStatus{State: swarm.TaskState(state), Timestamp: at}
	return t
}

// readyProxy is the row of an enrolled proxy node on the current bundle.
func readyProxy(node string) ingresshub.Node {
	return ingresshub.Node{SwarmNodeID: node, Status: "active", Health: "ok", AppliedBundleID: statusBundle}
}

// TestVIPStatusOneHolder is the tracer of the status (D-22): one node holds the address, the other is a BACKUP whose
// keepalived task runs, and every field the status carries for them is right.
func TestVIPStatusOneHolder(t *testing.T) {
	at := vipT0.Add(30 * time.Second)
	s := newStatusRig(t,
		keepalivedTask("task-a", vipNodeA, "running", at),
		keepalivedTask("task-b", vipNodeB, "starting", at),
	)
	heard := vipT0.Add(time.Minute)
	s.nodes.rows = []ingresshub.Node{readyProxy(vipNodeA), {SwarmNodeID: vipNodeB, Status: "active", Health: "ok", AppliedBundleID: "old-bundle"}}
	s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
		{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster, LastHeard: heard},
		{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateFault, Reason: ingressbundle.VIPReasonProbeFailing, LastHeard: heard},
	}, time.Time{})

	st := s.mustStatus()
	if !st.Enabled || st.Address != netip.MustParsePrefix("192.168.10.250/24") || st.RouterID != 51 || !st.RouterDerived || st.KeyGeneration != 2 {
		t.Fatalf("the configuration = %+v", st)
	}
	if st.Holder.State != ingressdeploy.VIPHolderOne || len(st.Holder.NodeIDs) != 1 || st.Holder.NodeIDs[0] != vipNodeA {
		t.Fatalf("holder = %+v, want one: %s", st.Holder, vipNodeA)
	}
	if !st.HolderlessSince.IsZero() {
		t.Fatalf("HolderlessSince = %v while a node holds the address", st.HolderlessSince)
	}
	if len(st.Candidates) != 2 {
		t.Fatalf("%d candidates, want 2: %+v", len(st.Candidates), st.Candidates)
	}
	a, b := st.Candidates[0], st.Candidates[1]
	if a.NodeID != vipNodeA || a.Hostname != "node-a" || a.VRRPState != ingressbundle.VIPStateMaster || !a.ProxyReady ||
		a.TaskState != "running" || !a.TaskStarted.Equal(at) || a.LastFault != "" || !a.LastHeard.Equal(heard) {
		t.Fatalf("candidate a = %+v", a)
	}
	if b.NodeID != vipNodeB || b.VRRPState != ingressbundle.VIPStateFault || b.ProxyReady || b.TaskState != "starting" ||
		!b.TaskStarted.IsZero() || b.LastFault != ingressbundle.VIPReasonProbeFailing {
		t.Fatalf("candidate b = %+v (its proxy is on an old bundle, its task is not running yet and it reported probe_failing)", b)
	}
}

var _ = errors.New
var _ = strings.Repeat
