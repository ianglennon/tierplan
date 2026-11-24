// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The swarm node IDs of the two nodes of the reader's tests: 25 lower-case alphanumerics, as the helper writes them.
var (
	vipNodeA = strings.Repeat("a", 25)
	vipNodeB = strings.Repeat("b", 25)
)

// vipT0 is the instant the logs of the reader's tests start at.
var vipT0 = time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)

// vipClock is a clock a test sets by hand. After delivers on the channel the test sends to.
type vipClock struct {
	mu    sync.Mutex
	at    time.Time
	ticks chan time.Time
}

func newVIPClock(at time.Time) *vipClock { return &vipClock{at: at, ticks: make(chan time.Time)} }

func (c *vipClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *vipClock) After(time.Duration) <-chan time.Time { return c.ticks }

func (c *vipClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// vipNodeEngine answers OnNode with the fake engine of the node whose agent ID is asked for, so each node has its own
// containers and its own logs. A node it does not know is an agent that is not there.
type vipNodeEngine struct {
	clis map[string]*dockerx.Client
}

var errNoAgent = errors.New("no such agent")

func (e *vipNodeEngine) OnNode(_ context.Context, _, nodeID string, fn func(*dockerx.Client) error) error {
	c, ok := e.clis[nodeID]
	if !ok {
		return errNoAgent
	}
	return fn(c)
}

func (e *vipNodeEngine) WriteOnManager(context.Context, string, func(*dockerx.Client) error) error {
	return errors.New("the reader writes nothing")
}

func (e *vipNodeEngine) WriteOnNode(context.Context, string, string, func(*dockerx.Client) error) error {
	return errors.New("the reader writes nothing")
}

func (e *vipNodeEngine) ClusterRead(context.Context, string, func(*dockerx.Client) error) error {
	return errors.New("the reader reads no cluster")
}

// vipRig is the two-node swarm the reader's tests read.
type vipRig struct {
	*rig
	clock *vipClock
	// a and b are the nodes' fake engines; deps are the dependencies the service was built with.
	a, b *dockertest.Fake
	deps ingressdeploy.Deps
}

// newVIPRig builds an environment with a virtual IP and two connected nodes, each with a running keepalived container
// ("ka-a" and "ka-b") whose log is empty.
func newVIPRig(t *testing.T) *vipRig {
	t.Helper()
	labels := map[string]string{ingress.LabelComponent: ingress.ComponentKeepalived}
	newNode := func(id string) (*dockertest.Fake, *dockerx.Client) {
		f := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{
			ID: id, Names: []string{"/" + id}, State: "running", Labels: labels,
		}}})
		cli, err := dockerx.New(f)
		if err != nil {
			t.Fatalf("dockerx.New: %v", err)
		}
		t.Cleanup(func() { _ = cli.Close() })
		return f, cli
	}
	fa, ca := newNode("ka-a")
	fb, cb := newNode("ka-b")
	v := &vipRig{clock: newVIPClock(vipT0.Add(time.Hour)), a: fa, b: fb}
	v.rig = newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Engine = &vipNodeEngine{clis: map[string]*dockerx.Client{"agent-a": ca, "agent-b": cb}}
		d.Clock = v.clock
		v.deps = *d
	}})
	v.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = []ingressdeploy.NodeInfo{
			{SwarmNodeID: vipNodeA, AgentID: "agent-a", Name: "node-a", Role: "manager", Connected: true},
			{SwarmNodeID: vipNodeB, AgentID: "agent-b", Name: "node-b", Role: "manager", Connected: true},
		}
	})
	err := v.istore.PutVIP(testsupport.Context(t), ingressstore.VIPConfig{
		EnvID: v.envID, Address: netip.MustParseAddr("192.168.10.250"), PrefixLen: 24, RouterID: 51,
		RouterDerived: true, KeySealed: "sealed", KeyGeneration: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("PutVIP: %v", err)
	}
	return v
}

// logLine is one line of a helper's log as the helper writes it.
func logLine(t *testing.T, at time.Time, ev ingressbundle.VIPEvent) dockertest.FakeLogLine {
	t.Helper()
	b, err := ev.MarshalLine()
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	return dockertest.FakeLogLine{Time: at, Text: string(b)}
}

// vipStateEvent is a state event of the node.
func vipStateEvent(node string, seq uint64, state, reason string) ingressbundle.VIPEvent {
	return ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: state, Reason: reason, Node: node, Seq: seq}
}

// read takes one pass over the environment.
func (v *vipRig) read() { v.svc.ReadVIPOnce(testsupport.Context(v.t), v.envID) }

// moves lists the moves stored for the environment, oldest first.
func (v *vipRig) moves() []ingressstore.VIPMove {
	v.t.Helper()
	got, err := v.istore.VIPMovesSince(testsupport.Context(v.t), v.envID, time.Time{})
	if err != nil {
		v.t.Fatalf("VIPMovesSince: %v", err)
	}
	return got
}

// holderNames lists the host names of the nodes that hold the address.
func (v *vipRig) holderNames() []string {
	var out []string
	for _, n := range v.svc.VIPHolders(v.envID) {
		out = append(out, n.Hostname)
	}
	return out
}

// TestVIPReaderRecordsAMove is the tracer of the reader (D-21, D-23): two nodes' keepalived logs are read through the
// nodes' own engines, the holder is known, and a move of the address is stored once, with its reason.
func TestVIPReaderRecordsAMove(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(1*time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateMaster, "")),
	)
	v.b.AppendLogs("ka-b",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeB, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(1*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)

	v.read()
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("holders after the first pass = %v, want [node-a]", got)
	}
	nodes := v.svc.VIPNodeStates(v.envID)
	if len(nodes) != 2 || nodes[0].Hostname != "node-a" || nodes[1].Hostname != "node-b" || nodes[1].State != ingressbundle.VIPStateBackup {
		t.Fatalf("node states = %+v, want node-a MASTER and node-b BACKUP", nodes)
	}
	if got := v.moves(); len(got) != 0 {
		t.Fatalf("the first holder is no move, but %d were stored: %+v", len(got), got)
	}

	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(10*time.Second), vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)))
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(11*time.Second), vipStateEvent(vipNodeB, 3, ingressbundle.VIPStateMaster, "")))
	v.read()
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("holders after the move = %v, want [node-b]", got)
	}
	want := ingressstore.VIPMove{
		EnvID: v.envID, At: vipT0.Add(11 * time.Second), FromNodeID: vipNodeA, FromNodeName: "node-a",
		ToNodeID: vipNodeB, ToNodeName: "node-b", Reason: ingressdeploy.VIPMoveProbeFailing,
	}
	got := v.moves()
	if len(got) != 1 {
		t.Fatalf("%d moves stored, want 1: %+v", len(got), got)
	}
	got[0].ID = 0
	if !got[0].At.Equal(want.At) || got[0].FromNodeID != want.FromNodeID || got[0].FromNodeName != want.FromNodeName ||
		got[0].ToNodeID != want.ToNodeID || got[0].ToNodeName != want.ToNodeName || got[0].Reason != want.Reason || got[0].EnvID != want.EnvID {
		t.Fatalf("stored move = %+v, want %+v", got[0], want)
	}

	// A pass with nothing new reads no line twice and stores nothing more.
	v.read()
	if n := len(v.moves()); n != 1 {
		t.Fatalf("a pass with nothing new left %d moves, want 1", n)
	}
}

// TestVIPReaderRunsOnTheClock pins that RunVIPReader reads once at its start and then on every tick of the injected
// clock, for an environment whose virtual IP is on.
func TestVIPReaderRunsOnTheClock(t *testing.T) {
	v := newVIPRig(t)
	rounds := make(chan struct{}, 4)
	v.svc.SetVIPHook(func() { rounds <- struct{}{} })
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))

	ctx, cancel := context.WithCancel(testsupport.Context(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		v.svc.RunVIPReader(ctx)
	}()
	testsupport.Await(t, "the first round", rounds)
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("holders after the first round = %v, want [node-a]", got)
	}

	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")))
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(2*time.Second), vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateMaster, "")))
	v.clock.ticks <- v.clock.Now()
	testsupport.Await(t, "the second round", rounds)
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("holders after the tick = %v, want [node-b]", got)
	}
	if n := len(v.moves()); n != 1 {
		t.Fatalf("%d moves after the tick, want 1", n)
	}
	cancel()
	testsupport.Await(t, "the reader to stop", done)
}
