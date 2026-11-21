// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// awayFix is a swarm of a manager and a worker whose agent was disconnected when ingress was switched off, so the worker's cache
// volume is a pending wipe, with the wipe loop's clock now a manual one.
func awayFix(t *testing.T) (*disableFix, *manualClock) {
	t.Helper()
	f := newDisableFix(t, disableOpts{worker: true, workerAway: true})
	wantDisableDone(t, f.disable(seesAll))
	if p, err := f.istore.PendingWipes(testsupport.Context(t), f.envID); err != nil || len(p) != 1 {
		t.Fatalf("pending wipes = %+v, %v; want the worker's", p, err)
	}
	if !hasVolume(t, f.worker) {
		t.Fatal("the worker's volume was wiped while its agent was away")
	}
	clock := newManualClock()
	f.mux.set(clock)
	return f, clock
}

// reconnect says the worker's agent is connected.
func (f *disableFix) reconnect() {
	f.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[1].Connected = true })
}

// pendingCount is how many wipes are queued for the environment.
func (f *disableFix) pendingCount() int {
	f.t.Helper()
	p, err := f.istore.PendingWipes(testsupport.Context(f.t), f.envID)
	if err != nil {
		f.t.Fatalf("PendingWipes: %v", err)
	}
	return len(p)
}

// runWipes starts the wipe loop and returns the channel that gets a value after each of its passes, and a stop that ends the
// loop and waits for it.
func (f *disableFix) runWipes() (passes <-chan struct{}, stop func()) {
	f.t.Helper()
	ch := make(chan struct{}, 16)
	f.svc.SetWipeHook(func() { ch <- struct{}{} })
	ctx, cancel := context.WithCancel(testsupport.Context(f.t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.RunWipes(ctx)
	}()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-done
	}
	f.t.Cleanup(stop)
	return ch, stop
}

// TestPendingWipeOnReconnect: an agent.connected event for the node makes RunWipes remove the volume once no ingress
// container remains, clear the entry and record ingress.wipe_done; the loop's tick does the same without an event.
func TestPendingWipeOnReconnect(t *testing.T) {
	t.Run("an agent.connected event", func(t *testing.T) {
		f, clock := awayFix(t)
		passes, stop := f.runWipes()
		testsupport.Await(t, "the first pass", passes)
		testsupport.Await(t, "the timer", clock.armed)
		if !hasVolume(t, f.worker) || f.pendingCount() != 1 {
			t.Fatal("the first pass, with the agent still away, wiped something")
		}

		f.reconnect()
		f.events <- envs.HealthEvent{Type: envs.EventAgentConnected, EnvID: f.envID, Data: envs.AgentConnectedData{AgentID: workerNode.AgentID, Node: workerNode.Name, Type: "worker"}}
		testsupport.Await(t, "the pass the event woke", passes)
		stop()

		if hasVolume(t, f.worker) {
			t.Error("the worker's cache volume is still there after its agent connected")
		}
		if n := f.pendingCount(); n != 0 {
			t.Errorf("pending wipes = %d, want the entry cleared", n)
		}
		if got := f.auditCount("ingress.wipe_done"); got != 1 {
			t.Errorf("ingress.wipe_done audit rows = %d, want 1", got)
		}
		if got := f.eng.removeCalls(workerNode.AgentID); got != 1 {
			t.Errorf("removals sent to the worker = %d, want exactly 1", got)
		}
	})

	t.Run("the tick", func(t *testing.T) {
		f, clock := awayFix(t)
		passes, _ := f.runWipes()
		testsupport.Await(t, "the first pass", passes)
		testsupport.Await(t, "the timer", clock.armed)
		f.reconnect()
		clock.fire()
		testsupport.Await(t, "the pass the tick took", passes)

		if hasVolume(t, f.worker) || f.pendingCount() != 0 {
			t.Error("the tick did not wipe the worker's cache volume")
		}
		if got := f.auditCount("ingress.wipe_done"); got != 1 {
			t.Errorf("ingress.wipe_done audit rows = %d, want 1", got)
		}
	})

	t.Run("a node that never came back stays queued and unreported", func(t *testing.T) {
		f, _ := awayFix(t)
		f.svc.WipeRound(testsupport.Context(t))
		if !hasVolume(t, f.worker) || f.pendingCount() != 1 || f.auditCount("ingress.wipe_done") != 0 {
			t.Error("a pass over a node whose agent is away changed something")
		}
	})
}

// TestNoWipeOfALiveProxy: a node that runs an ingress task container keeps its volume and its entry stays pending; an
// environment whose ingress is on again has its pending wipes dropped with no removal; a paused environment's wipes do not
// run, and neither do those of an environment another operation holds.
func TestNoWipeOfALiveProxy(t *testing.T) {
	ctx := testsupport.Context

	t.Run("a proxy container is on the node", func(t *testing.T) {
		f, _ := awayFix(t)
		f.reconnect()
		proxy := dockertest.FakeContainer{
			ID: "c-proxy", Names: []string{"/tierplan-ingress.node-2.abc"}, State: "running",
			Labels: ingress.ObjectLabels(), Mounts: []string{ingress.VolumeName},
		}
		workerCfg := dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{cacheVolume()}, Containers: []dockertest.FakeContainer{proxy}}
		f.workerFake.Reconfigure(workerCfg)

		f.svc.WipeRound(ctx(t))
		if !hasVolume(t, f.worker) {
			t.Fatal("the cache volume of a node that runs the proxy was wiped")
		}
		if n := f.pendingCount(); n != 1 {
			t.Errorf("pending wipes = %d, want the entry kept", n)
		}
		if got := f.eng.removeCalls(workerNode.AgentID); got != 0 {
			t.Errorf("%d removals were sent to a node that runs the proxy", got)
		}
		if got := f.auditCount("ingress.wipe_done"); got != 0 {
			t.Errorf("ingress.wipe_done audit rows = %d, want none", got)
		}

		// A stopped container of the proxy that still mounts the volume holds it too.
		proxy.State = "exited"
		workerCfg.Containers = []dockertest.FakeContainer{proxy}
		f.workerFake.Reconfigure(workerCfg)
		f.svc.WipeRound(ctx(t))
		if !hasVolume(t, f.worker) || f.eng.removeCalls(workerNode.AgentID) != 0 {
			t.Error("the volume of a node that still has a proxy container was wiped")
		}

		// Once the container is gone the next pass wipes it.
		f.workerFake.Reconfigure(dockertest.FakeConfig{Volumes: []dockertest.FakeVolume{cacheVolume()}})
		f.svc.WipeRound(ctx(t))
		if hasVolume(t, f.worker) || f.pendingCount() != 0 {
			t.Error("the volume was not wiped once the proxy container was gone")
		}
	})

	t.Run("ingress is on again", func(t *testing.T) {
		f, _ := awayFix(t)
		f.reconnect()
		if _, err := f.istore.SetEnabled(ctx(t), f.envID, true, "u-admin", nil); err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}
		f.svc.WipeRound(ctx(t))
		if !hasVolume(t, f.worker) {
			t.Fatal("the volume of an environment whose ingress is on again was wiped")
		}
		if n := f.pendingCount(); n != 0 {
			t.Errorf("pending wipes = %d, want them dropped", n)
		}
		if got := f.eng.removeCalls(workerNode.AgentID); got != 0 {
			t.Errorf("%d removals were sent to an environment whose ingress is on", got)
		}
		if got := f.auditCount("ingress.wipe_done"); got != 0 {
			t.Errorf("ingress.wipe_done audit rows = %d, want none for a dropped entry", got)
		}
	})

	t.Run("paused and busy environments", func(t *testing.T) {
		f, _ := awayFix(t)
		f.reconnect()
		if _, err := f.istore.SetPaused(ctx(t), f.envID, true, "system:restore", nil); err != nil {
			t.Fatalf("SetPaused: %v", err)
		}
		f.svc.WipeRound(ctx(t))
		if !hasVolume(t, f.worker) || f.pendingCount() != 1 || f.eng.removeCalls(workerNode.AgentID) != 0 {
			t.Fatal("a paused environment's wipe ran")
		}
		if _, err := f.istore.SetPaused(ctx(t), f.envID, false, "u-admin", nil); err != nil {
			t.Fatalf("SetPaused: %v", err)
		}

		release, _, ok := f.locks.TryAcquire(f.envID, stackops.Holder{Kind: stackops.HolderKindIngress, By: "test"})
		if !ok {
			t.Fatal("the operation lock was not free")
		}
		f.svc.WipeRound(ctx(t))
		if !hasVolume(t, f.worker) || f.pendingCount() != 1 {
			t.Fatal("a wipe ran while another operation held the environment")
		}
		release()

		f.svc.WipeRound(ctx(t))
		if hasVolume(t, f.worker) || f.pendingCount() != 0 {
			t.Error("the wipe did not run once the environment was resumed and free")
		}
	})
}
