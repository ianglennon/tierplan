// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// stepClock is a clock whose After moves it on by the wait and answers at once, so a handoff that waits ten seconds of its
// clock takes no time.
type stepClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.at
	return ch
}

// handoffEngine is the engine of the two nodes with writes: a write runs on the node's own engine, and afterStop, when set,
// runs once a write on a node has succeeded, which is where a test makes the other node take the address.
type handoffEngine struct {
	vipNodeEngine
	mu        sync.Mutex
	afterStop func(agent string)
	written   []string
}

func (e *handoffEngine) WriteOnNode(_ context.Context, _, agent string, fn func(*dockerx.Client) error) error {
	c, ok := e.clis[agent]
	if !ok {
		return errNoAgent
	}
	if err := fn(c); err != nil {
		return err
	}
	e.mu.Lock()
	e.written = append(e.written, agent)
	hook := e.afterStop
	e.mu.Unlock()
	if hook != nil {
		hook(agent)
	}
	return nil
}

func (e *handoffEngine) writes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.written)
}

// handoffRig is the two-node swarm of the handoff's tests: node A holds the address and node B is a BACKUP with a ready proxy.
type handoffRig struct {
	*rig
	clock  *stepClock
	a, b   *dockertest.Fake
	engine *handoffEngine
	nodes  *statusNodes
}

// newHandoffRig builds the rig. aWriteError, when set, is what node A's engine answers to every write.
func newHandoffRig(t *testing.T, aWriteError string) *handoffRig {
	t.Helper()
	labels := map[string]string{ingress.LabelComponent: ingress.ComponentKeepalived, "com.docker.swarm.service.name": ingress.ServiceNameKeepalived}
	newNode := func(id, writeError string) (*dockertest.Fake, *dockerx.Client) {
		f := dockertest.NewFake(t, dockertest.FakeConfig{WriteError: writeError, Containers: []dockertest.FakeContainer{{
			ID: id, Names: []string{"/" + id}, State: "running", Labels: labels,
		}}})
		cli, err := dockerx.New(f)
		if err != nil {
			t.Fatalf("dockerx.New: %v", err)
		}
		t.Cleanup(func() { _ = cli.Close() })
		return f, cli
	}
	fa, ca := newNode("ka-a", aWriteError)
	fb, cb := newNode("ka-b", "")
	h := &handoffRig{clock: &stepClock{at: vipT0.Add(time.Hour)}, a: fa, b: fb, nodes: &statusNodes{}}
	h.engine = &handoffEngine{vipNodeEngine: vipNodeEngine{clis: map[string]*dockerx.Client{"agent-a": ca, "agent-b": cb}}}
	h.rig = newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Engine = h.engine
		d.Clock = h.clock
		d.Nodes = h.nodes
		d.CurrentBundle = func(string) (string, bool) { return statusBundle, true }
	}})
	h.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = []ingressdeploy.NodeInfo{
			{SwarmNodeID: vipNodeA, AgentID: "agent-a", Name: "node-a", Role: "manager", Connected: true},
			{SwarmNodeID: vipNodeB, AgentID: "agent-b", Name: "node-b", Role: "manager", Connected: true},
		}
	})
	err := h.istore.PutVIP(testsupport.Context(t), ingressstore.VIPConfig{
		EnvID: h.envID, Address: netip.MustParseAddr("192.168.10.250"), PrefixLen: 24, RouterID: 51,
		RouterDerived: true, KeySealed: "sealed", KeyGeneration: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("PutVIP: %v", err)
	}
	h.nodes.rows = []ingresshub.Node{readyProxy(vipNodeA), readyProxy(vipNodeB)}
	// The reader learns who holds the address the way it does in service, from the helpers' logs.
	fa.AppendLogs("ka-a",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateMaster, "")),
	)
	fb.AppendLogs("ka-b",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeB, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)
	h.svc.ReadVIPOnce(testsupport.Context(t), h.envID)
	if got := h.svc.VIPHolders(h.envID); len(got) != 1 || got[0].NodeID != vipNodeA {
		t.Fatalf("holders before the handoff = %+v, want node A", got)
	}
	return h
}

// bTakesOver makes node B report MASTER as soon as node A's keepalived has been stopped.
func (h *handoffRig) bTakesOver() {
	h.engine.afterStop = func(agent string) {
		if agent != "agent-a" {
			return
		}
		h.a.AppendLogs("ka-a", logLine(h.t, h.clock.Now(), vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateStop, "")))
		h.b.AppendLogs("ka-b", logLine(h.t, h.clock.Now(), vipStateEvent(vipNodeB, 3, ingressbundle.VIPStateMaster, "")))
	}
}

// moves lists the moves stored for the environment, oldest first.
func (h *handoffRig) moves() []ingressstore.VIPMove {
	h.t.Helper()
	got, err := h.istore.VIPMovesSince(testsupport.Context(h.t), h.envID, time.Time{})
	if err != nil {
		h.t.Fatalf("VIPMovesSince: %v", err)
	}
	return got
}

func (h *handoffRig) handoff() (*ingressdeploy.VIPHandoff, error) {
	h.t.Helper()
	return h.svc.HandoffVIP(testsupport.Context(h.t), h.envID, vipNodeA)
}

// TestVIPHandoffBranches pins every outcome of the drain's handoff (D-25, PROH-26-04): the address moves when a peer takes it,
// and each way it cannot is an outcome that leaves the drain to go on, never an error, within the bound.
func TestVIPHandoffBranches(t *testing.T) {
	t.Run("a ready peer takes the address", func(t *testing.T) {
		h := newHandoffRig(t, "")
		h.bTakesOver()
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffMoved || got.Holder != "node-b" || got.HolderNodeID != vipNodeB {
			t.Fatalf("handoff = %+v, %v; want moved to node-b", got, err)
		}
		if w := h.a.Writes(); len(w) != 1 || w[0] != "POST /containers/ka-a/stop" {
			t.Errorf("node A writes = %v, want one stop of its keepalived", w)
		}
		if w := h.b.Writes(); len(w) != 0 {
			t.Errorf("node B writes = %v, want none", w)
		}
		if moves := h.moves(); len(moves) != 1 || moves[0].Reason != "drain_handoff" {
			t.Errorf("moves = %+v, want one drain_handoff: the handoff marks the node before it stops it", moves)
		}
	})

	t.Run("a holder with no ready peer is no_candidate and nothing is stopped", func(t *testing.T) {
		h := newHandoffRig(t, "")
		h.nodes.rows = []ingresshub.Node{readyProxy(vipNodeA)}
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffNoCandidate || got.Holder != "" {
			t.Fatalf("handoff = %+v, %v; want no_candidate", got, err)
		}
		if w := h.a.Writes(); len(w) != 0 {
			t.Errorf("node A writes = %v, want none", w)
		}
	})

	t.Run("an agent that is not connected is agent_unreachable", func(t *testing.T) {
		h := newHandoffRig(t, "")
		h.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffAgentUnreachable {
			t.Fatalf("handoff = %+v, %v; want agent_unreachable", got, err)
		}
		if w := h.a.Writes(); len(w) != 0 {
			t.Errorf("node A writes = %v, want none", w)
		}
	})

	t.Run("an agent the engine cannot reach is agent_unreachable", func(t *testing.T) {
		h := newHandoffRig(t, "")
		delete(h.engine.clis, "agent-a")
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffAgentUnreachable {
			t.Fatalf("handoff = %+v, %v; want agent_unreachable", got, err)
		}
	})

	t.Run("a stop that fails is agent_unreachable", func(t *testing.T) {
		h := newHandoffRig(t, "the engine refused")
		h.bTakesOver()
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffAgentUnreachable {
			t.Fatalf("handoff = %+v, %v; want agent_unreachable", got, err)
		}
		if w := h.engine.writes(); len(w) != 0 {
			t.Errorf("completed writes = %v, want none", w)
		}
	})

	t.Run("no new holder within the bound is timeout", func(t *testing.T) {
		h := newHandoffRig(t, "")
		start := h.clock.Now()
		got, err := h.handoff()
		if err != nil || got == nil || got.Outcome != ingressdeploy.VIPHandoffTimeout || got.Holder != "" {
			t.Fatalf("handoff = %+v, %v; want timeout", got, err)
		}
		if w := h.a.Writes(); len(w) != 1 {
			t.Errorf("node A writes = %v, want the stop that was sent", w)
		}
		if waited := h.clock.Now().Sub(start); waited < ingressdeploy.VIPHandoffBound || waited > ingressdeploy.VIPHandoffBound+time.Second {
			t.Errorf("waited %s of the clock, want the bound %s and no more", waited, ingressdeploy.VIPHandoffBound)
		}
	})

	t.Run("a node that does not hold the address is no handoff", func(t *testing.T) {
		h := newHandoffRig(t, "")
		got, err := h.svc.HandoffVIP(testsupport.Context(t), h.envID, vipNodeB)
		if err != nil || got != nil {
			t.Fatalf("handoff of the BACKUP = %+v, %v; want nil, nil", got, err)
		}
		if w := h.b.Writes(); len(w) != 0 {
			t.Errorf("node B writes = %v, want none", w)
		}
	})

	t.Run("no virtual IP is nil", func(t *testing.T) {
		h := newHandoffRig(t, "")
		if _, err := h.istore.DeleteVIP(testsupport.Context(t), h.envID); err != nil {
			t.Fatalf("DeleteVIP: %v", err)
		}
		got, err := h.handoff()
		if err != nil || got != nil {
			t.Fatalf("handoff without a virtual IP = %+v, %v; want nil, nil", got, err)
		}
		if w := h.a.Writes(); len(w) != 0 {
			t.Errorf("node A writes = %v, want none", w)
		}
	})

	t.Run("a store that cannot tell is the one error, and nothing is stopped", func(t *testing.T) {
		h := newHandoffRig(t, "")
		h.nodes.err = errors.New("the proxy table is unreadable")
		got, err := h.handoff()
		if err == nil || got != nil {
			t.Fatalf("handoff = %+v, %v; want an error and no handoff", got, err)
		}
		if w := h.a.Writes(); len(w) != 0 {
			t.Errorf("node A writes = %v, want none", w)
		}
	})
}
