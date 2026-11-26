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
	return newStatusRigAt(t, time.Time{}, tasks...)
}

// newStatusRigAt is newStatusRig with the deployer's clock standing at the instant, unless it is the zero time.
func newStatusRigAt(t *testing.T, at time.Time, tasks ...swarm.Task) *statusRig {
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
			if !at.IsZero() {
				d.Clock = fixedClock{at: at}
			}
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

// TestVIPStatusHolderStates pins the three answers about the holder, and that an environment with no row is not an empty
// status (VIP-03).
func TestVIPStatusHolderStates(t *testing.T) {
	lost := vipT0.Add(5 * time.Minute)
	t.Run("none", func(t *testing.T) {
		s := newStatusRig(t)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
			{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateBackup},
			{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateStop},
		}, lost)
		st := s.mustStatus()
		if st.Holder.State != ingressdeploy.VIPHolderNone || len(st.Holder.NodeIDs) != 0 || !st.HolderlessSince.Equal(lost) {
			t.Fatalf("holder = %+v since %v, want none since %v", st.Holder, st.HolderlessSince, lost)
		}
	})
	t.Run("many", func(t *testing.T) {
		s := newStatusRig(t)
		s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
			{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateMaster},
			{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster},
		}, time.Time{})
		st := s.mustStatus()
		if st.Holder.State != ingressdeploy.VIPHolderMany || len(st.Holder.NodeIDs) != 2 ||
			st.Holder.NodeIDs[0] != vipNodeA || st.Holder.NodeIDs[1] != vipNodeB {
			t.Fatalf("holder = %+v, want many listing both nodes in order", st.Holder)
		}
		if !st.HolderlessSince.IsZero() {
			t.Fatalf("HolderlessSince = %v with holders", st.HolderlessSince)
		}
	})
	t.Run("reader has not read", func(t *testing.T) {
		s := newStatusRig(t)
		st := s.mustStatus()
		if st.Holder.State != ingressdeploy.VIPHolderNone || len(st.Candidates) != 0 {
			t.Fatalf("status before any read = %+v, want no holder and no candidates", st)
		}
	})
	t.Run("no row", func(t *testing.T) {
		s := newStatusRig(t)
		if _, err := s.istore.DeleteVIP(testsupport.Context(t), s.envID); err != nil {
			t.Fatalf("DeleteVIP: %v", err)
		}
		st, err := s.status()
		if !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			t.Fatalf("VIPStatus without a row = %+v, %v; want ErrVIPNotEnabled", st, err)
		}
	})
}

// TestVIPStatusOrder pins that candidates come back ordered by host name, then node ID.
func TestVIPStatusOrder(t *testing.T) {
	s := newStatusRig(t)
	zID, bID := strings.Repeat("z", 25), strings.Repeat("b", 25)
	s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
		{NodeID: "id-m2", Hostname: "m2"}, {NodeID: "id-m1", Hostname: "m1"},
		{NodeID: zID, Hostname: "a"}, {NodeID: bID, Hostname: "a"},
	}, time.Time{})
	var got []string
	for _, c := range s.mustStatus().Candidates {
		got = append(got, c.Hostname+"("+c.NodeID+")")
	}
	want := []string{"a(" + bID + ")", "a(" + zID + ")", "m1(id-m1)", "m2(id-m2)"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

// TestVIPStatusWarnings pins the foreign router, the clock skew, the unreadable manager table and the ensure state.
func TestVIPStatusWarnings(t *testing.T) {
	s := newStatusRig(t)
	ensured := vipT0.Add(2 * time.Hour)
	if err := s.istore.RecordVIPEnsure(testsupport.Context(t), s.envID, ensured, ingressdeploy.EnsureEnableFailed); err != nil {
		t.Fatalf("RecordVIPEnsure: %v", err)
	}
	s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
		{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster, Foreign: true, ForeignSender: "10.0.0.4"},
		{NodeID: vipNodeB, Hostname: "node-b", State: ingressbundle.VIPStateBackup, ClockSkew: true},
	}, time.Time{})
	s.managers.err = errors.New("no manager list")

	st := s.mustStatus()
	if !st.ForeignRouter || st.ForeignSender != netip.MustParseAddr("10.0.0.4") {
		t.Fatalf("foreign router = %v from %v, want true from 10.0.0.4", st.ForeignRouter, st.ForeignSender)
	}
	if len(st.ClockSkewNodes) != 1 || st.ClockSkewNodes[0] != vipNodeB {
		t.Fatalf("clock skew nodes = %v, want [%s]", st.ClockSkewNodes, vipNodeB)
	}
	if st.ManagersReadable || len(st.Managers) != 0 {
		t.Fatalf("managers = %+v readable %v, want an unreadable table", st.Managers, st.ManagersReadable)
	}
	if !st.EnsureAt.Equal(ensured) || st.EnsureError != ingressdeploy.EnsureEnableFailed {
		t.Fatalf("ensure = %v %q, want %v %q", st.EnsureAt, st.EnsureError, ensured, ingressdeploy.EnsureEnableFailed)
	}

	// A sender that is not an address is never shown, though the warning stands.
	s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
		{NodeID: vipNodeA, Hostname: "node-a", Foreign: true, ForeignSender: "not an address"},
	}, time.Time{})
	if st := s.mustStatus(); !st.ForeignRouter || st.ForeignSender.IsValid() {
		t.Fatalf("a bad sender gave foreign %v sender %v, want the warning without the sender", st.ForeignRouter, st.ForeignSender)
	}

	// A readable table is carried as read.
	s.managers.err = nil
	s.managers.rows = []envs.ManagerPolicyRow{{NodeID: vipNodeA, Hostname: "node-a", State: envs.ManagerAllows}}
	if st := s.mustStatus(); !st.ManagersReadable || len(st.Managers) != 1 || st.Managers[0].State != envs.ManagerAllows {
		t.Fatalf("managers = %+v readable %v, want the one row", st.Managers, st.ManagersReadable)
	}
}

// TestVIPHolds pins the drain hook's question (D-25): whether the node holds the address and whether it holds it alone.
func TestVIPHolds(t *testing.T) {
	cases := []struct {
		name                 string
		other                string
		otherReady           bool
		wantHolds, wantAlone bool
	}{
		{"another BACKUP with a ready proxy", ingressbundle.VIPStateBackup, true, true, false},
		{"the other is FAULT", ingressbundle.VIPStateFault, true, true, true},
		{"the other's proxy is not ready", ingressbundle.VIPStateBackup, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStatusRig(t)
			s.nodes.rows = []ingresshub.Node{readyProxy(vipNodeA)}
			if c.otherReady {
				s.nodes.rows = append(s.nodes.rows, readyProxy(vipNodeB))
			}
			s.svc.SeedVIPState(s.envID, []ingressdeploy.SeedNode{
				{NodeID: vipNodeA, Hostname: "node-a", State: ingressbundle.VIPStateMaster},
				{NodeID: vipNodeB, Hostname: "node-b", State: c.other},
			}, time.Time{})
			holds, alone, err := s.svc.VIPHolds(testsupport.Context(t), s.envID, vipNodeA)
			if err != nil || holds != c.wantHolds || alone != c.wantAlone {
				t.Fatalf("VIPHolds(holder) = %v, %v, %v; want %v, %v", holds, alone, err, c.wantHolds, c.wantAlone)
			}
			if holds, alone, err := s.svc.VIPHolds(testsupport.Context(t), s.envID, vipNodeB); err != nil || holds || alone {
				t.Fatalf("VIPHolds(non-holder) = %v, %v, %v; want false, false", holds, alone, err)
			}
		})
	}
	t.Run("no virtual IP", func(t *testing.T) {
		s := newStatusRig(t)
		if _, err := s.istore.DeleteVIP(testsupport.Context(t), s.envID); err != nil {
			t.Fatalf("DeleteVIP: %v", err)
		}
		if holds, alone, err := s.svc.VIPHolds(testsupport.Context(t), s.envID, vipNodeA); err != nil || holds || alone {
			t.Fatalf("VIPHolds without a row = %v, %v, %v; want false, false, nil", holds, alone, err)
		}
	})
}
