// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
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
	labels := map[string]string{ingress.LabelComponent: ingress.ComponentKeepalived, "com.docker.swarm.service.name": ingress.ServiceNameKeepalived}
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

// settle takes the pass that settles what the pass before read: the server's clock has moved on by more than the settle
// window and there is nothing new in the logs.
func (v *vipRig) settle(svc *ingressdeploy.Service) {
	v.clock.set(v.clock.Now().Add(3 * time.Second))
	svc.ReadVIPOnce(testsupport.Context(v.t), v.envID)
}

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
		t.Fatalf("holders after the move = %v, want [node-b]: the holder shows at once, before the move is settled", got)
	}
	v.settle(v.svc)
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

	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(10*time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")))
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(11*time.Second), vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateMaster, "")))
	v.clock.ticks <- v.clock.Now()
	testsupport.Await(t, "the second round", rounds)
	v.clock.set(v.clock.Now().Add(3 * time.Second))
	v.clock.ticks <- v.clock.Now()
	testsupport.Await(t, "the third round", rounds)
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
	t1, t3 := vipT0.Add(10*time.Second), vipT0.Add(30*time.Second)
	v.a.AppendLogs("ka-a",
		logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")),
		// A yields to B and, after B has let go too, takes the address back: a node that stays in MASTER is the one that
		// held it last, so a second MASTER with no BACKUP between would not be a move.
		logLine(t, vipT0.Add(9*time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")),
		logLine(t, t3, vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateMaster, "")),
	)
	v.b.AppendLogs("ka-b",
		logLine(t, t1, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateMaster, "")),
		logLine(t, vipT0.Add(29*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
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
	v.settle(fresh)
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

	v.settle(fresh)
	if n := len(v.moves()); n != 2 {
		t.Fatalf("a second pass left %d moves, want 2", n)
	}

	// A third server with the same store and logs stores nothing either: the last move of the replay is settled by a later pass
	// than the one that read it, and it is still the one that is stored.
	third := ingressdeploy.New(v.deps)
	third.ReadVIPOnce(testsupport.Context(t), v.envID)
	v.settle(third)
	v.settle(third)
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

// setKeepalivedState changes the state the node's fake engine reports for its keepalived container, as a container that was
// killed or removed from a node: the log it wrote stays.
func (v *vipRig) setKeepalivedState(f *dockertest.Fake, id, state string) {
	v.t.Helper()
	labels := map[string]string{ingress.LabelComponent: ingress.ComponentKeepalived, "com.docker.swarm.service.name": ingress.ServiceNameKeepalived}
	f.Reconfigure(dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{ID: id, Names: []string{"/" + id}, State: state, Labels: labels}}})
}

// noHolderBell says whether the header bell would raise vip_no_holder for the environment at the instant.
func (v *vipRig) noHolderBell(svc *ingressdeploy.Service, now time.Time) bool {
	v.t.Helper()
	f, ok, err := svc.VIPFacts(testsupport.Context(v.t), v.envID)
	if err != nil || !ok {
		v.t.Fatalf("VIPFacts = %v, %v", ok, err)
	}
	for _, w := range envs.VIPWarnings(v.envID, "prod", f, nil, now) {
		if w.ID == envs.WarnVIPNoHolder {
			return true
		}
	}
	return false
}

// TestVIPReaderDoesNotCountAHolderWhoseKeepalivedIsGone pins WR-04: a holder whose last line is MASTER, on a node whose engine
// answered this pass and runs no keepalived container (killed without a STOP line, the task removed), is not the holder, so the
// status says none and the clock of the no-holder bell starts. Its last state is kept, so it holds the address again as soon as
// a running container is read.
func TestVIPReaderDoesNotCountAHolderWhoseKeepalivedIsGone(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	// The other node's proxy is down, so nobody can take the address over.
	v.b.AppendLogs("ka-b", logLine(t, vipT0, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)))
	v.read()
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("holders after the first pass = %v, want [node-a]", got)
	}
	if since := v.svc.VIPHolderlessSince(v.envID); !since.IsZero() {
		t.Fatalf("the address is held, but it is holderless since %s", since)
	}

	lost := v.clock.Now().Add(time.Minute)
	v.clock.set(lost)
	v.setKeepalivedState(v.a, "ka-a", "exited")
	v.read()
	if got := v.holderNames(); len(got) != 0 {
		t.Fatalf("holders after the holder's container was gone = %v, want none", got)
	}
	if since := v.svc.VIPHolderlessSince(v.envID); !since.Equal(lost) {
		t.Errorf("holderless since %s, want %s: the no-holder bell is timed from the pass that lost the holder", since, lost)
	}
	for _, n := range v.svc.VIPNodeStates(v.envID) {
		if n.Hostname == "node-a" && n.State == ingressbundle.VIPStateMaster {
			t.Errorf("node-a is shown as MASTER while no keepalived runs there")
		}
	}
	if n := len(v.moves()); n != 0 {
		t.Errorf("%d moves stored, want none: nobody took the address", n)
	}
	if v.noHolderBell(v.svc, lost.Add(5*time.Second)) {
		t.Error("the no-holder bell is raised 5 s after the holder was lost, want it after more than 10 s")
	}
	if !v.noHolderBell(v.svc, lost.Add(11*time.Second)) {
		t.Error("the no-holder bell is not raised 11 s after the only holder was lost")
	}

	// A running container is read again with nothing new in its log: the node still holds the address.
	v.clock.set(lost.Add(time.Minute))
	v.setKeepalivedState(v.a, "ka-a", "running")
	v.read()
	if got := v.holderNames(); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("holders after the container ran again = %v, want [node-a]", got)
	}
	if since := v.svc.VIPHolderlessSince(v.envID); !since.IsZero() {
		t.Errorf("the address is held again, but it is holderless since %s", since)
	}
	if v.noHolderBell(v.svc, lost.Add(2*time.Minute)) {
		t.Error("the no-holder bell is raised while the address is held")
	}
}

// TestVIPReaderKeepsAHolderItCannotAsk pins the other half of WR-04: not being able to ask is not evidence that nobody holds
// the address. A holder whose agent is offline, whose node ID is not known, or whose read times out stays the holder, the
// no-holder clock never starts and no bell is raised, however long that lasts.
func TestVIPReaderKeepsAHolderItCannotAsk(t *testing.T) {
	setup := func(t *testing.T) (*vipRig, *ingressdeploy.Service, *vipStallEngine) {
		t.Helper()
		v := newVIPRig(t)
		v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
		v.b.AppendLogs("ka-b", logLine(t, vipT0, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)))
		eng := &vipStallEngine{Engine: v.deps.Engine, stall: map[string]bool{}, entered: make(chan string, 8)}
		d := v.deps
		d.Engine = eng
		svc := ingressdeploy.New(d)
		svc.SetVIPReadTimeouts(100*time.Millisecond, time.Hour) // wall-clock: the bound a stalled engine is cut at; a correct pass ends no later, and a wrong one never ends
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		if h := svc.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-a" {
			t.Fatalf("holders after the first pass = %v, want [node-a]", h)
		}
		return v, svc, eng
	}
	wantHeld := func(t *testing.T, v *vipRig, svc *ingressdeploy.Service, why string) {
		t.Helper()
		if h := svc.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-a" {
			t.Fatalf("holders %s = %v, want [node-a] still", why, h)
		}
		if since := svc.VIPHolderlessSince(v.envID); !since.IsZero() {
			t.Errorf("%s: holderless since %s, want the address held", why, since)
		}
		if v.noHolderBell(svc, v.clock.Now().Add(time.Hour)) {
			t.Errorf("%s: the no-holder bell is raised for an address nobody was shown to have lost", why)
		}
	}

	t.Run("the agent is offline", func(t *testing.T) {
		v, svc, _ := setup(t)
		v.clock.set(v.clock.Now().Add(time.Minute))
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		wantHeld(t, v, svc, "with the agent offline")
		v.clock.set(v.clock.Now().Add(time.Hour))
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		wantHeld(t, v, svc, "an hour with the agent offline")
	})

	t.Run("the read times out", func(t *testing.T) {
		v, svc, eng := setup(t)
		eng.stall["agent-a"] = true
		v.clock.set(v.clock.Now().Add(time.Minute))
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		wantHeld(t, v, svc, "with the holder's read timed out")
	})

	t.Run("the node ID is not known", func(t *testing.T) {
		v, svc, _ := setup(t)
		v.clock.set(v.clock.Now().Add(time.Minute))
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].SwarmNodeID = "" })
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		wantHeld(t, v, svc, "with the holder's node ID unknown")
	})

	t.Run("the agent is gone from the list", func(t *testing.T) {
		v, svc, _ := setup(t)
		v.clock.set(v.clock.Now().Add(time.Minute))
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes = i.Nodes[1:] })
		svc.ReadVIPOnce(testsupport.Context(t), v.envID)
		wantHeld(t, v, svc, "with the holder's agent no longer listed")
	})
}

// TestVIPReaderHolderThatFellSilentIsReplacedAsFailed pins that the demotion leaves the reason of a move alone: a silent
// MASTER replaced by another node's MASTER in the same pass is still a holder_failed move.
func TestVIPReaderHolderThatFellSilentIsReplacedAsFailed(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	v.b.AppendLogs("ka-b", logLine(t, vipT0, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateBackup, "")))
	v.read()

	v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(5*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateMaster, "")))
	v.read()
	v.settle(v.svc)
	got := v.moves()
	if len(got) != 1 || got[0].Reason != ingressdeploy.VIPMoveHolderFailed || got[0].ToNodeID != vipNodeB {
		t.Fatalf("moves = %+v, want one holder_failed move to node-b", got)
	}
	if h := v.holderNames(); len(h) != 1 || h[0] != "node-b" {
		t.Fatalf("holders = %v, want [node-b]", h)
	}
}

// TestVIPReaderReadsOnlyTasksOfTheService pins IN-03: a container that carries only the component label is not read, since
// anyone with Docker access on the node can set it, and it could write lines for the node's own ID.
func TestVIPReaderReadsOnlyTasksOfTheService(t *testing.T) {
	v := newVIPRig(t)
	forged := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{
		ID: "forged", Names: []string{"/forged"}, State: "running",
		Labels: map[string]string{ingress.LabelComponent: ingress.ComponentKeepalived},
	}}})
	cli, err := dockerx.New(forged)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	forged.AppendLogs("forged", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	d := v.deps
	d.Engine = &vipNodeEngine{clis: map[string]*dockerx.Client{"agent-a": cli, "agent-b": cli}}
	svc := ingressdeploy.New(d)

	svc.ReadVIPOnce(testsupport.Context(t), v.envID)
	if n := len(forged.LogQueries()); n != 0 {
		t.Errorf("the log of a container with only the component label was read %d times, want 0", n)
	}
	for _, n := range svc.VIPNodeStates(v.envID) {
		if n.State != "" {
			t.Errorf("node %s has state %q from a forged container's log", n.Hostname, n.State)
		}
	}
}

// TestVIPReaderLongReplayStoresNothingTwice pins IN-06: a log with more lines than one pass takes (500) is replayed over
// several passes after a restart, and the moves the later passes rebuild from lines older than the newest stored move are
// not stored again.
func TestVIPReaderLongReplayStoresNothingTwice(t *testing.T) {
	v := newVIPRig(t)
	t1, t2 := vipT0.Add(10*time.Second), vipT0.Add(20*time.Second)
	// node-a's log is longer than a pass reads: it holds the address, yields after 3 s, and after 600 lines of filler takes the
	// address back at t2. node-b holds it in between, from t1 to 18 s.
	lines := []dockertest.FakeLogLine{
		logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")),
		logLine(t, vipT0.Add(3*time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateBackup, "")),
	}
	for i := range 600 {
		lines = append(lines, logLine(t, vipT0.Add(3*time.Second+time.Duration(i+1)*time.Millisecond), ingressbundle.VIPEvent{
			Event: ingressbundle.VIPEventClockSkew, Node: vipNodeA, Seq: uint64(i + 3), Active: i%2 == 0,
		}))
	}
	lines = append(lines, logLine(t, t2, vipStateEvent(vipNodeA, 603, ingressbundle.VIPStateMaster, "")))
	v.a.AppendLogs("ka-a", lines...)
	v.b.AppendLogs("ka-b",
		logLine(t, t1, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateMaster, "")),
		logLine(t, vipT0.Add(18*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)
	for _, m := range []ingressstore.VIPMove{
		{EnvID: v.envID, At: t1, FromNodeID: vipNodeA, FromNodeName: "node-a", ToNodeID: vipNodeB, ToNodeName: "node-b", Reason: ingressdeploy.VIPMoveUnknown},
		{EnvID: v.envID, At: t2, FromNodeID: vipNodeB, FromNodeName: "node-b", ToNodeID: vipNodeA, ToNodeName: "node-a", Reason: ingressdeploy.VIPMoveUnknown},
	} {
		if err := v.istore.InsertVIPMove(testsupport.Context(t), m); err != nil {
			t.Fatalf("InsertVIPMove: %v", err)
		}
	}

	fresh := ingressdeploy.New(v.deps)
	for pass := 1; pass <= 4; pass++ {
		v.settle(fresh)
		if n := len(v.moves()); n != 2 {
			t.Fatalf("%d moves stored after replay pass %d, want the 2 that were stored before the restart: %+v", n, pass, v.moves())
		}
	}
	if h := fresh.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-a" {
		t.Fatalf("holders after the replay = %v, want [node-a]", h)
	}
}

// TestVIPReaderOneFailoverIsOneMove plays the two faults of the LAN dry run of 2026-11-25 through the logs: the holder's
// container is killed, its replacement starts before the reader looks, and the move is one holder_failed row; and a claim that
// loses its election in a later pass than the one that read it stores nothing.
func TestVIPReaderOneFailoverIsOneMove(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a", logLine(t, vipT0, vipStateEvent(vipNodeA, 1, ingressbundle.VIPStateMaster, "")))
	v.b.AppendLogs("ka-b", logLine(t, vipT0, vipStateEvent(vipNodeB, 1, ingressbundle.VIPStateBackup, "")))
	v.read()
	v.settle(v.svc)

	// The lost claim of an election.
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(10*time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateMaster, "")))
	v.read()
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(10500*time.Millisecond), vipStateEvent(vipNodeB, 3, ingressbundle.VIPStateBackup, "")))
	v.settle(v.svc)
	v.settle(v.svc)
	if n := len(v.moves()); n != 0 {
		t.Fatalf("a claim that lost stored %+v, want no move", v.moves())
	}

	v.a.AppendLogs("ka-a", logLine(t, vipT0.Add(22*time.Second), ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}))
	v.b.AppendLogs("ka-b", logLine(t, vipT0.Add(23*time.Second), vipStateEvent(vipNodeB, 4, ingressbundle.VIPStateMaster, "")))
	v.read()
	v.settle(v.svc)
	got := v.moves()
	if len(got) != 1 || got[0].Reason != ingressdeploy.VIPMoveHolderFailed || got[0].FromNodeID != vipNodeA || got[0].ToNodeID != vipNodeB {
		t.Fatalf("moves = %+v, want one holder_failed move from node-a to node-b", got)
	}
}

// TestVIPStatusShowsAddressInUse pins the status half of D-11: the last fault of a node held out by another machine's answer
// is address_in_use, although the FAULT line that follows says probe_failing.
func TestVIPStatusShowsAddressInUse(t *testing.T) {
	v := newVIPRig(t)
	v.a.AppendLogs("ka-a",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: ingressbundle.VIPReasonAddressInUse, Active: true, Node: vipNodeA, Seq: 2}),
		logLine(t, vipT0.Add(2*time.Second), vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateFault, ingressbundle.VIPReasonProbeFailing)),
	)
	v.read()
	st, err := v.svc.VIPStatus(testsupport.Context(t), v.envID)
	if err != nil {
		t.Fatalf("VIPStatus: %v", err)
	}
	for _, c := range st.Candidates {
		if c.NodeID == vipNodeA {
			if c.LastFault != ingressbundle.VIPReasonAddressInUse || c.VRRPState != ingressbundle.VIPStateFault {
				t.Fatalf("node-a = %+v, want FAULT with the last fault address_in_use", c)
			}
			return
		}
	}
	t.Fatalf("node-a is no candidate: %+v", st.Candidates)
}

// TestVIPReaderRestartWithAgentsConnectingOneByOne pins the restart rule as it happens in service, found on the LAN on
// 2026-11-25: after a restart no agent is connected and then each connects in its own time. The log of one node alone tells a
// history that did not happen, so nothing is stored until every node has been read, and then nothing is stored twice and a
// move made while the server was down is stored once.
func TestVIPReaderRestartWithAgentsConnectingOneByOne(t *testing.T) {
	const late = 120 * time.Second
	logs := func(t *testing.T, v *vipRig) (t1, t2, t3 time.Time) {
		t.Helper()
		at := func(s int) time.Time { return vipT0.Add(time.Duration(s) * time.Second) }
		m, b := ingressbundle.VIPStateMaster, ingressbundle.VIPStateBackup
		f := ingressbundle.VIPStateFault
		p := ingressbundle.VIPReasonProbeFailing
		// A holds, A to B at 11, B to A at 31 (both stored before the restart), A to B at 51, while the server was down.
		v.a.AppendLogs("ka-a", logLine(t, at(0), vipStateEvent(vipNodeA, 1, m, "")),
			logLine(t, at(10), vipStateEvent(vipNodeA, 2, f, p)), logLine(t, at(20), vipStateEvent(vipNodeA, 3, b, "")),
			logLine(t, at(31), vipStateEvent(vipNodeA, 4, m, "")), logLine(t, at(50), vipStateEvent(vipNodeA, 5, f, p)))
		v.b.AppendLogs("ka-b", logLine(t, at(0), vipStateEvent(vipNodeB, 1, b, "")),
			logLine(t, at(11), vipStateEvent(vipNodeB, 2, m, "")), logLine(t, at(30), vipStateEvent(vipNodeB, 3, f, p)),
			logLine(t, at(40), vipStateEvent(vipNodeB, 4, b, "")), logLine(t, at(51), vipStateEvent(vipNodeB, 5, m, "")))
		for _, mv := range []ingressstore.VIPMove{
			{EnvID: v.envID, At: at(11), FromNodeID: vipNodeA, FromNodeName: "node-a", ToNodeID: vipNodeB, ToNodeName: "node-b", Reason: ingressdeploy.VIPMoveProbeFailing},
			{EnvID: v.envID, At: at(31), FromNodeID: vipNodeB, FromNodeName: "node-b", ToNodeID: vipNodeA, ToNodeName: "node-a", Reason: ingressdeploy.VIPMoveProbeFailing},
		} {
			if err := v.istore.InsertVIPMove(testsupport.Context(t), mv); err != nil {
				t.Fatalf("InsertVIPMove: %v", err)
			}
		}
		return at(11), at(31), at(51)
	}
	connect := func(v *vipRig, a, b bool) {
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected, i.Nodes[1].Connected = a, b })
	}
	for _, aFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("a first %v", aFirst), func(t *testing.T) {
			v := newVIPRig(t)
			_, _, t3 := logs(t, v)
			fresh := ingressdeploy.New(v.deps)
			connect(v, false, false)
			v.settle(fresh)
			v.settle(fresh)
			connect(v, aFirst, !aFirst)
			v.settle(fresh)
			v.settle(fresh)
			if n := len(v.moves()); n != 2 {
				t.Fatalf("%d moves with one node read, want the 2 that were stored: %+v", n, v.moves())
			}
			connect(v, true, true)
			for range 3 {
				v.settle(fresh)
			}
			got := v.moves()
			if len(got) != 3 || !got[2].At.Equal(t3) || got[2].FromNodeID != vipNodeA || got[2].ToNodeID != vipNodeB || got[2].Reason != ingressdeploy.VIPMoveProbeFailing {
				t.Fatalf("moves = %+v, want the 2 stored and A to B at %s, probe_failing", got, t3)
			}
			if h := fresh.VIPHolders(v.envID); len(h) != 1 || h[0].Hostname != "node-b" {
				t.Fatalf("holders = %v, want [node-b]", h)
			}
		})
	}

	t.Run("a node that stays offline does not hold the history back for ever, and tells nothing old when it is back", func(t *testing.T) {
		v := newVIPRig(t)
		logs(t, v)
		fresh := ingressdeploy.New(v.deps)
		connect(v, false, true)
		v.settle(fresh)
		v.clock.set(v.clock.Now().Add(late))
		v.settle(fresh)
		v.settle(fresh)
		before := len(v.moves())
		// B's log alone shows B taking the address after the newest stored move; who lost it cannot be known, so that is one row
		// at most, and the node's own old history adds none.
		if before > 3 {
			t.Fatalf("%d moves stored from one node's log, want at most the one that is newer: %+v", before, v.moves())
		}
		connect(v, true, true)
		for range 3 {
			v.settle(fresh)
		}
		if n := len(v.moves()); n != before {
			t.Fatalf("%d moves after the late node was read, want %d: %+v", n, before, v.moves())
		}
	})
}
