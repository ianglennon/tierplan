// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// How the reader reads the helpers' logs (D-21).
const (
	// VIPReadInterval is how often one pass reads every node of every environment with a virtual IP. A move shows within
	// about two passes of the helper writing it.
	VIPReadInterval = 2 * time.Second
	// vipMaxLines is the most lines read from one container in one pass; the rest are read by the next pass. A hostile
	// log can therefore make a pass cost no more than this (T-26-21).
	vipMaxLines = 500
	// vipFirstTail is how many of the last lines a container's first read takes, so a restarted server rebuilds the state
	// from a bounded part of the log.
	vipFirstTail = "2000"
	// vipLineBuf is how much of one line is kept: the longest event line plus the engine's timestamp. The rest of a longer
	// line is dropped as it is read.
	vipLineBuf = ingressbundle.VIPLineMax + 64
	// vipMaxBytes is the most one container read takes off the stream, whatever the lines look like.
	vipMaxBytes = 4 << 20
	// vipReadParallel is how many nodes of one environment are read at once.
	vipReadParallel = 8
	// vipNodeReadTimeout is the longest one node's read (list, inspect, logs) may take. An agent that is connected but whose
	// engine accepts a call and never answers would otherwise hold the environment's pass, and so every later pass, for as
	// long as the server runs.
	vipNodeReadTimeout = 8 * time.Second
	// vipPassReadTimeout is the longest the loop lets one environment's pass take: a few batches of nodes at the per-node
	// bound.
	vipPassReadTimeout = 30 * time.Second
)

// errVIPEnough ends a read that has the lines it may take.
var errVIPEnough = errors.New("enough lines")

// vipCursor is how far one container's log has been read: the engine's time of the last line taken, and the helper's
// sequence number of the last event at that time. A line at the cursor's time with a sequence number at or below it is
// the line already taken, read again because the engine's since is inclusive.
type vipCursor struct {
	NodeID  string
	LastSeq uint64
	// LastTime is the engine's time of the last line taken, event or not.
	LastTime time.Time
	// Done is true once a container that had stopped was read to its end, so it is never read again.
	Done bool
}

// vipEnv is the memory of one environment's virtual IP. pass serialises reads of the environment; mu guards the rest.
type vipEnv struct {
	// pass holds one slot, taken for the length of a pass. It is a channel and not a mutex so that a caller with a deadline
	// (a drain's handoff, a switch-off's wait) gives up when its context ends instead of waiting for a stuck pass.
	pass chan struct{}
	mu   sync.Mutex
	// view is the state the events have built; cursors are by container ID.
	view    vipEnvState
	cursors map[string]vipCursor
	// handoff and disabling are the marks (markVIPHandoff, markVIPDisabling).
	handoff   map[string]time.Time
	disabling bool
	// settled is false until a pass has stored what it found: the first pass after a start, and the first after a failed
	// store, store only moves newer than the newest stored one (D-21).
	settled bool
	// skipped counts the lines refused or dropped, never their text.
	skipped uint64
}

// vipState is the reader's memory, by environment.
type vipState struct {
	mu   sync.Mutex
	envs map[string]*vipEnv
}

func newVIPState() *vipState { return &vipState{envs: map[string]*vipEnv{}} }

// env returns the environment's memory, made when there is none.
func (v *vipState) env(envID string) *vipEnv {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.envs[envID]
	if e == nil {
		e = &vipEnv{
			pass:    make(chan struct{}, 1),
			view:    vipEnvState{Nodes: map[string]vipNode{}},
			cursors: map[string]vipCursor{},
			handoff: map[string]time.Time{},
		}
		v.envs[envID] = e
	}
	return e
}

// lockPass takes the environment's pass slot. It reports false, taking nothing, when ctx ends first.
func (e *vipEnv) lockPass(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case e.pass <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// unlockPass gives the slot back.
func (e *vipEnv) unlockPass() { <-e.pass }

// find returns the environment's memory without making any.
func (v *vipState) find(envID string) *vipEnv {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.envs[envID]
}

// keep drops the memory of every environment not in the set.
func (v *vipState) keep(ids map[string]bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for id := range v.envs {
		if !ids[id] {
			delete(v.envs, id)
		}
	}
}

// RunVIPReader reads the keepalived helpers' lines of every environment with a virtual IP, once at the start and then
// every VIPReadInterval of the injected clock, until ctx ends. A pass over one environment never waits for another's
// engine for longer than the pass over it takes, and the loop takes no pass before the last one has ended.
func (s *Service) RunVIPReader(ctx context.Context) {
	for {
		s.vipRound(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.d.Clock.After(VIPReadInterval):
		}
	}
}

// vipRound takes one pass over every environment that has a virtual IP switched on, or is being switched off.
func (s *Service) vipRound(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := s.d.Store.ListVIPs(ctx)
	if err != nil {
		s.log().Warn("the virtual IP reader could not list the environments", "error", err)
		return
	}
	var ids []string
	live := map[string]bool{}
	for _, r := range rows {
		if r.Enabled || s.vipIsDisabling(r.EnvID) {
			ids = append(ids, r.EnvID)
			live[r.EnvID] = true
		}
	}
	s.vip.keep(live)
	var g errgroup.Group
	g.SetLimit(ensureParallel)
	for _, id := range ids {
		g.Go(func() error {
			// One environment's pass never holds the round for longer than this, whatever its engines do.
			pctx, cancel := context.WithTimeout(ctx, s.vipPassTimeout)
			defer cancel()
			s.readVIPOnce(pctx, id)
			return nil
		})
	}
	_ = g.Wait()
	if s.vipHook != nil {
		s.vipHook()
	}
}

// nodeRead is what one node's pass found.
type nodeRead struct {
	node   NodeInfo
	events []vipEvent
	// cursors are the cursors of the containers read, to be kept if the pass is.
	cursors map[string]vipCursor
	// listed are the IDs of every keepalived container of the node; running is true when one of them runs.
	listed  []string
	running bool
	// ok is true when the node's engine answered every call.
	ok      bool
	skipped uint64
}

// readVIPOnce takes one pass over one environment: it reads the new lines of every connected node's keepalived
// containers through that node's agent, applies them, and records each move of the address (D-21, D-23). It follows
// nothing: every read is a plain log read that ends, so no interactive stream slot of the tunnel is held (T-26-22).
// What a pass finds is kept only when its moves are stored, so a failed store is read again.
func (s *Service) readVIPOnce(ctx context.Context, envID string) {
	env := s.vip.env(envID)
	if !env.lockPass(ctx) {
		return
	}
	defer env.unlockPass()
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		s.log().Warn("the virtual IP reader could not read an environment", "environment", envID, "error", err)
		return
	}
	now := s.d.Clock.Now()

	env.mu.Lock()
	cursors := make(map[string]vipCursor, len(env.cursors))
	for id, c := range env.cursors {
		cursors[id] = c
	}
	env.mu.Unlock()

	reads := make([]nodeRead, len(info.Nodes))
	var g errgroup.Group
	g.SetLimit(vipReadParallel)
	for i, n := range info.Nodes {
		reads[i] = nodeRead{node: n}
		// A node whose swarm node ID is not known yet is not read: every line would be judged against an empty ID, refused as
		// another node's, and its cursor moved past it, so the events of the first read would be lost for good.
		if !n.Connected || n.AgentID == "" || n.SwarmNodeID == "" {
			continue
		}
		g.Go(func() error {
			// A node that does not answer in time is read as not heard (it is silent), and the pass goes on without it.
			nctx, cancel := context.WithTimeout(ctx, s.vipNodeTimeout)
			defer cancel()
			reads[i] = s.readVIPNode(nctx, envID, n, cursors)
			return nil
		})
	}
	_ = g.Wait()
	if ctx.Err() != nil {
		return
	}

	var events []vipEvent
	silent := map[string]bool{}
	skipped := uint64(0)
	for _, r := range reads {
		events = append(events, r.events...)
		skipped += r.skipped
		if !r.ok || !r.running {
			silent[r.node.SwarmNodeID] = true
		}
	}

	env.mu.Lock()
	marks := vipMarks{Handoff: map[string]time.Time{}, Disabling: env.disabling, Silent: silent}
	for id, at := range env.handoff {
		if now.Sub(at) > 2*vipHandoffWindow {
			delete(env.handoff, id)
			continue
		}
		marks.Handoff[id] = at
	}
	// A node the state knows that the engine no longer lists is as silent as one that is not connected.
	prev := env.view
	for id := range prev.Nodes {
		if !slices.ContainsFunc(info.Nodes, func(n NodeInfo) bool { return n.SwarmNodeID == id }) {
			silent[id] = true
		}
	}
	settled := env.settled
	env.mu.Unlock()

	next, moves := detectMoves(prev, events, marks)
	// A node that cannot be heard is not a holder, whatever its last line was: a holder killed without a STOP line, or lost
	// with its host, left MASTER as its last word. Its state is kept, so it holds the address again when it is heard.
	for id, n := range next.Nodes {
		n.Unheard = silent[id]
		next.Nodes[id] = n
	}
	for _, r := range reads {
		if !r.ok || len(r.listed) == 0 {
			continue
		}
		n := next.Nodes[r.node.SwarmNodeID]
		n.NodeID = r.node.SwarmNodeID
		if r.node.Name != "" {
			n.Hostname = r.node.Name
		}
		if n.Faults == nil {
			n.Faults = map[string]bool{}
		}
		n.LastHeard = now
		next.Nodes[n.NodeID] = n
	}
	switch {
	case len(holders(next.Nodes)) > 0:
		next.HolderlessSince = time.Time{}
	case next.HolderlessSince.IsZero():
		next.HolderlessSince = now
	}

	if !settled && len(moves) > 0 {
		latest, ok, err := s.d.Store.LatestVIPMove(ctx, envID)
		if err != nil {
			s.log().Warn("the virtual IP reader could not read the newest move", "environment", envID, "error", err)
			return
		}
		if ok {
			moves = slices.DeleteFunc(moves, func(m ingressstore.VIPMove) bool { return !m.At.After(latest.At) })
		}
	}
	for _, m := range moves {
		m.EnvID = envID
		if err := s.d.Store.InsertVIPMove(ctx, m); err != nil {
			s.log().Warn("the virtual IP reader could not store a move", "environment", envID, "error", err)
			env.mu.Lock()
			env.settled = false
			env.mu.Unlock()
			return
		}
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	env.view = next
	env.settled = true
	env.skipped += skipped
	for _, r := range reads {
		if !r.ok {
			continue
		}
		// A container the node no longer lists is gone, and so is its cursor.
		for id, c := range env.cursors {
			if c.NodeID == r.node.SwarmNodeID && !slices.Contains(r.listed, id) {
				delete(env.cursors, id)
			}
		}
	}
	for _, r := range reads {
		for id, c := range r.cursors {
			env.cursors[id] = c
		}
	}
	if skipped > 0 {
		s.log().Debug("the virtual IP reader skipped lines it could not use", "environment", envID, "lines", skipped)
	}
}

// readVIPNode reads one node's keepalived containers through its agent. It never reads the lines of a container for
// another node's ID: a line whose node is not this node's is refused (T-26-21).
func (s *Service) readVIPNode(ctx context.Context, envID string, n NodeInfo, cursors map[string]vipCursor) nodeRead {
	out := nodeRead{node: n, cursors: map[string]vipCursor{}}
	err := s.d.Engine.OnNode(ctx, envID, n.AgentID, func(cli *dockerx.Client) error {
		rows, err := cli.ContainersWithLabel(ctx, ingress.LabelComponent, ingress.ComponentKeepalived)
		if err != nil {
			return err
		}
		out.ok = true
		for _, row := range rows {
			out.listed = append(out.listed, row.ID)
			if row.State == "running" {
				out.running = true
			}
			cur, have := cursors[row.ID]
			if have && cur.Done {
				out.cursors[row.ID] = cur
				continue
			}
			cur.NodeID = n.SwarmNodeID
			if !s.readVIPContainer(ctx, cli, row, have, &cur, &out) {
				out.ok = false
			}
			out.cursors[row.ID] = cur
		}
		return nil
	})
	if err != nil {
		out.ok = false
		s.log().Debug("the virtual IP reader could not read a node", "environment", envID, "node", n.SwarmNodeID, "error", err)
	}
	return out
}

// readVIPContainer reads the new lines of one container into out and advances cur. It reports false when the engine failed
// the read, and true when the read ended, including when it ended at the cap of lines.
func (s *Service) readVIPContainer(ctx context.Context, cli *dockerx.Client, row dockerx.ContainerRow, have bool, cur *vipCursor, out *nodeRead) bool {
	opts := dockerx.LogOptions{Tail: vipFirstTail}
	if have && !cur.LastTime.IsZero() {
		opts = dockerx.LogOptions{Since: logSince(cur.LastTime)}
	}
	rc, tty, err := cli.ContainerLogs(ctx, row.ID, opts)
	if err != nil {
		// A task that went between the list and the read is not a failure of the node; a log the engine cannot read back
		// never will be.
		return dockerx.IsNotFound(err) || errors.Is(err, dockerx.ErrLogsUnreadable)
	}
	defer func() { _ = rc.Close() }()
	sink := &vipLineSink{max: vipMaxLines}
	var copyErr error
	src := &io.LimitedReader{R: rc, N: vipMaxBytes}
	if tty {
		_, copyErr = io.Copy(sink, src)
	} else {
		_, copyErr = stdcopy.StdCopy(sink, sink, src)
	}
	sink.finish()
	hitLimit := src.N <= 0
	capped := errors.Is(copyErr, errVIPEnough)
	failed := copyErr != nil && !capped && !errors.Is(copyErr, io.ErrUnexpectedEOF)
	for _, rl := range sink.lines {
		s.takeVIPLine(rl, cur, out)
	}
	if !failed && !capped && row.State != "running" && row.State != "restarting" && row.State != "created" && !hitLimit && !sink.partial {
		// The container had stopped before the read began, and the read reached its end: nothing more will be written.
		cur.Done = true
	}
	return !failed
}

// takeVIPLine parses one line of a log and, when it is a new event of this node, adds it to out and advances the cursor.
func (s *Service) takeVIPLine(rl vipRawLine, cur *vipCursor, out *nodeRead) {
	at, payload, ok := splitLogStamp(rl.text)
	if !ok {
		out.skipped++
		return
	}
	advance := func() {
		if at.After(cur.LastTime) {
			cur.LastTime, cur.LastSeq = at, 0
		}
	}
	if at.Before(cur.LastTime) {
		return
	}
	if rl.over {
		if at.After(cur.LastTime) {
			out.skipped++
		}
		advance()
		return
	}
	ev, err := ingressbundle.ParseVIPLine(payload)
	if err != nil {
		// A line already counted at this instant is read again by the next pass and is not counted again.
		if !at.Equal(cur.LastTime) {
			out.skipped++
		}
		advance()
		return
	}
	if at.Equal(cur.LastTime) && ev.Seq <= cur.LastSeq {
		return
	}
	if ev.Node != out.node.SwarmNodeID {
		out.skipped++
		advance()
		return
	}
	host := out.node.Name
	if host == "" {
		host = ev.Name
	}
	out.events = append(out.events, vipEvent{At: at, NodeID: ev.Node, Hostname: host, VIPEvent: ev})
	cur.LastTime, cur.LastSeq = at, ev.Seq
}

// logSince is a time in the form the engine's since takes: Unix seconds with nine digits of fraction.
func logSince(t time.Time) string {
	return fmt.Sprintf("%d.%09d", t.Unix(), t.Nanosecond())
}

// splitLogStamp splits a log line into the engine's timestamp and the text after it.
func splitLogStamp(line []byte) (time.Time, []byte, bool) {
	sp := bytes.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, nil, false
	}
	at, err := time.Parse(time.RFC3339Nano, string(line[:sp]))
	if err != nil {
		return time.Time{}, nil, false
	}
	return at.UTC(), line[sp+1:], true
}

// vipRawLine is one line of a log as the sink kept it. over is true when the line was longer than the sink keeps, so
// text holds only its first bytes: enough for the timestamp, which moves the cursor past it, and never an event.
type vipRawLine struct {
	text []byte
	over bool
}

// vipLineSink is the writer a log stream is copied into. It keeps at most max lines and at most vipLineBuf bytes of
// each, so what a log holds cannot make the reader allocate more than that.
type vipLineSink struct {
	max     int
	buf     []byte
	over    bool
	lines   []vipRawLine
	partial bool
}

// Write takes bytes of the stream. It returns errVIPEnough once it holds max lines.
func (w *vipLineSink) Write(p []byte) (int, error) {
	done := 0
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if room := vipLineBuf - len(w.buf); len(chunk) > room {
			w.over = true
			chunk = chunk[:room]
		}
		w.buf = append(w.buf, chunk...)
		if i < 0 {
			return done + len(p), nil
		}
		done += i + 1
		p = p[i+1:]
		w.endLine()
		if len(w.lines) >= w.max {
			return done, errVIPEnough
		}
	}
	return done, nil
}

// endLine closes the line being held.
func (w *vipLineSink) endLine() {
	line := bytes.TrimSuffix(w.buf, []byte("\r"))
	w.lines = append(w.lines, vipRawLine{text: bytes.Clone(line), over: w.over})
	w.buf, w.over = w.buf[:0], false
}

// finish closes a last line that has no line ending. The helper ends every line, so one that does not is cut short (by
// the byte limit, or a stream that broke) and is kept as an over-long one: its timestamp moves the cursor past it, and
// it is never taken for an event.
func (w *vipLineSink) finish() {
	if len(w.buf) == 0 || len(w.lines) >= w.max {
		return
	}
	w.partial = true
	w.over = true
	w.endLine()
}

var _ io.Writer = (*vipLineSink)(nil)

// markVIPHandoff records that the server asked the node, named by its swarm node ID, to hand the address over now, so a
// move away from it in the next seconds is a drain_handoff (D-23).
func (s *Service) markVIPHandoff(envID, nodeID string) {
	e := s.vip.env(envID)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handoff[nodeID] = s.d.Clock.Now()
}

// markVIPDisabling says that the environment's virtual IP is being switched off, or that it no longer is, so a move
// in between is recorded as disabled and the reader keeps reading until the switch-off has ended.
func (s *Service) markVIPDisabling(envID string, on bool) {
	e := s.vip.env(envID)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.disabling = on
}

// vipIsDisabling reports whether a switch-off of the environment's virtual IP is marked as running.
func (s *Service) vipIsDisabling(envID string) bool {
	e := s.vip.find(envID)
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.disabling
}

// vipNodeStates returns what is known of every node's helper in the environment, sorted by host name then ID. The
// result is a copy.
func (s *Service) vipNodeStates(envID string) []vipNode {
	e := s.vip.find(envID)
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]vipNode, 0, len(e.view.Nodes))
	for _, n := range e.view.Nodes {
		n.Faults = cloneFaults(n.Faults)
		if n.Unheard && n.State == ingressbundle.VIPStateMaster {
			// What the status and the bell read: a MASTER that cannot be heard is not one.
			n.State = ""
		}
		out = append(out, n)
	}
	slices.SortFunc(out, compareVIPNodes)
	return out
}

// vipHolderlessSince is when the environment's address last had no holder; the zero time while one holds it and when the
// reader has not read the environment yet.
func (s *Service) vipHolderlessSince(envID string) time.Time {
	e := s.vip.find(envID)
	if e == nil {
		return time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.view.HolderlessSince
}
