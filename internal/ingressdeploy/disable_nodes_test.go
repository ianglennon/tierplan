// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// strayNodes is a swarm of three nodes of which the environment's agents know two (the manager and the worker). The third,
// "worker-2", has no agent: it still runs a proxy task and holds the cache volume with every certificate's private key.
func strayNodes(c *dockertest.FakeConfig) {
	c.NodeRows = []dockertest.FakeNode{
		{ID: "node-1", Hostname: "manager-1", Role: "manager", Leader: true},
		{ID: "node-2", Hostname: "worker-1", Role: "worker"},
		{ID: "node-3", Hostname: "worker-2", Role: "worker"},
	}
}

// newStrayFix is a disable fixture whose swarm has a node no agent covers.
func newStrayFix(t *testing.T) *disableFix {
	t.Helper()
	return newDisableFix(t, disableOpts{worker: true, config: strayNodes})
}

// TestDisableQueuesWipesForSwarmNodesWithoutAnAgent: the nodes a switch-off wipes are the swarm's own node list, not only the
// nodes the agents know (WR-04). A node with no agent is named in the preview as unreachable, is queued as a pending wipe,
// keyed by its swarm node ID, and is named in the volumes result; the job is done, not failed. When an agent for it
// is installed later and connects, the wipe loop removes the volume and clears the entry.
func TestDisableQueuesWipesForSwarmNodesWithoutAnAgent(t *testing.T) {
	f := newStrayFix(t)
	ctx := testsupport.Context(t)

	prev, err := f.svc.PreviewDisable(ctx, f.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if !slices.Equal(prev.Unreachable, []string{"worker-2"}) {
		t.Errorf("preview unreachable = %v, want the node no agent covers", prev.Unreachable)
	}
	if len(prev.Removes.Volumes) != 3 {
		t.Errorf("preview volumes = %+v, want all three swarm nodes", prev.Removes.Volumes)
	}

	job := f.disable(seesAll)
	wantDisableDone(t, job)
	it := itemOf(job, ingressdeploy.ItemVolumes)
	if it.Result["unreachable"] != "worker-2" || it.Result["pending"] != "1" || it.Result["wiped"] != "2" {
		t.Fatalf("volumes result = %v, want worker-2 named as not wiped, two wiped", it.Result)
	}
	pending, err := f.istore.PendingWipes(ctx, f.envID)
	if err != nil || len(pending) != 1 || pending[0].SwarmNodeID != "node-3" || pending[0].Volume != ingress.VolumeName {
		t.Fatalf("pending wipes = %+v, %v; want the agent-less node's cache volume", pending, err)
	}
	if got := f.auditCount("ingress.wipe_queued"); got != 1 {
		t.Errorf("ingress.wipe_queued audit rows = %d, want 1", got)
	}

	// A rerun keeps the one entry and audits it once.
	wantDisableDone(t, f.disable(seesAll))
	if p, _ := f.istore.PendingWipes(ctx, f.envID); len(p) != 1 {
		t.Errorf("pending wipes after a rerun = %+v, want still one", p)
	}
	if got := f.auditCount("ingress.wipe_queued"); got != 1 {
		t.Errorf("ingress.wipe_queued audit rows after a rerun = %d, want 1", got)
	}

	// An agent is installed on that node and connects: the queued wipe finds it by its swarm node ID.
	late := dockertest.NewFake(t, dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{cacheVolume()}})
	cli, err := dockerx.New(late)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	f.eng.byAgent["agent-3"] = cli
	f.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = append(i.Nodes, ingressdeploy.NodeInfo{SwarmNodeID: "node-3", AgentID: "agent-3", Name: "worker-2", Role: "worker", Connected: true})
	})
	f.mux.set(newManualClock())
	f.svc.WipeRound(ctx)
	if has, err := cli.VolumeExists(ctx, ingress.VolumeName); err != nil || has {
		t.Errorf("the agent-less node's volume after its agent connected: exists %v, %v; want it wiped", has, err)
	}
	if n := f.pendingCount(); n != 0 {
		t.Errorf("pending wipes = %d, want the entry cleared", n)
	}
}

// TestDisableQueuesWipesForProxyNodesTheSwarmNoLongerLists: a proxy node that enrolled and has left the swarm (so neither the
// agents nor the swarm's node list name it) still holds the cache volume, and its row is about to be deleted: it is queued
// from the row before the nodes step removes it (WR-04).
func TestDisableQueuesWipesForProxyNodesTheSwarmNoLongerLists(t *testing.T) {
	f := newDisableFix(t, disableOpts{config: func(c *dockertest.FakeConfig) {
		c.NodeRows = []dockertest.FakeNode{{ID: "node-1", Hostname: "manager-1", Role: "manager", Leader: true}}
	}})
	ctx := testsupport.Context(t)
	f.addNodeRow("edge-gone", "node-9", "active")

	job := f.disable(seesAll)
	wantDisableDone(t, job)
	it := itemOf(job, ingressdeploy.ItemVolumes)
	if it.Result["unreachable"] != "edge-gone" || it.Result["pending"] != "1" {
		t.Fatalf("volumes result = %v, want the enrolled node named as not wiped", it.Result)
	}
	pending, err := f.istore.PendingWipes(ctx, f.envID)
	if err != nil || len(pending) != 1 || pending[0].SwarmNodeID != "node-9" {
		t.Fatalf("pending wipes = %+v, %v; want node-9", pending, err)
	}
	if rows, _ := f.tokens.ListNodes(ctx, f.envID); len(rows) != 0 {
		t.Errorf("node rows after the switch-off = %+v, want the active row removed", rows)
	}
}

// TestDisableFailsWhenTheSwarmNodeListCannotBeRead: when the manager's node list cannot be read the nodes to wipe are not
// known, so the volumes step fails rather than finishing with a node silently left out; once the read works the same call
// finishes (WR-04).
func TestDisableFailsWhenTheSwarmNodeListCannotBeRead(t *testing.T) {
	var reads *readFailEngine
	var infos *infoHook
	f := newDisableFix(t, disableOpts{worker: true, config: strayNodes, deps: func(d *ingressdeploy.Deps) {
		reads = &readFailEngine{Engine: d.Engine}
		d.Engine = reads
		infos = &infoHook{Environments: d.Envs}
		d.Envs = infos
	}})
	// The volumes step reads the environment afresh, which is the second read of a switch-off (the first is Disable's own); the
	// cluster reads of the swarm fail from there on.
	infos.at, infos.fn = 2, func() { reads.fail.Store(true) }

	job := f.disable(seesAll)
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done though the node list could not be read: %+v", job.Items)
	}
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeDisableVolumesFailed {
		t.Errorf("volumes = %q %q, want failed with disable_volumes_failed", it.State, it.Code)
	}
	for _, id := range []string{"network", "secret", "token", "nodes"} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped {
			t.Errorf("%s = %q, want skipped", id, it.State)
		}
	}

	reads.fail.Store(false)
	infos.at = 0
	wantDisableDone(t, f.disable(seesAll))
	if p, _ := f.istore.PendingWipes(testsupport.Context(t), f.envID); len(p) != 1 || p[0].SwarmNodeID != "node-3" {
		t.Errorf("pending wipes after the rerun = %+v, want the agent-less node", p)
	}
}

// readFailEngine fails every cluster read while fail is set.
type readFailEngine struct {
	ingressdeploy.Engine
	fail atomic.Bool
}

func (e *readFailEngine) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	if e.fail.Load() {
		return errors.New("the manager did not answer")
	}
	return e.Engine.ClusterRead(ctx, envID, fn)
}

// infoHook runs fn on the at-th read of an environment counted from when at is set, so a test changes the world between two
// steps of a job without a sleep.
type infoHook struct {
	ingressdeploy.Environments
	n  atomic.Int32
	at int32
	fn func()
}

func (h *infoHook) Info(ctx context.Context, envID string) (ingressdeploy.EnvInfo, error) {
	if h.at > 0 && h.n.Add(1) == h.at && h.fn != nil {
		h.fn()
	}
	return h.Environments.Info(ctx, envID)
}
