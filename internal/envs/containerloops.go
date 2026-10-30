// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// Detecting a standalone container that keeps dying (plan 06-36, RES-13, D-15, RESEARCH
// Pattern 8 and A12). A swarm service's loop is told from its failed tasks (restartloop.go);
// a container run by hand or by compose has no task, so it is told from the die events the
// environment's feed already holds, with the exit code they carry.

const (
	eventTypeContainer = "container"
	eventActionDie     = "die"
	attrExitCode       = "exitCode"
	// labelSwarmTaskID marks the container of a swarm task. Its service is the one in a loop,
	// never the task (D-15).
	labelSwarmTaskID = "com.docker.swarm.task.id"
	// restartPolicyNo is the policy of a container that is never restarted by the engine.
	restartPolicyNo = "no"
)

// ContainerLoop is a standalone container that keeps dying and is restarted by its policy
// (RES-13, D-15).
type ContainerLoop struct {
	// NodeID is the node the container is on, as the container routes take it, and Name its
	// name. ContainerID is the ID of the container of that name as of the last read.
	NodeID, Name, ContainerID string
	// Count is how many distinct deaths with a non-zero exit code fell inside the window as of
	// the last read.
	Count int
	// Window is the window they were counted in.
	Window time.Duration
	// LastExitCode is the exit code of the newest death, LatestAt when it died.
	LastExitCode string
	// Since is when the container was first flagged, in the current run of the flag, and
	// LatestAt when its newest death was.
	Since, LatestAt time.Time
	// Muted is true when a user muted the container's restart-loop alerts (D-16): the flag
	// stays, the bell leaves it out.
	Muted bool
}

// ContainerLoopKey is how a container is named across reads and in its mute: its node and
// its name. A container recreated under the same name on the same node is the same one, so
// it keeps its mute (RESEARCH A12). A Docker container name never holds a slash.
func ContainerLoopKey(nodeID, name string) string { return nodeID + "/" + name }

// ContainerLoopSource is the container events one node's feed holds.
type ContainerLoopSource struct {
	NodeID string
	Events []dockerx.EventRow
}

// containerDeaths is what the tracker keeps of one container: when each of its deaths with
// a non-zero exit code happened, by event ID, the newest of them, and when the flag was
// raised (zero while it is not).
type containerDeaths struct {
	nodeID, name, containerID string
	at                        map[string]time.Time
	latestAt                  time.Time
	lastExit                  string
	flaggedAt                 time.Time
}

// ContainerLoopTracker is the restart-loop detector of the standalone containers of one
// environment (RES-13). It is fed the die events of every cycle and remembers the ones it
// has seen, so a count survives the feed's ring dropping an event. It only flags: it holds
// no client and no function, so it has nothing to restart, stop or remove a container with.
// It is safe for concurrent use.
type ContainerLoopTracker struct {
	mu         sync.Mutex
	containers map[string]*containerDeaths
}

// NewContainerLoopTracker returns a tracker that has seen nothing.
func NewContainerLoopTracker() *ContainerLoopTracker {
	return &ContainerLoopTracker{containers: map[string]*containerDeaths{}}
}

// Observe takes the container events of every node of an environment and returns the
// standalone containers in a restart loop by ContainerLoopKey. now is the clock, so the same
// calls always give the same answer.
//
// A death is a container event of action die whose exit code is present and not 0: a clean
// exit is the container being stopped, not failing. A container is flagged when at least
// threshold distinct deaths of it fell within window of now (inclusive, and a death exactly
// window old is out), its restart policy is not "no" and it is not the container of a swarm
// task. Once flagged it stays flagged until a whole window passes with no new death
// (hysteresis, as for services), so Count can fall below the threshold while the flag
// holds. A death seen in several cycles counts once. A threshold below one or a window of
// zero or less is the default.
//
// rows lists a node's containers and reports false when it cannot (the node is unreachable):
// it is called at most once per node and only for a node with a death still in its window. A
// container that is not listed is forgotten, with its deaths. policy gives a container's
// restart policy and reports false when it cannot be read; it is called only for a container
// that has reached the threshold and is not flagged yet, so a calm host costs no inspect. A
// container whose node or policy cannot be read is not flagged this cycle, and a flag already
// raised is kept.
func (t *ContainerLoopTracker) Observe(now time.Time, sources []ContainerLoopSource, rows func(nodeID string) ([]dockerx.ContainerRow, bool), policy func(nodeID string, row dockerx.ContainerRow) (string, bool), threshold int, window time.Duration) map[string]ContainerLoop {
	if threshold < 1 {
		threshold = DefaultRestartLoopFailures
	}
	if window <= 0 {
		window = DefaultRestartLoopWindow
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, src := range sources {
		for _, ev := range src.Events {
			t.record(now, window, src.NodeID, ev)
		}
	}

	type nodeRows struct {
		byName map[string]dockerx.ContainerRow
		ok     bool
	}
	nodes := map[string]nodeRows{}
	nodeOf := func(id string) nodeRows {
		if n, seen := nodes[id]; seen {
			return n
		}
		n := nodeRows{}
		if list, ok := rows(id); ok {
			n.ok, n.byName = true, make(map[string]dockerx.ContainerRow, len(list))
			for _, r := range list {
				n.byName[r.Name] = r
			}
		}
		nodes[id] = n
		return n
	}

	// Sorted, so that what is asked of policy, and in what order, is the same every time.
	keys := make([]string, 0, len(t.containers))
	for key := range t.containers {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	out := map[string]ContainerLoop{}
	for _, key := range keys {
		d := t.containers[key]
		for id, at := range d.at {
			if now.Sub(at) >= window {
				delete(d.at, id)
			}
		}
		if len(d.at) == 0 {
			delete(t.containers, key)
			continue
		}
		node := nodeOf(d.nodeID)
		if node.ok {
			row, listed := node.byName[d.name]
			if !listed || row.Labels[labelSwarmTaskID] != "" {
				delete(t.containers, key)
				continue
			}
			d.containerID = row.ID
			if d.flaggedAt.IsZero() {
				if len(d.at) < threshold {
					continue
				}
				if name, known := policy(d.nodeID, row); !known || name == "" || name == restartPolicyNo {
					continue
				}
				d.flaggedAt = now
			}
		}
		if d.flaggedAt.IsZero() {
			continue
		}
		out[key] = ContainerLoop{
			NodeID: d.nodeID, Name: d.name, ContainerID: d.containerID, Count: len(d.at), Window: window,
			LastExitCode: d.lastExit, Since: d.flaggedAt, LatestAt: d.latestAt,
		}
	}
	return out
}

// record remembers one event of a node when it is a death inside the window. The caller
// holds t.mu.
func (t *ContainerLoopTracker) record(now time.Time, window time.Duration, nodeID string, ev dockerx.EventRow) {
	if ev.Type != eventTypeContainer || ev.Action != eventActionDie || ev.Name == "" {
		return
	}
	if code := ev.Attributes[attrExitCode]; code == "" || code == "0" {
		return
	}
	at := ev.Time
	if at.IsZero() && ev.TimeNano > 0 {
		at = time.Unix(0, ev.TimeNano)
	}
	if at.IsZero() {
		at = now
	}
	if now.Sub(at) >= window {
		return
	}
	key := ContainerLoopKey(nodeID, ev.Name)
	d := t.containers[key]
	if d == nil {
		d = &containerDeaths{nodeID: nodeID, name: ev.Name, containerID: ev.ActorID, at: map[string]time.Time{}}
		t.containers[key] = d
	}
	if _, seen := d.at[ev.ID]; seen {
		return
	}
	d.at[ev.ID] = at
	if !at.Before(d.latestAt) {
		d.latestAt, d.lastExit = at, ev.Attributes[attrExitCode]
	}
}

// ApplyContainerMutes merges the mutes of an environment into the loops the tracker found
// (D-16). It is pure and returns copies: the tracker's answer is shared with readers of the
// state, so it is never changed in place. mutedKeys are ContainerLoopKeys.
//
//   - merged is loops with Muted set on the ones a user muted. A mute never creates a loop.
//   - muted is every muted key, looping or not, so a page can say a quiet container is muted
//     and a container recreated under its name is muted again at once (A12).
func ApplyContainerMutes(loops map[string]ContainerLoop, mutedKeys []string) (merged map[string]ContainerLoop, muted map[string]bool) {
	muted = make(map[string]bool, len(mutedKeys))
	for _, key := range mutedKeys {
		muted[key] = true
	}
	merged = make(map[string]ContainerLoop, len(loops))
	for key, l := range loops {
		l.Muted = muted[key]
		merged[key] = l
	}
	return merged, muted
}

// get is the remembered restart policy of a container.
func (c *PolicyCache) get(id string) (dockerx.RestartPolicy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[id]
	return p, ok
}

// put remembers the restart policy of a container.
func (c *PolicyCache) put(id string, p dockerx.RestartPolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = p
}

// keepOnly forgets every container but the ones in keep.
func (c *PolicyCache) keepOnly(keep map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.m {
		if !keep[id] {
			delete(c.m, id)
		}
	}
}

// loopTarget is a node of an environment as the container loops see it: the node ID the
// container routes take, and the key of its engine in the monitor, which is also the key of
// its event feed.
type loopTarget struct{ nodeID, key string }

// loopTargets are the nodes of an environment that can be read now.
func loopTargets(in RollupInput) []loopTarget {
	if in.Kind == KindLocal {
		return []loopTarget{{nodeID: in.EnvID, key: in.EnvID}}
	}
	var out []loopTarget
	for _, a := range connectedAgents(in) {
		out = append(out, loopTarget{nodeID: a.ID, key: a.TargetKey()})
	}
	return out
}

// observeContainerLoops feeds the die events every node's feed holds to the environment's
// container tracker and stores what it found, with the mutes merged in (RES-13, D-15, D-16).
// It reads the feed's ring and the cached container lists, and inspects a container only to
// learn its restart policy, once, after it reached the threshold, at most MaxPolicyInspects
// containers a cycle (T-06-106). It never writes to Docker (T-06-104).
//
// standalone says the environment's policy cache is the one FailingContainers keeps for a
// single engine: it is left whole. On any other environment the cache is only this
// detector's, and it keeps just the containers asked about this cycle.
func (c *Clusters) observeContainerLoops(ctx context.Context, in RollupInput, e *clusterEntry, standalone bool) {
	// The cluster read's own deadline may be nearly spent, and this is not part of it.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	targets := loopTargets(in)
	keyOf := make(map[string]string, len(targets))
	sources := make([]ContainerLoopSource, 0, len(targets))
	for _, tg := range targets {
		keyOf[tg.nodeID] = tg.key
		var events []dockerx.EventRow
		if c.events != nil {
			events = c.events.Recent(tg.key, ringSize)
		}
		sources = append(sources, ContainerLoopSource{NodeID: tg.nodeID, Events: events})
	}
	rows := func(nodeID string) ([]dockerx.ContainerRow, bool) {
		key, ok := keyOf[nodeID]
		if !ok {
			return nil, false
		}
		snap, _, err := c.snaps.Get(dctx, key)
		if err != nil {
			return nil, false
		}
		return snap.Containers, true
	}
	inspects := 0
	asked := map[string]bool{}
	policy := func(nodeID string, row dockerx.ContainerRow) (string, bool) {
		asked[row.ID] = true
		if p, ok := e.policies.get(row.ID); ok {
			return p.Name, true
		}
		key, ok := keyOf[nodeID]
		if !ok || inspects >= MaxPolicyInspects {
			return "", false
		}
		inspects++
		cli, _, err := c.svc.mon.Client(key)
		if err != nil {
			return "", false
		}
		got, err := cli.RestartPolicies(dctx, []string{row.ID})
		p, ok := got[row.ID]
		if err != nil || !ok {
			return "", false
		}
		e.policies.put(row.ID, p)
		return p.Name, true
	}
	found := e.containerLoops.Observe(c.now(), sources, rows, policy, in.RestartLoopFailures, in.RestartLoopWindow)
	if !standalone {
		e.policies.keepOnly(asked)
	}

	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer scancel()
	e.mu.Lock()
	previous := e.state.MutedContainers
	e.mu.Unlock()
	merged, muted := ApplyContainerMutes(found, c.mutedContainerKeys(sctx, in.EnvID, previous))
	e.mu.Lock()
	e.state.ContainerLoops, e.state.MutedContainers = merged, muted
	e.mu.Unlock()
}
