// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"sort"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/tunnel"
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
// not answered, or the data is not read here. There is no place for updates or
// address pools: those columns arrive with the features behind them.
type Overview struct {
	// EnvType is "swarm" or "standalone", empty until it is known.
	EnvType string
	// IsLocalSocket is true for the Docker socket on the server's own host.
	IsLocalSocket bool
	// Nodes counts the nodes whose agent is present of the nodes expected. It is nil
	// while no agent has enrolled and for a local swarm, whose node count is read
	// from the cluster, not from here.
	Nodes *NodeCounts
	// Failing counts failing services. It is nil until service data is read; the
	// swarm fan-out fills it.
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

func lessVersion(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
