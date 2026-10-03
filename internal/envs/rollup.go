// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
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
	// Revoked is true for an agent an admin revoked; RevokedAt is when.
	Revoked   bool
	RevokedAt time.Time
	// Connected is true while the agent has a live session.
	Connected bool
	// ConnectedAt is when the current or last session began.
	ConnectedAt time.Time
	// LastSeen is the last sign of life: a heartbeat, or the connect itself.
	LastSeen time.Time
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
}

// ClusterView is what the swarm causes of degraded and offline are decided from.
// Plan 20 fills it; nil in RollupInput means no cluster information.
type ClusterView struct{}

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

// Rollup decides the health of one environment from its agents and their targets.
func Rollup(in RollupInput) Health {
	_ = in
	return Health{}
}

// ClockOffsetSeconds is d in whole seconds, rounded toward zero, with its sign.
func ClockOffsetSeconds(d time.Duration) int {
	_ = d
	return 0
}

// CertDaysRemaining is how many whole days are left until notAfter, rounded
// down; it is negative once the certificate has expired.
func CertDaysRemaining(notAfter, now time.Time) int {
	_, _ = notAfter, now
	return 0
}
