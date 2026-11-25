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

// nodeState returns the reader's state of the node with the host name.
func (v *vipRig) nodeState(host string) (st struct {
	LastEvent, LastHeard time.Time
	State                string
}, ok bool) {
	for _, n := range v.svc.VIPNodeStates(v.envID) {
		if n.Hostname == host {
			st.LastEvent, st.LastHeard, st.State = n.LastEvent, n.LastHeard, n.State
			return st, true
		}
	}
	return st, false
}

// TestVIPReaderRestartStoresNothingTwice pins the restart rule (D-21): a server that starts with no memory reads every
// log from its start, rebuilds every node's state, and stores only the moves newer than the newest one the store holds.
func TestVIPReaderRestartStoresNothingTwice(t *testing.T) {
	v := newVIPRig(t)
	t1, t3 := vipT0.Add(1*time.Second), vipT0.Add(3*time.Second)
	v.a.AppendLogs("ka-a",
		logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")),
		// A yields to B and, after B has let go too, takes the address back: a node that stays in MASTER is the one that
		// held it last, so a second MASTER with no BACKUP between would not be a move.
		logLine(t, vipT0.Add(1500*time.Millisecond), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")),
		logLine(t, t3, vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateMaster, "")),
	)
	v.b.AppendLogs("ka-b",
		logLine(t, t1, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateMaster, "")),
		logLine(t, vipT0.Add(2*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)
	// The move of t1 is stored already, by the server that ran before the restart.
	err := v.istore.InsertVIPMove(testsupport.Context(t), ingressstore.VIPMove{
		EnvID: v.envID, At: t1, FromNodeID: vipNodeA, FromNodeName: "node-a", ToNodeID: vipNodeB, ToNodeName: "node-b",
		Reason: ingressdeploy.VIPMoveUnknown,
	})
	if err != nil {
		t.Fatalf("InsertVIPMove: %v", err)
	}

	fresh := ingressdeploy.New(v.deps)
	fresh.ReadVIPOnce(testsupport.Context(t), v.envID)
	got := v.moves()
	if len(got) != 2 {
		t.Fatalf("%d moves after the first pass of a fresh server, want 2 (the stored one and the later one): %+v", len(got), got)
	}
	if !got[0].At.Equal(t1) || !got[1].At.Equal(t3) || got[1].FromNodeID != vipNodeB || got[1].ToNodeID != vipNodeA {
		t.Fatalf("moves = %+v, want the one at t1 and then B to A at t3", got)
	}
	if h := fresh.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-a" {
		t.Fatalf("holders after the restart = %v, want [node-a]", h)
	}

	fresh.ReadVIPOnce(testsupport.Context(t), v.envID)
	if n := len(v.moves()); n != 2 {
		t.Fatalf("a second pass left %d moves, want 2", n)
	}

	// A third server with the same store and logs stores nothing either.
	third := ingressdeploy.New(v.deps)
	third.ReadVIPOnce(testsupport.Context(t), v.envID)
	if n := len(v.moves()); n != 2 {
		t.Fatalf("a second restart left %d moves, want 2", n)
	}
}

// TestVIPReaderBounds pins what a hostile or busy log can cost (Pitfall 15, T-26-21): at most 500 lines of a container
// are read in a pass, and a line that is not an event, is too long, or names another node is counted and dropped.
func TestVIPReaderBounds(t *testing.T) {
	v := newVIPRig(t)
	at := func(i int) time.Time { return vipT0.Add(time.Duration(i) * time.Millisecond) }
	const total = 800
	lines := make([]dockertest.FakeLogLine, 0, total)
	for i := range total {
		switch i {
		case 9:
			lines = append(lines, dockertest.FakeLogLine{Time: at(i), Text: "this is not an event"})
		case 19:
			lines = append(lines, dockertest.FakeLogLine{Time: at(i), Text: strings.Repeat("x", 3000)})
		case 29:
			lines = append(lines, logLine(t, at(i), ingressbundle.VIPEvent{Event: ingressbundle.VIPEventClockSkew, Node: vipNodeB, Seq: uint64(i + 1)}))
		default:
			lines = append(lines, logLine(t, at(i), ingressbundle.VIPEvent{
				Event: ingressbundle.VIPEventClockSkew, Node: vipNodeA, Seq: uint64(i + 1), Active: i%2 == 0,
			}))
		}
	}
	v.a.AppendLogs("ka-a", lines...)
	t1 := v.clock.Now()

	v.read()
	a, ok := v.nodeState("node-a")
	if !ok {
		t.Fatal("the reader knows nothing of node-a after the first pass")
	}
	if !a.LastEvent.Equal(at(499)) {
		t.Fatalf("the last event read after the first pass is at %s, want the 500th line at %s", a.LastEvent, at(499))
	}
	if n := v.svc.VIPSkipped(v.envID); n != 3 {
		t.Fatalf("%d lines skipped after the first pass, want 3 (not an event, too long, another node's)", n)
	}

	// The node that cannot be reached is not read and keeps the time it was last heard.
	t2 := t1.Add(time.Minute)
	v.clock.set(t2)
	v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[1].Connected = false })
	before := len(v.b.LogQueries())
	v.read()
	a, _ = v.nodeState("node-a")
	if !a.LastEvent.Equal(at(799)) {
		t.Fatalf("the last event read after the second pass is at %s, want the 800th line at %s", a.LastEvent, at(799))
	}
	if !a.LastHeard.Equal(t2) {
		t.Errorf("node-a was last heard at %s, want %s", a.LastHeard, t2)
	}
	b, ok := v.nodeState("node-b")
	if !ok || !b.LastHeard.Equal(t1) {
		t.Errorf("node-b was last heard at %s (known %v), want %s: a node that is not connected keeps its time", b.LastHeard, ok, t1)
	}
	if got := len(v.b.LogQueries()); got != before {
		t.Errorf("node-b's log was read %d times while its agent was not connected", got-before)
	}
	if n := v.svc.VIPSkipped(v.envID); n != 3 {
		t.Errorf("%d lines skipped after the second pass, want still 3: nothing is read twice", n)
	}
}

// TestVIPReaderFollowsNoStream pins T-26-22: the reader asks for plain reads that end, never a follow, so no
// interactive stream slot of the tunnel is held.
func TestVIPReaderFollowsNoStream(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	v.b.AppendLogs("ka-b", logLine(t, vipT0, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateBackup, "")))
	v.read()
	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")))
	v.read()

	for name, f := range map[string]*dockertest.Fake{"node-a": v.a, "node-b": v.b} {
		qs := f.LogQueries()
		if len(qs) == 0 {
			t.Fatalf("%s's log was never read", name)
		}
		for _, q := range qs {
			if q.Get("follow") == "1" {
				t.Errorf("%s: a log request followed the stream: %v", name, q)
			}
		}
		if n := f.LogFollows(); n != 0 {
			t.Errorf("%s: %d follows are open", name, n)
		}
	}
	// The second read of node-a starts after the first's last line, not at the start of its log.
	last := v.a.LogQueries()[len(v.a.LogQueries())-1]
	if last.Get("since") == "" || last.Get("tail") != "" && last.Get("tail") != "all" {
		t.Errorf("the second read of node-a asked %v, want a since and no tail", last)
	}
}

// TestVIPReaderWaitsForTheNodeID pins WR-03: an agent that is connected before it has reported its swarm node ID is not
// read, because every line would be refused as another node's and the cursor moved past it. Once the ID is known the node's
// whole log is read, including the MASTER line written before.
func TestVIPReaderWaitsForTheNodeID(t *testing.T) {
	v := newVIPRig(t)
	v.b.AppendLogs("ka-b",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeB, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateMaster, "")),
	)
	v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[1].SwarmNodeID = "" })

	v.read()
	if n := len(v.b.LogQueries()); n != 0 {
		t.Fatalf("node-b's log was read %d times before its node ID was known, want 0", n)
	}
	if n := v.svc.VIPSkipped(v.envID); n != 0 {
		t.Fatalf("%d lines were refused before the node ID was known, want 0", n)
	}

	v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[1].SwarmNodeID = vipNodeB })
	v.read()
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-b" {
		t.Fatalf("holders once the node ID is known = %v, want [node-b]: the MASTER line written before must still be read", got)
	}
}
