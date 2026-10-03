// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// agentServiceName is the name of the swarm service the install command creates.
// Its tasks are the agents, one per node.
const agentServiceName = "bayplan-agent"

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

// Cluster reads the nodes, the services with their task counts and the tasks of
// the bayplan-agent service from a swarm manager. It is read-only and has no
// variant that changes anything. A worker, and a manager that has lost quorum,
// answer with errors whose text carries the daemon's own message, which
// envs.ReasonFor maps to not_a_manager and no_quorum.
//
// Nodes are sorted by hostname then ID and services by name then ID. The agent
// tasks are reduced to the newest task of each node, which is its current one;
// the rows are sorted by node ID.
func (c *Client) Cluster(ctx context.Context) (ClusterRead, error) {
	cli := c.cur()
	var out ClusterRead

	ns, err := cli.NodeList(ctx, client.NodeListOptions{})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list swarm nodes on %s: %w", c.t.Describe(), err)
	}
	out.Nodes = make([]NodeRow, 0, len(ns.Items))
	for _, n := range ns.Items {
		row := NodeRow{
			ID: n.ID, Hostname: n.Description.Hostname, Role: string(n.Spec.Role),
			Availability: string(n.Spec.Availability), State: string(n.Status.State),
			EngineVersion: n.Description.Engine.EngineVersion, OS: n.Description.Platform.OS,
		}
		if n.ManagerStatus != nil {
			row.Leader = n.ManagerStatus.Leader
			row.Reachability = string(n.ManagerStatus.Reachability)
		}
		out.Nodes = append(out.Nodes, row)
	}
	slices.SortFunc(out.Nodes, func(a, b NodeRow) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.ID, b.ID))
	})

	ss, err := cli.ServiceList(ctx, client.ServiceListOptions{Status: true})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list swarm services on %s: %w", c.t.Describe(), err)
	}
	out.Services = make([]ServiceRow, 0, len(ss.Items))
	for _, s := range ss.Items {
		row := ServiceRow{ID: s.ID, Name: s.Spec.Name, Mode: serviceMode(s.Spec.Mode), UpdatedAt: s.UpdatedAt.UTC()}
		if s.ServiceStatus != nil {
			row.Running, row.Desired = s.ServiceStatus.RunningTasks, s.ServiceStatus.DesiredTasks
		}
		if s.UpdateStatus != nil {
			row.UpdateState = string(s.UpdateStatus.State)
		}
		out.Services = append(out.Services, row)
	}
	slices.SortFunc(out.Services, func(a, b ServiceRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	ts, err := cli.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", agentServiceName)})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list the %s tasks on %s: %w", agentServiceName, c.t.Describe(), err)
	}
	out.AgentTasks = currentAgentTasks(ts.Items)
	return out, nil
}

// serviceMode names a service's mode.
func serviceMode(m swarm.ServiceMode) string {
	switch {
	case m.Global != nil:
		return "global"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	case m.GlobalJob != nil:
		return "global-job"
	default:
		return "replicated"
	}
}

// currentAgentTasks keeps the newest task of each node: a swarm keeps the history
// of a service's tasks, and only the latest says how the agent is doing there.
func currentAgentTasks(tasks []swarm.Task) []AgentTaskRow {
	newest := map[string]swarm.Task{}
	for _, t := range tasks {
		if t.NodeID == "" {
			continue // not scheduled yet
		}
		if cur, ok := newest[t.NodeID]; !ok || t.CreatedAt.After(cur.CreatedAt) || t.CreatedAt.Equal(cur.CreatedAt) && t.ID > cur.ID {
			newest[t.NodeID] = t
		}
	}
	rows := make([]AgentTaskRow, 0, len(newest))
	for node, t := range newest {
		rows = append(rows, AgentTaskRow{NodeID: node, State: string(t.Status.State), Error: t.Status.Err})
	}
	slices.SortFunc(rows, func(a, b AgentTaskRow) int { return cmp.Compare(a.NodeID, b.NodeID) })
	return rows
}

// RestartPolicy is the restart policy of one container.
type RestartPolicy struct {
	Name     string
	MaxRetry int
}

// RestartPolicies inspects each container and returns only its restart policy.
// The inspect answer also carries the container's environment variables and
// mounts, which can hold secrets, and none of it is kept. A container with no
// policy is reported as "no". An unknown container is an error; the caller asks
// for IDs it has just listed.
func (c *Client) RestartPolicies(ctx context.Context, ids []string) (map[string]RestartPolicy, error) {
	cli := c.cur()
	out := make(map[string]RestartPolicy, len(ids))
	for _, id := range ids {
		res, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect container %s on %s: %w", id, c.t.Describe(), err)
		}
		p := RestartPolicy{Name: "no"}
		if hc := res.Container.HostConfig; hc != nil && hc.RestartPolicy.Name != "" {
			p = RestartPolicy{Name: string(hc.RestartPolicy.Name), MaxRetry: hc.RestartPolicy.MaximumRetryCount}
		}
		out[id] = p
	}
	return out, nil
}
