// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"sort"
	"time"
)

// States an environment can be in besides the three the monitor reports for one
// engine (D-33). They are decided by Rollup over the agents of the environment,
// never by a monitor.
const (
	// StatePending is an agent environment no agent has enrolled in yet.
	StatePending State = "pending"
	// StateReconnecting is an environment none of whose agents has reconnected
	// since the server started, inside the startup grace (D-41).
	StateReconnecting State = "reconnecting"
	// StateDegraded is an environment that works but not completely: a node is
	// missing, cannot reach Docker, runs an unsupported engine, or has a
	// certificate or clock that needs attention.
	StateDegraded State = "degraded"
	// StateRevoked is an environment every agent of which an admin revoked.
	StateRevoked State = "revoked"
	// StateClusterChanged is a swarm environment whose cluster was re-initialised.
	StateClusterChanged State = "cluster_changed"
)

// Thresholds of the rollup (D-45).
const (
	// CertWarnBefore is how long before a certificate expires it is reported.
	CertWarnBefore = 7 * 24 * time.Hour
	// ClockWarnAbove is the clock offset above which a node is reported.
	ClockWarnAbove = 30 * time.Second
	// IssueCertExpiring is the issue code of a certificate that expires soon. It
	// is not a connection failure, so it has no D-40 code of its own.
	IssueCertExpiring = "cert_expiring"

	envTypeSwarm = "swarm"
	maxNodeLen   = 64
)

// AgentView is everything the rollup knows about one agent row: the stored part
// (the row) and the live part (the hub's memory of its sessions and the monitor's
// status of its target).
type AgentView struct {
	ID, Node, Role string
	// SwarmNodeID is the swarm node the agent runs on, empty for a standalone host
	// and for an agent that reported none.
	SwarmNodeID string
	// Revoked is true for an agent an admin revoked; RevokedAt is when.
	Revoked   bool
	RevokedAt time.Time
	// Connected is true while the agent has a live session.
	Connected bool
	// ConnectedAt is when the current or last session began.
	ConnectedAt time.Time
	// LastSeen is the last sign of life of this server run: a heartbeat, or the
	// connect itself. It is zero for an agent that has not connected since the
	// server started, and presence is decided from it alone.
	LastSeen time.Time
	// StoredSeen is agents.last_seen_at, written at most once a minute. It is never
	// used for presence; it tells how long ago contact was lost when this run has
	// no sign of the agent.
	StoredSeen time.Time
	// Ended is when the last session ended, zero when the server has seen none end.
	Ended time.Time
	// LastCode is why the last session ended (a tunnel Disconnect code).
	LastCode string
	// EngineReachable is what the agent's last heartbeat said about Docker.
	EngineReachable bool
	// Target is the monitor's view of the agent's target.
	Target Status
	// CertNotAfter is when the agent's certificate expires.
	CertNotAfter time.Time
	// ClockOffset is how far the agent's clock is ahead of the server's.
	ClockOffset time.Duration
	// Version is the agent version: what it reported when it last connected this
	// run, else what it reported when it enrolled.
	Version string
	// Socket marks the one view that is not an agent: the folded local host, reached
	// through the Docker socket (D-48). Its ID is its target key.
	Socket bool
	// ReadOnly is what the host owner set (COAMING_AGENT_READ_ONLY): the agent
	// refuses every write. It is known only while the agent is connected.
	ReadOnly bool
	// PrivilegedServices is what the host owner set
	// (COAMING_AGENT_ALLOW_PRIVILEGED_SERVICES): the agent forwards service specs
	// that give tasks host access. Known only while the agent is connected; a later
	// plan reads it to tell, before a write, that a manager refuses host access.
	PrivilegedServices bool
	// HostNet is what the connected agent reported about reading its host's routes
	// (wire.HostNetOK or wire.HostNetUnmounted), empty for an agent that reported
	// nothing, which is one older than Phase 10. Known only while the agent is
	// connected; the subnet picker's route check asks an agent only when it is "ok".
	HostNet string
}

// TargetKey is the monitor key of the view's Docker engine: the agent's target, or for
// the folded local host its socket's.
func (a AgentView) TargetKey() string {
	if a.Socket {
		return a.ID
	}
	return AgentTarget(a.ID)
}

// NodeSince names a node and when it started to have a problem.
type NodeSince struct {
	Node  string
	Since time.Time
}

// ClusterView is what the swarm causes of degraded are decided from, and what the
// overview counts nodes, agents and failing services from. It is a snapshot of
// what the last read of the cluster said, filled by Clusters.View; nil in
// RollupInput means no cluster information, and the rollup then decides from the
// agents alone.
type ClusterView struct {
	// Known is true while a node list from a manager is held and no quorum problem
	// has made it stale. The counts below mean nothing otherwise.
	Known bool
	// NodesReady and NodesTotal count the nodes of the cluster that are ready and
	// all of them.
	NodesReady, NodesTotal int
	// ExpectedAgents is how many nodes should run an agent: availability active
	// and state ready or down (a drained or paused node gets no global task).
	ExpectedAgents int
	// LeaderPresent is true when the node list names a leader.
	LeaderPresent bool
	// NoQuorum is true when a manager answered the quorum-loss error; the cluster
	// calls fail and node-local data keeps working. NoQuorumSince is when it began.
	NoQuorum      bool
	NoQuorumSince time.Time
	// NotAManager is true when agents are present but none of them is a manager, so
	// nothing can answer a cluster question (D-44).
	NotAManager bool
	// ManagersUnreachable are managers that failed over to another or that the node
	// list calls unreachable, by name, until they are back.
	ManagersUnreachable []NodeSince
	// UnsupportedNodes are nodes on an engine older than Docker 24, by name (D-47).
	UnsupportedNodes []NodeSince
	// Failing counts failing services of a swarm, or failing containers of a
	// standalone host; nil until it has been worked out.
	Failing *FailingCounts
}

// RollupInput is everything Rollup decides from. All times are passed in, so the
// same input always gives the same Health.
type RollupInput struct {
	EnvID string
	// Kind is KindLocal or KindAgent; EnvType is "swarm" or "standalone" for an
	// agent environment.
	Kind, EnvType string
	// ClusterChanged is set once the cluster behind the environment was
	// re-initialised; ClusterChangedAt is when.
	ClusterChanged   bool
	ClusterChangedAt time.Time
	Agents           []AgentView
	// Expected is how many agents the environment should have; zero means one per
	// agent that is not revoked.
	Expected    int
	ServerStart time.Time
	Now         time.Time
	// OfflineAfter is how long a disconnected agent still counts as present, and
	// Grace how long after the server started a missing agent is shown as
	// reconnecting.
	OfflineAfter, Grace time.Duration
	// Local is the monitor's status of the local socket, used when Kind is KindLocal.
	Local   Status
	Cluster *ClusterView
	// RestartLoopFailures and RestartLoopWindow are the environment's restart-loop
	// threshold and window (RES-13).
	RestartLoopFailures int
	RestartLoopWindow   time.Duration
}

// Issue is one reason an environment is degraded.
type Issue struct {
	Code, Node string
	Since      time.Time
}

// Health is the rolled-up state of one environment (D-33).
type Health struct {
	State State
	// Reason is a D-40 code (or IssueCertExpiring) for every state but online and
	// pending, and for the local socket the probe's reason.
	Reason string
	Since  time.Time
	Issues []Issue
}

// Rollup decides the health of one environment from its agents and their targets
// (D-33). It is a pure function: the clock is in the input, the input is not
// changed, and the same input always gives the same state, reason, since and
// issue order.
//
// The rules, first match wins:
//
//   - the local socket keeps what its monitor says;
//   - a cluster that was re-initialised is cluster_changed, whatever the agents say;
//   - no agent rows is pending, and only revoked rows is revoked;
//   - no agent present is reconnecting inside the grace after the server started
//     and offline after it, with reason timeout when the last session ended by
//     heartbeat timeout (D-41);
//   - a standalone host whose agent cannot reach Docker is offline, and one on an
//     unsupported engine is unsupported;
//   - anything else wrong, on any node, is an issue and makes the environment
//     degraded; no issue is online.
//
// An agent is present while it is connected, or for OfflineAfter after its last
// sign of life, so a blip shorter than the window changes nothing (D-41).
func Rollup(in RollupInput) Health {
	if in.Kind == KindLocal {
		return Health{State: in.Local.State, Reason: in.Local.Reason, Since: in.Local.Since}
	}
	if in.ClusterChanged {
		return Health{State: StateClusterChanged, Reason: ReasonClusterChanged, Since: firstSet(in.ClusterChangedAt, in.ServerStart)}
	}
	// A copy, so the caller's slice is neither reordered nor aliased.
	agents := append([]AgentView(nil), in.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	if len(agents) == 0 {
		return Health{State: StatePending}
	}

	var active, present []AgentView
	var revokedAt time.Time
	for _, a := range agents {
		if a.Revoked {
			revokedAt = later(revokedAt, a.RevokedAt)
			continue
		}
		active = append(active, a)
		if a.presentAt(in.Now, in.OfflineAfter) {
			present = append(present, a)
		}
	}
	if len(active) == 0 {
		return Health{State: StateRevoked, Reason: ReasonRevoked, Since: firstSet(revokedAt, in.ServerStart)}
	}
	if len(present) == 0 {
		return absent(in, active)
	}

	issues := nodeIssues(in, active, present)
	if in.EnvType == envTypeSwarm {
		issues = append(issues, clusterIssues(in.Cluster, issues)...)
		sortIssues(issues)
	}
	if in.EnvType != envTypeSwarm && len(active) == 1 {
		// A standalone host has one engine: what is wrong with it is the state of
		// the environment, not one of several nodes' issues.
		a := present[0]
		switch {
		case a.Target.State == StateUnsupported:
			return Health{State: StateUnsupported, Reason: ReasonUnsupportedEngine, Since: firstSet(a.Target.Since, a.ConnectedAt, in.ServerStart)}
		case a.engineUnreachable():
			return Health{State: StateOffline, Reason: ReasonEngineUnreachable, Since: unreachableSince(a)}
		}
		// Only certificate and clock issues are left for it.
	}
	if len(issues) > 0 {
		since := issues[0].Since
		for _, is := range issues[1:] {
			if since.IsZero() || (!is.Since.IsZero() && is.Since.Before(since)) {
				since = is.Since
			}
		}
		return Health{State: StateDegraded, Reason: issues[0].Code, Since: firstSet(since, in.ServerStart), Issues: issues}
	}
	return Health{State: StateOnline, Since: firstSet(present[0].Target.Since, present[0].ConnectedAt)}
}

// absent is the health of an environment with agents, none of them present.
func absent(in RollupInput, active []AgentView) Health {
	if in.Now.Sub(in.ServerStart) < in.Grace {
		return Health{State: StateReconnecting, Reason: ReasonAgentOffline, Since: in.ServerStart}
	}
	// The reason comes from the session that ended last, the best evidence of how
	// contact was lost; ties go to the lowest ID, which active already is.
	reason := ReasonAgentOffline
	var lastEnd, lastSeen time.Time
	for _, a := range active {
		if a.Ended.After(lastEnd) {
			lastEnd = a.Ended
			if a.LastCode == ReasonTimeout {
				reason = ReasonTimeout
			} else {
				reason = ReasonAgentOffline
			}
		}
		lastSeen = later(later(lastSeen, a.LastSeen), a.StoredSeen)
	}
	return Health{State: StateOffline, Reason: reason, Since: firstSet(lastEnd, lastSeen, in.ServerStart)}
}

// issueRank orders issues, worst first: a cluster that has lost its quorum, one
// with no manager to ask, a node that is gone, then one whose engine cannot be
// reached, then one on an engine that is not supported, then the warnings
// (certificate, clock), the order of the overview's severity list.
var issueRank = map[string]int{
	// The cluster cannot answer at all, then nothing could be asked of it.
	ReasonNoQuorum:          0,
	ReasonNotAManager:       1,
	ReasonAgentOffline:      2,
	ReasonTimeout:           2,
	ReasonEngineUnreachable: 3,
	ReasonUnsupportedEngine: 4,
	IssueCertExpiring:       5,
	ReasonClockSkew:         6,
}

// nodeIssues lists what is wrong with the nodes: an agent missing, an engine that
// cannot be reached or is not supported, a certificate close to expiry and a
// clock that is out of step. The list is in a fixed order.
func nodeIssues(in RollupInput, active, present []AgentView) []Issue {
	var issues []Issue
	for _, a := range active {
		if !a.presentAt(in.Now, in.OfflineAfter) {
			issues = append(issues, Issue{Code: ReasonAgentOffline, Node: nodeName(a), Since: firstSet(a.Ended, a.LastSeen, in.ServerStart)})
		}
	}
	// Agents the environment should have but that have never enrolled.
	for i := len(active); i < in.Expected; i++ {
		issues = append(issues, Issue{Code: ReasonAgentOffline, Since: in.ServerStart})
	}
	for _, a := range present {
		switch {
		case a.Target.State == StateUnsupported:
			issues = append(issues, Issue{Code: ReasonUnsupportedEngine, Node: nodeName(a), Since: firstSet(a.Target.Since, a.ConnectedAt)})
		case a.engineUnreachable():
			issues = append(issues, Issue{Code: ReasonEngineUnreachable, Node: nodeName(a), Since: unreachableSince(a)})
		}
		if !a.CertNotAfter.IsZero() && a.CertNotAfter.Sub(in.Now) < CertWarnBefore {
			issues = append(issues, Issue{Code: IssueCertExpiring, Node: nodeName(a), Since: a.CertNotAfter.Add(-CertWarnBefore)})
		}
		if abs(a.ClockOffset) > ClockWarnAbove {
			issues = append(issues, Issue{Code: ReasonClockSkew, Node: nodeName(a), Since: a.ConnectedAt})
		}
	}
	sortIssues(issues)
	return issues
}

// sortIssues puts issues in their fixed order: worst first, then by node name.
func sortIssues(issues []Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if ri, rj := issueRank[issues[i].Code], issueRank[issues[j].Code]; ri != rj {
			return ri < rj
		}
		if issues[i].Node != issues[j].Node {
			return issues[i].Node < issues[j].Node
		}
		return issues[i].Code < issues[j].Code
	})
}

// clusterIssues are the swarm problems the cluster view reports (D-43, D-44, D-47):
// the quorum lost, no manager to ask, a manager that failed over or is unreachable,
// and a node on an engine older than Docker 24. A node the agents' own issues
// already name for the same cause is not listed twice. A nil view adds nothing.
func clusterIssues(c *ClusterView, have []Issue) []Issue {
	if c == nil {
		return nil
	}
	listed := make(map[string]struct{}, len(have))
	for _, is := range have {
		listed[is.Code+"\x00"+is.Node] = struct{}{}
	}
	var out []Issue
	add := func(code, node string, since time.Time) {
		key := code + "\x00" + node
		if _, dup := listed[key]; dup {
			return
		}
		listed[key] = struct{}{}
		out = append(out, Issue{Code: code, Node: node, Since: since})
	}
	if c.NoQuorum {
		add(ReasonNoQuorum, "", c.NoQuorumSince)
	}
	if c.NotAManager {
		add(ReasonNotAManager, "", time.Time{})
	}
	for _, m := range c.ManagersUnreachable {
		add(ReasonAgentOffline, cleanText(m.Node, maxNodeLen), m.Since)
	}
	for _, n := range c.UnsupportedNodes {
		add(ReasonUnsupportedEngine, cleanText(n.Node, maxNodeLen), n.Since)
	}
	return out
}

// presentAt reports whether the agent counts as present at now: connected, or
// within offlineAfter of its last sign of life. An agent that was never heard from
// is never present.
func (a AgentView) presentAt(now time.Time, offlineAfter time.Duration) bool {
	if a.Revoked {
		return false
	}
	if a.Connected {
		return true
	}
	if a.LastSeen.IsZero() {
		return false
	}
	return now.Sub(a.LastSeen) < offlineAfter
}

// engineUnreachable is whether the agent cannot reach Docker on its host: its
// heartbeat says so, or the monitor's probe through the tunnel failed.
func (a AgentView) engineUnreachable() bool {
	return !a.EngineReachable || a.Target.State == StateOffline
}

func unreachableSince(a AgentView) time.Time {
	if a.Target.State == StateOffline {
		return firstSet(a.Target.Since, a.ConnectedAt)
	}
	return firstSet(a.ConnectedAt, a.LastSeen)
}

// connectedAgents are the agents that are not revoked and have a live session,
// ordered by ID.
func connectedAgents(in RollupInput) []AgentView {
	var out []AgentView
	for _, a := range in.Agents {
		if a.Connected && !a.Revoked {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// nodeName is the name of a node as shown in an issue: the host name the agent
// reported, cleaned. An agent's text is never trusted (T-02-43): control
// characters are dropped, the name is cut to the length of an environment name,
// and a blank name falls back to the agent ID.
func nodeName(a AgentView) string {
	if name := cleanText(a.Node, maxNodeLen); name != "" {
		return name
	}
	return cleanText(a.ID, maxNodeLen)
}

// ClockOffsetSeconds is d in whole seconds, rounded toward zero, with its sign.
func ClockOffsetSeconds(d time.Duration) int { return int(d / time.Second) }

// CertDaysRemaining is how many whole days are left until notAfter, rounded
// down; it is negative once the certificate has expired.
func CertDaysRemaining(notAfter, now time.Time) int {
	const day = 24 * time.Hour
	d := notAfter.Sub(now)
	days := int(d / day)
	if d < 0 && d%day != 0 {
		days-- // Go truncates toward zero; the days left are rounded down
	}
	return days
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// later is the later of two times; a zero time is the earliest.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// firstSet is the first of ts that is not the zero time, or the zero time.
func firstSet(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
