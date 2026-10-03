// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

// Targets returns the monitor keys an environment is read through, in a fixed
// order. The local socket is one target keyed by the environment itself. An agent
// environment has one target per agent that is connected now, keyed by the agent
// (D-47): an agent that is only "present" inside the offline window has no live
// session and so no target. Nothing keys a Docker client by an agent
// environment's own ID.
func Targets(in RollupInput) []string {
	if in.Kind == KindLocal {
		return []string{in.EnvID}
	}
	var keys []string
	for _, a := range connectedAgents(in) {
		keys = append(keys, AgentTarget(a.ID))
	}
	return keys
}

// Primary is the one target a read of a single engine goes to. For the local
// socket it is the environment itself. For an agent environment it is the
// connected manager with the lowest ID, because only a manager answers cluster
// questions, else the connected agent with the lowest ID, which on a standalone
// host is the only one. Which manager serves cluster calls, and the failover
// between them, is plan 20's choice (ClusterCall); this is the one-engine read.
func Primary(in RollupInput) (string, bool) {
	if in.Kind == KindLocal {
		return in.EnvID, true
	}
	connected := connectedAgents(in)
	for _, a := range connected {
		if a.Role == roleManager {
			return AgentTarget(a.ID), true
		}
	}
	if len(connected) > 0 {
		return AgentTarget(connected[0].ID), true
	}
	return "", false
}
