// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// PlacementInput is the path of the placement view and its optional narrowing to one service.
type PlacementInput struct {
	EnvironmentPathInput
	ServiceID string `query:"serviceId" pattern:"^$|^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"Only the tasks and the service of this service ID or name; every node is still listed. 404 service_not_found for a service the swarm does not have"`
}

// PlacementNode is one swarm node as the placement view shows it, with the facts a
// placement constraint is matched against.
type PlacementNode struct {
	ID           string            `json:"id" doc:"The swarm node ID"`
	Hostname     string            `json:"hostname" doc:"The node's host name"`
	Role         string            `json:"role" doc:"The node's role"`
	Leader       bool              `json:"leader" doc:"True for the manager that leads the swarm"`
	Availability string            `json:"availability" doc:"active, pause or drain"`
	State        string            `json:"state" doc:"The swarm's word for the node: ready, down, disconnected or unknown"`
	Cpus         float64           `json:"cpus" doc:"The CPUs the node reports; 0 when it reports none"`
	MemoryBytes  int64             `json:"memoryBytes" doc:"The memory the node reports, in bytes; 0 when it reports none"`
	Address      string            `json:"address" doc:"The address the swarm reaches the node at, which node.ip constraints are matched against; empty when unknown"`
	OS           string            `json:"os" doc:"The node's operating system (node.platform.os)"`
	Arch         string            `json:"arch" doc:"The node's architecture (node.platform.arch)"`
	Labels       map[string]string `json:"labels" doc:"The node's labels (node.labels.*); an empty object when it has none"`
	EngineLabels map[string]string `json:"engineLabels" doc:"The labels of the node's engine (engine.labels.*); an empty object when it has none"`
	TargetNodeID string            `json:"targetNodeId" doc:"The node ID the container routes of this environment take (?node= of the container page) for this node; empty when no connected agent or local socket reaches it now"`
}

// PlacementTask is one task whose desired state is running.
type PlacementTask struct {
	ID           string `json:"id" doc:"The task ID"`
	ServiceID    string `json:"serviceId" doc:"The service the task belongs to"`
	Service      string `json:"service" doc:"The service's name"`
	Stack        string `json:"stack" doc:"The stack the service belongs to; empty for a service made on its own"`
	Slot         int    `json:"slot" doc:"The replica number of a replicated service's task; 0 for a global service's"`
	NodeID       string `json:"nodeId" doc:"The swarm node the task was placed on; empty while it is not placed. The swarm keeps the ID of a node that has since been removed"`
	NodeKnown    bool   `json:"nodeKnown" doc:"False when the task names a node the swarm no longer lists (a removed node); true otherwise, also for a task with no node"`
	State        string `json:"state" doc:"The state to show: Docker's own word, except unknown for a task that has not ended on a node that is down, disconnected or removed, which cannot say it is still running"`
	DockerState  string `json:"dockerState" doc:"The state Docker last recorded for the task"`
	DesiredState string `json:"desiredState" doc:"The state the swarm wants the task in"`
	Error        string `json:"error" doc:"Docker's error text about the task (for a task that could not be placed, why), cleaned of control characters; empty when there is none. To be shown as plain text"`
}

// PlacementService is a service that has tasks to place, with its placement constraints.
type PlacementService struct {
	ID          string   `json:"id" doc:"The service ID"`
	Name        string   `json:"name" doc:"The service's name"`
	Stack       string   `json:"stack" doc:"The stack the service belongs to; empty for a service made on its own"`
	Mode        string   `json:"mode" doc:"replicated, global, replicated-job or global-job"`
	Constraints []string `json:"constraints" doc:"The placement constraints of the service's spec, as written; an empty list when it has none"`
	Preferences []string `json:"preferences" doc:"The label each Spread placement preference spreads tasks over (node.labels.zone), in the spec's order; an empty list when it has none"`
	MaxReplicas uint64   `json:"maxReplicas" doc:"The most tasks of the service one node may run; 0 when the spec sets no limit"`
}

// PlacementBody is where the tasks of a swarm run.
type PlacementBody struct {
	Nodes    []PlacementNode    `json:"nodes" doc:"Every node of the swarm, sorted by host name then ID"`
	Tasks    []PlacementTask    `json:"tasks" doc:"The tasks whose desired state is running, sorted by service name, slot then task ID. A task with no node has an empty nodeId; one on a node the swarm no longer lists has nodeKnown false"`
	Services []PlacementService `json:"services" doc:"The services the tasks belong to (every service, or the one asked for), sorted by name"`
}

// PlacementOutput wraps PlacementBody for huma.
type PlacementOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         PlacementBody
}

// registerPlacement mounts the placement route.
func registerPlacement(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getPlacement", Method: http.MethodGet, Path: "/api/env/{envId}/placement",
		Summary: "Where the tasks of a swarm run",
		Description: "The nodes of the swarm, the tasks whose desired state is running and the services they belong to, read live from a manager in one read. " +
			"A task with no node (pending, no suitable node) has an empty nodeId and its error; a task on a node the swarm no longer lists has nodeKnown false. " +
			"A task that has not ended on a node that is down, disconnected or removed reads unknown, never running; dockerState keeps Docker's own word. " +
			"With serviceId the tasks and services are narrowed to that service while every node stays, so empty columns show. " +
			"Each node carries the facts a placement constraint is matched against, and each service its constraints. " +
			"404 service_not_found for an unknown serviceId; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *PlacementInput) (*PlacementOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getPlacement(ctx, d, env.ID, in.ServiceID)
			if err != nil {
				return nil, err
			}
			return &PlacementOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// getPlacement reads the nodes, the services and the running-desired tasks in one cluster
// read. Nothing is written.
func getPlacement(ctx context.Context, d ServicesDeps, envID, serviceID string) (PlacementBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return PlacementBody{}, err
	}
	var (
		read dockerx.ClusterRead
		rows []dockerx.TaskRow
	)
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		if read, e = c.Cluster(readCtx); e != nil {
			return e
		}
		filter := dockerx.TaskFilter{DesiredState: "running"}
		if serviceID != "" {
			i := slices.IndexFunc(read.Services, func(s dockerx.ServiceRow) bool { return s.ID == serviceID || s.Name == serviceID })
			if i < 0 {
				return dockerx.ErrServiceNotFound
			}
			read.Services = []dockerx.ServiceRow{read.Services[i]}
			filter.ServiceIDs = []string{read.Services[0].ID}
		}
		rows, e = c.Tasks(readCtx, filter)
		return e
	})
	if err != nil {
		return PlacementBody{}, serviceReadProblem(d, envID, err)
	}
	return placementOf(read, rows, swarmNodeTargets(ctx, d, envID)), nil
}

// swarmNodeTargets maps swarm node IDs to the nodes the container routes take. A node nothing
// reaches is a task that cannot be opened, not a failed read, so a failure here is logged and
// answered with no targets.
func swarmNodeTargets(ctx context.Context, d ServicesDeps, envID string) map[string]envs.NodeRef {
	if d.Aggregator == nil {
		return nil
	}
	targets, err := d.Aggregator.SwarmNodeTargets(ctx, envID)
	if err != nil {
		d.Log.Warn("mapping swarm nodes to agents failed", "environment", envID, "error", err)
		return nil
	}
	return targets
}

// placementOf builds the body from one cluster read and the tasks. A task of a service the
// read does not list is left out: a removed service has no card to open.
func placementOf(read dockerx.ClusterRead, rows []dockerx.TaskRow, targets map[string]envs.NodeRef) PlacementBody {
	body := PlacementBody{
		Nodes:    make([]PlacementNode, 0, len(read.Nodes)),
		Tasks:    make([]PlacementTask, 0, len(rows)),
		Services: make([]PlacementService, 0, len(read.Services)),
	}
	nodeState := make(map[string]string, len(read.Nodes))
	for _, n := range read.Nodes {
		nodeState[n.ID] = n.State
		pn := PlacementNode{
			ID: n.ID, Hostname: cleanTaskText(n.Hostname), Role: n.Role, Leader: n.Leader, Availability: n.Availability, State: n.State,
			Cpus: float64(n.NanoCPUs) / 1e9, MemoryBytes: n.MemoryBytes, Address: n.Address, OS: n.OS, Arch: n.Arch,
			Labels: n.Labels, EngineLabels: n.EngineLabels,
		}
		if ref, ok := targets[n.ID]; ok {
			pn.TargetNodeID = ref.ID
		}
		body.Nodes = append(body.Nodes, pn)
	}
	byID := make(map[string]dockerx.ServiceRow, len(read.Services))
	for _, s := range read.Services {
		byID[s.ID] = s
		body.Services = append(body.Services, PlacementService{
			ID: s.ID, Name: s.Name, Stack: s.Stack, Mode: s.Mode, Constraints: s.Constraints, Preferences: s.Preferences, MaxReplicas: s.MaxReplicas,
		})
	}
	for _, r := range rows {
		svc, ok := byID[r.ServiceID]
		if !ok {
			continue
		}
		state, listed := nodeState[r.NodeID]
		body.Tasks = append(body.Tasks, PlacementTask{
			ID: r.ID, ServiceID: svc.ID, Service: svc.Name, Stack: svc.Stack, Slot: r.Slot,
			NodeID: r.NodeID, NodeKnown: r.NodeID == "" || listed,
			State: effectiveTaskState(r, state), DockerState: r.State, DesiredState: r.DesiredState,
			Error: cleanTaskText(r.Err),
		})
	}
	slices.SortStableFunc(body.Tasks, func(a, b PlacementTask) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.ID, b.ID))
	})
	return body
}
