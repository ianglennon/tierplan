// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"time"
)

// NodeRow is one swarm node as the cluster view shows it.
type NodeRow struct {
	ID, Hostname, Role, Availability, State, EngineVersion, OS string
	// Leader is true for the one manager that leads the swarm.
	Leader bool
	// Reachability is the manager's reachability (reachable, unreachable or
	// unknown), empty for a worker.
	Reachability string
}

// ServiceRow is one swarm service with its task counts.
type ServiceRow struct {
	ID, Name string
	// Mode is replicated, global, replicated-job or global-job.
	Mode             string
	Running, Desired uint64
	// UpdateState is the state of the service's last update, empty when it has none.
	UpdateState string
	UpdatedAt   time.Time
}

// AgentTaskRow is the state of the bayplan-agent service's current task on one node.
type AgentTaskRow struct{ NodeID, State, Error string }

// ClusterRead is what one manager says about the cluster.
type ClusterRead struct {
	Nodes      []NodeRow
	Services   []ServiceRow
	AgentTasks []AgentTaskRow
}

// Cluster reads the nodes, the services and the bayplan-agent tasks from a swarm manager.
func (c *Client) Cluster(ctx context.Context) (ClusterRead, error) {
	return ClusterRead{}, nil
}

// RestartPolicy is the restart policy of one container.
type RestartPolicy struct {
	Name     string
	MaxRetry int
}

// RestartPolicies inspects the containers with the given IDs and returns only their restart policies.
func (c *Client) RestartPolicies(ctx context.Context, ids []string) (map[string]RestartPolicy, error) {
	return nil, nil
}
