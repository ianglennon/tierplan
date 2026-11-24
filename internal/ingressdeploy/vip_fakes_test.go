// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipTestAddress is an address inside the network of the fake swarm's three nodes.
const vipTestAddress = "10.9.0.50/24"

// fakeManagers is the table of the managers' privileged-services opt-in a test sets.
type fakeManagers struct {
	mu   sync.Mutex
	rows []envs.ManagerPolicyRow
	err  error
}

func (f *fakeManagers) ManagerPolicies(context.Context, string) ([]envs.ManagerPolicyRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]envs.ManagerPolicyRow(nil), f.rows...), f.err
}

func (f *fakeManagers) set(rows []envs.ManagerPolicyRow, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

// vipManagerRows is the three managers of the fake swarm, each in the state given.
func vipManagerRows(states ...string) []envs.ManagerPolicyRow {
	rows := make([]envs.ManagerPolicyRow, 0, len(states))
	for i, st := range states {
		rows = append(rows, envs.ManagerPolicyRow{NodeID: "node-" + string(rune('1'+i)), Hostname: "manager-" + string(rune('1'+i)), State: st})
	}
	return rows
}

// vipNodeRows is three managers whose addresses are 10.9.0.11 to 10.9.0.13.
func vipNodeRows() []dockertest.FakeNode {
	return []dockertest.FakeNode{
		{ID: "node-1", Hostname: "manager-1", Role: "manager", Leader: true, Address: "10.9.0.11"},
		{ID: "node-2", Hostname: "manager-2", Role: "manager", Address: "10.9.0.12"},
		{ID: "node-3", Hostname: "manager-3", Role: "manager", Address: "10.9.0.13"},
	}
}

// vipEnableRig is a rig whose swarm has three managers that allow privileged services and whose ingress is on.
type vipEnableRig struct {
	*rig
	managers *fakeManagers
	// base is the swarm's configuration without a test's change, for a Reconfigure that undoes it.
	base dockertest.FakeConfig
}

// newVIPEnableRig builds a vipEnableRig. config, when set, adjusts the fake swarm after the nodes are in place.
func newVIPEnableRig(t *testing.T, config func(*dockertest.FakeConfig), deps func(*ingressdeploy.Deps)) *vipEnableRig {
	t.Helper()
	managers := &fakeManagers{rows: vipManagerRows(envs.ManagerAllows, envs.ManagerAllows, envs.ManagerAllows)}
	v := &vipEnableRig{managers: managers}
	v.rig = newRig(t, rigOptions{
		config: func(c *dockertest.FakeConfig) {
			c.NodeRows = vipNodeRows()
			v.base = *c
			if config != nil {
				config(c)
			}
		},
		deps: func(d *ingressdeploy.Deps) {
			d.Managers = managers
			if deps != nil {
				deps(d)
			}
		},
	})
	if _, err := v.istore.SetEnabled(testsupport.Context(t), v.envID, true, "u-admin", nil); err != nil {
		t.Fatalf("switch ingress on: %v", err)
	}
	return v
}

// enableVIP starts a virtual IP enable and waits for its job to end. The job is returned whatever its outcome.
func (v *vipEnableRig) enableVIP(req ingressdeploy.VIPRequest) jobs.Job {
	v.t.Helper()
	id, err := v.svc.EnableVIP(testsupport.Context(v.t), testActor, testOwner, v.envID, req)
	if err != nil {
		v.t.Fatalf("EnableVIP: %v", err)
	}
	return v.wait(id)
}
