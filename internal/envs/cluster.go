// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Timing of the cluster reader (RESEARCH A12). They are tuning, kept in one place.
const (
	// DefaultClusterInterval is how often the cluster of each environment is read
	// in the background.
	DefaultClusterInterval = 15 * time.Second
	// DefaultClusterTimeout bounds one background read.
	DefaultClusterTimeout = 5 * time.Second
	// DefaultClusterTTL is how long State serves what the last read said.
	DefaultClusterTTL = 5 * time.Second

	// failureWindow is how long a manager's tunnel failure counts against it when
	// a manager is chosen.
	failureWindow = 5 * time.Minute
	// refreshParallel bounds the environments read at once.
	refreshParallel = 8
	// minEngineMajor is the oldest Docker Engine Bayplan manages (D-47).
	minEngineMajor = 24
	// auditTimeout bounds the write of a failover's audit row.
	auditTimeout = 5 * time.Second

	// agentHubActor is the principal under which what the hub notices is audited
	// (D-13); the cluster reader is part of how the hub keeps an environment going.
	agentHubActor = "system:agent-hub"

	// eventTypeBayplan is the type of a feed row that is Bayplan's own.
	eventTypeBayplan = "bayplan"
)

// ErrNoManager is the error of a cluster read of an environment that has no
// manager agent connected, or none whose engine is ready to be asked. It maps to
// the reason not_a_manager (D-44).
var ErrNoManager = errors.New("envs: no manager agent is connected")

// NodeObserver is told each node list a manager answered with, and which node
// answered: the agents service's reaper is one (D-10).
type NodeObserver interface {
	Observe(ctx context.Context, envID string, list dockerx.ClusterRead, answeringNodeID string)
}

// HealthPublisher is where a failover is announced and who is told that an
// environment's health may have changed: the health broker.
type HealthPublisher interface {
	Publish(ev HealthEvent) HealthEvent
	Notify(envID string)
}

// ClusterOptions configure Clusters. A zero field takes its default.
type ClusterOptions struct {
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Publisher announces failover and asks for a recompute; nil announces nothing.
	Publisher HealthPublisher
	// Events is where a failover is also written as a row of the environment's
	// feed; nil writes none.
	Events *Hub
	// Reaper is shown every successful node list of a swarm; nil shows it none.
	Reaper NodeObserver
	// Grace is how long after an update a service is not counted as failing; it
	// defaults to DefaultFailingServiceGrace.
	Grace time.Duration
	// Interval, Timeout and TTL default to the Default* constants above.
	Interval, Timeout, TTL time.Duration
	// Log receives what cannot be returned; it defaults to the service's.
	Log *slog.Logger
}

// ClusterState is what Clusters last learned about one environment. It is a copy:
// changing it changes nothing.
type ClusterState struct {
	// Read is the last successful read of the cluster, zero before the first.
	Read dockerx.ClusterRead
	// FetchedAt is when Read was taken; AttemptedAt is when a read was last tried,
	// whatever came of it.
	FetchedAt, AttemptedAt time.Time
	// LeaderNodeID is the swarm node ID of the leader in Read, and LeaderAgentID is
	// the agent on that node, empty when the leader has no agent enrolled.
	LeaderNodeID, LeaderAgentID string
	// InUse is the agent of the manager the last read was sent to.
	InUse string
	// NoQuorum is true while the managers answer that the swarm has no leader;
	// NoQuorumSince is when that began.
	NoQuorum      bool
	NoQuorumSince time.Time
	// Failing counts failing services of a swarm or failing containers of a
	// standalone host, nil until worked out or while the quorum is lost.
	Failing *FailingCounts
}

// Known is true when a node list is held and the quorum is not lost.
func (s ClusterState) Known() bool { return !s.FetchedAt.IsZero() && !s.NoQuorum }

// awayManager is a manager that stopped answering and was replaced.
type awayManager struct {
	node  string
	since time.Time
}

// clusterEntry is everything held for one environment.
type clusterEntry struct {
	// refresh makes one read of the environment at a time, so a burst of callers
	// shares one.
	refresh sync.Mutex

	mu    sync.Mutex
	state ClusterState
	// failures are the times a manager's tunnel failed under a read, by agent.
	failures map[string][]time.Time
	// away are the managers a read failed over from that are not back yet.
	away     map[string]awayManager
	policies *PolicyCache
}

// Clusters reads the cluster of swarm environments: from which manager, with what
// retry, and what it all means for the environment's health. Everything it does to
// Docker is a read, and its exported methods have no mutating variant: a read that
// loses its tunnel may be repeated once on another manager, which is safe only
// because it changes nothing (D-42). It is safe for concurrent use.
type Clusters struct {
	svc    *Service
	snaps  *Snapshots
	pub    HealthPublisher
	reaper NodeObserver
	events *Hub
	now    func() time.Time
	log    *slog.Logger

	grace, interval, timeout, ttl time.Duration

	mu      sync.Mutex
	entries map[string]*clusterEntry
}

// NewClusters returns the cluster reader of svc. snaps is read for the containers
// of a standalone host.
func NewClusters(svc *Service, snaps *Snapshots, o ClusterOptions) *Clusters {
	c := &Clusters{
		svc: svc, snaps: snaps, pub: o.Publisher, reaper: o.Reaper, events: o.Events, now: o.Now, log: o.Log,
		grace: o.Grace, interval: o.Interval, timeout: o.Timeout, ttl: o.TTL,
		entries: map[string]*clusterEntry{},
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = svc.log
	}
	if c.grace <= 0 {
		c.grace = DefaultFailingServiceGrace
	}
	if c.interval <= 0 {
		c.interval = DefaultClusterInterval
	}
	if c.timeout <= 0 {
		c.timeout = DefaultClusterTimeout
	}
	if c.ttl <= 0 {
		c.ttl = DefaultClusterTTL
	}
	return c
}

func (c *Clusters) entry(envID string) *clusterEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[envID]
	if e == nil {
		e = &clusterEntry{failures: map[string][]time.Time{}, away: map[string]awayManager{}, policies: NewPolicyCache()}
		c.entries[envID] = e
	}
	return e
}

func (c *Clusters) lookup(envID string) (*clusterEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[envID]
	return e, ok
}

// Forget drops what is known of a removed environment.
func (c *Clusters) Forget(envID string) {
	c.mu.Lock()
	delete(c.entries, envID)
	c.mu.Unlock()
	if f, ok := c.reaper.(interface{ Forget(envID string) }); ok {
		f.Forget(envID)
	}
}

// Cached is the state as last read, with no I/O. It reports false for an
// environment nothing was ever read for.
func (c *Clusters) Cached(envID string) (ClusterState, bool) {
	e, ok := c.lookup(envID)
	if !ok {
		return ClusterState{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, true
}

// LeaderAgent is the ID of the agent on the leading manager as of the last read,
// or empty. It does no I/O.
func (c *Clusters) LeaderAgent(envID string) string {
	st, _ := c.Cached(envID)
	return st.LeaderAgentID
}

// candidates are the connected managers a read may go to, best first: the leader
// first, then those
// the node list does not call unreachable, then the one with the fewest recent
// tunnel failures, then the lowest agent ID so that the choice is stable.
func (c *Clusters) candidates(in RollupInput, e *clusterEntry, needClient bool) []AgentView {
	var st ClusterState
	var failures map[string]int
	if e != nil {
		e.mu.Lock()
		st = e.state
		failures = e.recentFailures(c.now())
		e.mu.Unlock()
	}
	reach := map[string]string{}
	for _, n := range st.Read.Nodes {
		reach[n.ID] = n.Reachability
	}
	var out []AgentView
	for _, a := range connectedAgents(in) {
		if a.Role != roleManager {
			continue
		}
		if needClient {
			if _, _, err := c.svc.mon.Client(AgentTarget(a.ID)); err != nil {
				continue
			}
		}
		out = append(out, a)
	}
	leader := func(a AgentView) bool { return a.SwarmNodeID != "" && a.SwarmNodeID == st.LeaderNodeID }
	unreachable := func(a AgentView) bool { return reach[a.SwarmNodeID] == "unreachable" }
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if la, lb := leader(a), leader(b); la != lb {
			return la
		}
		if ua, ub := unreachable(a), unreachable(b); ua != ub {
			return !ua
		}
		if fa, fb := failures[a.ID], failures[b.ID]; fa != fb {
			return fa < fb
		}
		return a.ID < b.ID
	})
	return out
}

// recentFailures counts each agent's tunnel failures inside failureWindow and
// forgets the older ones. The caller holds e.mu.
func (e *clusterEntry) recentFailures(now time.Time) map[string]int {
	out := make(map[string]int, len(e.failures))
	for id, ts := range e.failures {
		kept := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < failureWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(e.failures, id)
			continue
		}
		e.failures[id] = kept
		out[id] = len(kept)
	}
	return out
}

// ClusterRead runs a read of the cluster on one manager of the environment and
// returns its error. The manager is the leader when it is connected, else one the
// node list does not call unreachable, else the one with the fewest recent tunnel
// failures. When the read fails because the tunnel to that manager is gone, and for
// no other reason, it is run once more on another manager, and the caller sees
// only the second answer: that the first manager stopped answering is said once in
// the environment's feed, on the health stream and in the audit log (D-43).
//
// There is no variant that changes anything, and fn must not either: the retry is
// safe only for a read (D-42). ErrNoManager means no manager agent is connected.
func (c *Clusters) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	in, err := c.svc.Input(ctx, envID, c.now())
	if err != nil {
		return err
	}
	_, err = c.read(ctx, in, fn)
	return err
}

// read is ClusterRead over an input already taken, and also says which manager
// answered.
func (c *Clusters) read(ctx context.Context, in RollupInput, fn func(*dockerx.Client) error) (AgentView, error) {
	if in.Kind != KindAgent || in.EnvType != envTypeSwarm {
		return AgentView{}, fmt.Errorf("envs: %s is not a swarm environment reached through agents", in.EnvID)
	}
	e := c.entry(in.EnvID)
	cands := c.candidates(in, e, true)
	if len(cands) == 0 {
		for _, a := range connectedAgents(in) {
			if a.Role == roleManager {
				return AgentView{}, fmt.Errorf("%w: no manager's engine has answered yet", ErrOffline)
			}
		}
		return AgentView{}, ErrNoManager
	}
	first := cands[0]
	c.noteChoice(ctx, in, e, first)

	err := c.run(first, fn)
	if err == nil {
		return first, nil
	}
	if !errors.Is(err, tunnel.ErrAgentGone) {
		return first, err
	}
	// The tunnel to that manager is gone: a read is safe to repeat, once.
	c.noteFailure(e, first.ID)
	if len(cands) < 2 {
		return first, err
	}
	next := cands[1]
	if err := c.run(next, fn); err != nil {
		if errors.Is(err, tunnel.ErrAgentGone) {
			c.noteFailure(e, next.ID)
		}
		return next, err
	}
	c.failover(ctx, in, e, first, next)
	return next, nil
}

// run calls fn with the Docker client of one manager's tunnel.
func (c *Clusters) run(a AgentView, fn func(*dockerx.Client) error) error {
	cli, _, err := c.svc.mon.Client(AgentTarget(a.ID))
	if err != nil {
		return fmt.Errorf("manager %s: %w", nodeName(a), err)
	}
	return fn(cli)
}

func (c *Clusters) noteFailure(e *clusterEntry, agentID string) {
	e.mu.Lock()
	e.failures[agentID] = append(e.failures[agentID], c.now())
	e.mu.Unlock()
}

// noteChoice records the manager a read is about to go to. When the manager in use
// before it is gone, because its agent disconnected between reads, that is a
// failover as much as one found under a read.
func (c *Clusters) noteChoice(ctx context.Context, in RollupInput, e *clusterEntry, chosen AgentView) {
	e.mu.Lock()
	prev := e.state.InUse
	e.mu.Unlock()
	if prev == "" || prev == chosen.ID {
		e.mu.Lock()
		e.state.InUse = chosen.ID
		e.mu.Unlock()
		return
	}
	for _, a := range in.Agents {
		if a.ID == prev && !a.Revoked && !a.Connected {
			c.failover(ctx, in, e, a, chosen)
			return
		}
	}
	// The one in use is fine, or gone for good: the leader moved, nothing to say.
	e.mu.Lock()
	e.state.InUse = chosen.ID
	e.mu.Unlock()
}

// failover says that from stopped answering and to took over, once however many
// reads noticed: the first to find the manager in use still pointing at from does
// it under the entry's lock, and the others find it already moved.
func (c *Clusters) failover(ctx context.Context, in RollupInput, e *clusterEntry, from, to AgentView) {
	now := c.now()
	e.mu.Lock()
	if e.state.InUse != "" && e.state.InUse != from.ID {
		e.mu.Unlock()
		return
	}
	e.state.InUse = to.ID
	e.away[from.ID] = awayManager{node: nodeName(from), since: now}
	e.mu.Unlock()

	fromNode, toNode := nodeName(from), nodeName(to)
	msg := fmt.Sprintf("Manager %s unreachable, using %s", fromNode, toNode)
	c.log.Warn("a manager stopped answering; using another", "environment", in.EnvID, "from", from.ID, "to", to.ID)

	if c.pub != nil {
		c.pub.Publish(HealthEvent{Type: EventFailover, EnvID: in.EnvID, At: now, Data: FailoverData{
			FromAgentID: from.ID, FromNode: fromNode, ToAgentID: to.ID, ToNode: toNode,
		}})
		c.pub.Notify(in.EnvID)
	}
	if c.events != nil {
		c.events.Inject(AgentTarget(to.ID), dockerx.EventRow{
			ID:   fmt.Sprintf("bayplan-%d-%s-%s", now.UnixNano(), from.ID, to.ID),
			Time: now.UTC(), TimeNano: now.UnixNano(),
			Type: eventTypeBayplan, Action: "failover", ActorID: in.EnvID, Name: msg,
			Attributes: map[string]string{"from": fromNode, "to": toNode},
		})
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := c.svc.aud.Record(actx, audit.Entry{
		Actor:  audit.Actor{Type: audit.ActorSystem, ID: agentHubActor, Name: agentHubActor},
		Action: audit.ActionEnvironmentFailover, TargetType: targetEnvironment, TargetID: in.EnvID, EnvID: in.EnvID,
		Result:  audit.ResultSuccess,
		Details: map[string]string{"from": fromNode, "to": toNode},
	}); err != nil {
		c.log.Error("audit a manager failover", "environment", in.EnvID, "error", err)
	}
}

// State is the cluster state of an environment, read again when what it holds is
// older than the TTL. Reads of one environment share a single call.
func (c *Clusters) State(ctx context.Context, envID string) (ClusterState, error) {
	e, ok := c.lookup(envID)
	if !ok {
		if !c.svc.Known(ctx, envID) {
			return ClusterState{}, ErrNotFound
		}
		e = c.entry(envID)
	}
	fresh := func() (ClusterState, bool) {
		e.mu.Lock()
		defer e.mu.Unlock()
		st := e.state
		return st, !st.AttemptedAt.IsZero() && c.now().Sub(st.AttemptedAt) < c.ttl
	}
	if st, ok := fresh(); ok {
		return st, nil
	}
	e.refresh.Lock()
	defer e.refresh.Unlock()
	if st, ok := fresh(); ok {
		return st, nil
	}
	err := c.refresh(ctx, envID, e)
	st, _ := c.Cached(envID)
	return st, err
}

// Refresh reads the environment's cluster now. It returns what the read returned:
// an error that maps to no_quorum or not_a_manager still records that in the state.
func (c *Clusters) Refresh(ctx context.Context, envID string) error {
	e := c.entry(envID)
	e.refresh.Lock()
	defer e.refresh.Unlock()
	return c.refresh(ctx, envID, e)
}

// refresh is the read of one environment, by what it is. The caller holds
// e.refresh.
func (c *Clusters) refresh(ctx context.Context, envID string, e *clusterEntry) error {
	in, err := c.svc.Input(ctx, envID, c.now())
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	switch {
	case in.Kind == KindLocal:
		return c.refreshLocal(rctx, in, e)
	case in.EnvType == envTypeSwarm:
		return c.refreshSwarm(rctx, in, e)
	default:
		return c.refreshContainers(rctx, in, e)
	}
}

func (c *Clusters) refreshSwarm(ctx context.Context, in RollupInput, e *clusterEntry) error {
	var read dockerx.ClusterRead
	used, err := c.read(ctx, in, func(cli *dockerx.Client) error {
		r, err := cli.Cluster(ctx)
		read = r
		return err
	})
	c.apply(in, e, read, err)
	if err == nil && c.reaper != nil {
		// Each successful node list is what an agent's node is missing from or not
		// (D-10). The read's own deadline is nearly spent, and the writes that may
		// follow are not part of it.
		octx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
		defer cancel()
		c.reaper.Observe(octx, in.EnvID, read, used.SwarmNodeID)
	}
	return err
}

// refreshLocal reads the Docker socket of the server's own host: a swarm manager's
// cluster, or a standalone engine's containers. A worker has nothing cluster-wide.
func (c *Clusters) refreshLocal(ctx context.Context, in RollupInput, e *clusterEntry) error {
	switch in.Local.Probe.Type {
	case dockerx.TypeSwarmManager:
		cli, _, err := c.svc.mon.Client(in.EnvID)
		if err != nil {
			return err
		}
		read, err := cli.Cluster(ctx)
		c.apply(in, e, read, err)
		return err
	case dockerx.TypeStandalone:
		return c.refreshContainers(ctx, in, e)
	}
	return nil
}

// apply records the outcome of a read of the cluster.
func (c *Clusters) apply(in RollupInput, e *clusterEntry, read dockerx.ClusterRead, err error) {
	now := c.now()
	changed := false
	e.mu.Lock()
	e.state.AttemptedAt = now
	switch {
	case err == nil:
		changed = e.state.NoQuorum
		e.state.Read, e.state.FetchedAt = read, now
		e.state.NoQuorum, e.state.NoQuorumSince = false, time.Time{}
		e.state.LeaderNodeID, e.state.LeaderAgentID = "", ""
		for _, n := range read.Nodes {
			if n.Leader {
				e.state.LeaderNodeID = n.ID
			}
		}
		for _, a := range in.Agents {
			if !a.Revoked && a.SwarmNodeID != "" && a.SwarmNodeID == e.state.LeaderNodeID {
				e.state.LeaderAgentID = a.ID
			}
		}
		failing, total := FailingServices(read.Services, now, c.grace)
		e.state.Failing = &FailingCounts{Failing: failing, Total: total}
	case ReasonFor(err) == ReasonNoQuorum:
		if !e.state.NoQuorum {
			e.state.NoQuorum, e.state.NoQuorumSince = true, now
			changed = true
		}
		e.state.Failing = nil
	}
	e.mu.Unlock()
	if changed && c.pub != nil {
		c.pub.Notify(in.EnvID)
	}
}

// refreshContainers counts the failing containers of a standalone engine: the
// local socket's, or the one agent of a standalone host.
func (c *Clusters) refreshContainers(ctx context.Context, in RollupInput, e *clusterEntry) error {
	key, ok := c.svc.primary(in)
	if !ok {
		return nil
	}
	snap, _, err := c.snaps.Get(ctx, key)
	if err != nil {
		return err
	}
	cli, _, err := c.svc.mon.Client(key)
	if err != nil {
		return err
	}
	failing, total := FailingContainers(ctx, snap.Containers, e.policies, func(ctx context.Context, ids []string) (map[string]dockerx.RestartPolicy, error) {
		return cli.RestartPolicies(ctx, ids)
	})
	e.mu.Lock()
	e.state.AttemptedAt = c.now()
	e.state.Failing = &FailingCounts{Failing: failing, Total: total}
	e.mu.Unlock()
	return nil
}

// RefreshAll reads every enabled environment, a few at a time, each bounded by the
// timeout. One that cannot be read is skipped; what it cost is in its own state.
func (c *Clusters) RefreshAll(ctx context.Context) {
	rows, err := c.svc.st.RQ.ListEnvironments(ctx)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("list environments to read their clusters", "error", err)
		}
		return
	}
	keep := make(map[string]struct{}, len(rows))
	var wg sync.WaitGroup
	sem := make(chan struct{}, refreshParallel)
	for _, row := range rows {
		keep[row.ID] = struct{}{}
		if row.Enabled == 0 {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := c.Refresh(ctx, row.ID); err != nil && ctx.Err() == nil {
				c.log.Debug("read an environment's cluster", "environment", row.ID, "error", err)
			}
		}()
	}
	wg.Wait()
	c.mu.Lock()
	for id := range c.entries {
		if _, ok := keep[id]; !ok {
			delete(c.entries, id)
		}
	}
	c.mu.Unlock()
}

// Run reads every environment now and then every Interval until ctx ends.
func (c *Clusters) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.RefreshAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Manager is the monitor key a one-engine read of the environment goes to: for a
// swarm, the manager a cluster read would choose, so the dashboard follows the
// leader too; for anything else, Primary.
func (c *Clusters) Manager(in RollupInput) (string, bool) {
	if in.Kind != KindAgent || in.EnvType != envTypeSwarm {
		return Primary(in)
	}
	e, _ := c.lookup(in.EnvID)
	if cands := c.candidates(in, e, false); len(cands) > 0 {
		return AgentTarget(cands[0].ID), true
	}
	return Primary(in)
}

// View is the cluster view of an environment's rollup input, worked out from the
// last read with no I/O; it is nil for an environment nothing is known of. It also
// forgets a manager that has come back.
func (c *Clusters) View(in RollupInput) *ClusterView {
	e, ok := c.lookup(in.EnvID)
	var st ClusterState
	var away []NodeSince
	if ok {
		e.mu.Lock()
		st = e.state
		for id, m := range e.away {
			back := false
			listed := false
			for _, a := range in.Agents {
				if a.ID == id {
					listed = true
					back = a.Connected
				}
			}
			if back || !listed {
				delete(e.away, id)
				continue
			}
			away = append(away, NodeSince{Node: m.node, Since: m.since})
		}
		e.mu.Unlock()
	}

	v := &ClusterView{Failing: st.Failing}
	switch {
	case in.Kind == KindLocal:
		// The server's own socket: a swarm manager's nodes, or a standalone engine's
		// failing containers. Nothing is known before the first read.
		if !ok {
			return nil
		}
		if v.Known = st.Known(); v.Known {
			fillFromNodes(v, st)
		}
		return v
	case in.EnvType != envTypeSwarm:
		// A standalone host has only its failing containers to add.
		if !ok {
			return nil
		}
		return v
	}
	v.Known = st.Known()
	v.NoQuorum, v.NoQuorumSince = st.NoQuorum, st.NoQuorumSince
	if v.Known {
		fillFromNodes(v, st)
	}
	v.NotAManager = notAManager(in)
	sort.Slice(away, func(i, j int) bool { return away[i].Node < away[j].Node })
	v.ManagersUnreachable = append(away, v.ManagersUnreachable...)
	return v
}

// fillFromNodes fills the counts and per-node problems from the node list.
func fillFromNodes(v *ClusterView, st ClusterState) {
	v.NodesTotal = len(st.Read.Nodes)
	for _, n := range st.Read.Nodes {
		if n.State == "ready" {
			v.NodesReady++
		}
		if n.Availability == "active" && (n.State == "ready" || n.State == "down") {
			v.ExpectedAgents++
		}
		if n.Leader {
			v.LeaderPresent = true
		}
		name := cleanText(n.Hostname, maxNodeLen)
		if name == "" {
			name = cleanText(n.ID, maxNodeLen)
		}
		if n.Role == roleManager && n.Reachability == "unreachable" {
			v.ManagersUnreachable = append(v.ManagersUnreachable, NodeSince{Node: name, Since: st.FetchedAt})
		}
		if major, ok := engineMajor(n.EngineVersion); ok && major < minEngineMajor {
			v.UnsupportedNodes = append(v.UnsupportedNodes, NodeSince{Node: name, Since: st.FetchedAt})
		}
	}
}

// notAManager is whether agents are present but none of them is a manager, so no
// cluster question can be asked (D-44).
func notAManager(in RollupInput) bool {
	any := false
	for _, a := range in.Agents {
		if a.Revoked || !a.presentAt(in.Now, in.OfflineAfter) {
			continue
		}
		if a.Role == roleManager {
			return false
		}
		any = true
	}
	return any
}

// engineMajor is the major version of an engine version such as "29.1.0" or
// "25.0.0-beta.1"; ok is false for one that does not start with a number.
func engineMajor(v string) (int, bool) {
	head, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	n, err := strconv.Atoi(head)
	return n, err == nil
}
