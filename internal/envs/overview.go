// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"sort"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/tunnel"
)

// NodeCounts is how many of an environment's nodes are ready of how many it has.
type NodeCounts struct{ Ready, Total int }

// FailingCounts is how many of an environment's services are failing of how many
// it has.
type FailingCounts struct{ Failing, Total int }

// AgentCounts is how many of an environment's expected agents are connected.
type AgentCounts struct{ Connected, Expected int }

// Overview is what the environments table, the switcher and the mobile rows show
// of an environment without asking for its resources (D-32). A nil pointer or an
// empty string is "not known": the environment has no agent yet, the engine has
// not answered, or the data is not read here. The updates column arrives with the
// feature behind it.
type Overview struct {
	// EnvType is "swarm" or "standalone", empty until it is known.
	EnvType string
	// IsLocalSocket is true for the Docker socket on the server's own host.
	IsLocalSocket bool
	// Nodes counts the nodes whose agent is present of the nodes expected. It is nil
	// while no agent has enrolled and for a local swarm, whose node count is read
	// from the cluster, not from here.
	Nodes *NodeCounts
	// Failing counts failing services of a swarm, or failing containers of a
	// standalone host (D-34). It is nil until they have been read.
	Failing *FailingCounts
	// Agents counts connected agents of expected ones, on a swarm only.
	Agents *AgentCounts
	// AgentVersion is the oldest version among the agents, empty for none or the
	// local socket. UpdateAvailable is true when any agent is behind the server.
	AgentVersion    string
	UpdateAvailable bool
	// EngineVersion is the engine version of the primary target.
	EngineVersion string
	// LastSeen is the last sign of life of any agent: this run's, else the stored
	// one. For the local socket it is the last probe that found the engine online.
	LastSeen time.Time
	// AddressSpace is the fullest pool or network of the environment and its orphan count,
	// from the last read of its address space. It is nil when nothing has been measured, and
	// while the environment is offline, since the numbers would be out of date.
	AddressSpace *AddressSummary
}

// overview works out an environment's Overview from the same input its health is
// rolled up from, the status of the target its engine data is read through and the
// version of the server.
func overview(in RollupInput, st Status, serverVersion string) Overview {
	o := Overview{IsLocalSocket: in.Kind == KindLocal, EngineVersion: st.Probe.EngineVersion}
	if in.Kind == KindLocal {
		switch st.Probe.Type {
		case dockerx.TypeSwarmManager, dockerx.TypeSwarmWorker:
			o.EnvType = envTypeSwarm
		case dockerx.TypeStandalone:
			o.EnvType = envTypeStandalone
		}
		switch {
		case o.EnvType == envTypeSwarm:
			// Left unknown: the nodes of a swarm are the cluster's, not this host's.
		case st.State == StateOnline:
			o.Nodes = &NodeCounts{Ready: 1, Total: 1}
		case st.State == StateOffline || st.State == StateUnsupported:
			o.Nodes = &NodeCounts{Ready: 0, Total: 1}
		}
		if c := in.Cluster; c != nil {
			// The nodes and failing services of a local swarm manager are the cluster's.
			// A worker, or a socket that is not answering, cannot say: the cached read
			// was made while a manager was up and is not shown as current.
			if c.Known && o.EnvType == envTypeSwarm && st.Probe.Type == dockerx.TypeSwarmManager && st.State == StateOnline {
				o.Nodes = &NodeCounts{Ready: c.NodesReady, Total: c.NodesTotal}
			}
			o.Failing = c.Failing
			if st.State == StateOnline {
				o.AddressSpace = c.AddressSpace
			}
		}
		if st.State == StateOnline {
			o.LastSeen = st.LastAttempt
		}
		return o
	}

	o.EnvType = in.EnvType
	var active []AgentView
	var seen time.Time
	for _, a := range in.Agents {
		seen = later(seen, a.LastSeen)
		if !a.Revoked {
			active = append(active, a)
		}
	}
	if seen.IsZero() {
		// Nothing heard this run: the stored time is the best date there is.
		for _, a := range in.Agents {
			seen = later(seen, a.StoredSeen)
		}
	}
	o.LastSeen = seen
	if len(active) == 0 {
		return o
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })

	total := max(in.Expected, len(active))
	ready, connected := 0, 0
	for _, a := range active {
		if a.presentAt(in.Now, in.OfflineAfter) {
			ready++
		}
		if a.Connected {
			connected++
		}
	}
	o.Nodes = &NodeCounts{Ready: ready, Total: total}
	if in.EnvType == envTypeSwarm {
		o.Agents = &AgentCounts{Connected: connected, Expected: total}
	}
	if c := in.Cluster; c != nil {
		// What a manager said about the cluster is better than what the agents
		// imply: it counts nodes that never enrolled and leaves out drained ones. It is
		// only current while a manager is connected to answer; with none, the read was
		// made while one was up and the agents' own count stands (the summary route
		// says "unknown" for the same case).
		if c.Known && in.EnvType == envTypeSwarm && managerConnected(active) {
			o.Nodes = &NodeCounts{Ready: c.NodesReady, Total: c.NodesTotal}
			o.Agents = &AgentCounts{Connected: connected, Expected: max(c.ExpectedAgents, connected)}
		}
		o.Failing = c.Failing
		if ready > 0 {
			o.AddressSpace = c.AddressSpace
		}
	}

	// The oldest version stands for the environment, so the update badge shows while
	// any agent is behind; versions that are not releases cannot be ordered and give
	// way to ones that can.
	var oldest [3]int
	oldestOK := false
	for _, a := range active {
		if a.Version == "" {
			continue
		}
		v, ok := tunnel.ParseVersion(a.Version)
		if o.AgentVersion == "" || ok && (!oldestOK || lessVersion(v, oldest)) {
			o.AgentVersion, oldest, oldestOK = a.Version, v, ok
		}
		if tunnel.Behind(serverVersion, a.Version) {
			o.UpdateAvailable = true
		}
	}
	return o
}

// managerConnected is whether any of the agents is a connected manager, the only
// kind that can answer a cluster question.
func managerConnected(agents []AgentView) bool {
	for _, a := range agents {
		if a.Connected && a.Role == roleManager {
			return true
		}
	}
	return false
}

func lessVersion(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
