// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

const (
	problemNodeNotFound = "node_not_found"

	// The values of the nodes list's status filter (UI-SPEC "Nodes" chips).
	nodeStatusManagers = "managers"
	nodeStatusWorkers  = "workers"
	nodeStatusDrained  = "drained"
	nodeStatusDown     = "down"

	// maxEarlierNodeTasks is how many tasks that are no longer meant to run a node's task
	// list adds to the ones that are.
	maxEarlierNodeTasks = 50
)

// nodeStatuses are the values status= takes.
var nodeStatuses = []string{nodeStatusManagers, nodeStatusWorkers, nodeStatusDrained, nodeStatusDown}

// NodesDeps are the dependencies of the swarm node routes.
type NodesDeps struct {
	// Aggregator finds the node TierPlan's own container is on, and maps swarm nodes to the
	// agents that reach them.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the routes are served from.
	Clusters *envs.Clusters
	Audit    *audit.Writer
	Checker  *rbac.Checker
	// Agents revokes the agent of a node that is removed.
	Agents *agents.Service
	// Live is where an agent's version and last sign of life are read. Nil means none is
	// known.
	Live AgentSessions
	// SelfContainerID is TierPlan's own container ID; empty when it cannot tell.
	SelfContainerID string
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// services is the part of the deps the shared swarm read helpers take.
func (d NodesDeps) services() ServicesDeps {
	return ServicesDeps{Aggregator: d.Aggregator, Clusters: d.Clusters, Audit: d.Audit, Checker: d.Checker, Log: d.Log}
}

// NodeItem is one swarm node as the list shows it.
type NodeItem struct {
	ID              string  `json:"id" doc:"The swarm node ID"`
	Hostname        string  `json:"hostname" doc:"The node's host name"`
	Role            string  `json:"role" enum:"manager,worker" doc:"The node's role"`
	Leader          bool    `json:"leader" doc:"True for the manager that leads the swarm"`
	RunsTierPlan    bool    `json:"runsTierPlan" doc:"True for the node whose engine hosts TierPlan's own container (or, on the local socket, the node the socket is)"`
	State           string  `json:"state" doc:"The swarm's word for the node: ready, down, disconnected or unknown"`
	Availability    string  `json:"availability" enum:"active,pause,drain" doc:"What the node's spec lets the swarm do with it"`
	Reachability    string  `json:"reachability" doc:"A manager's reachability (reachable, unreachable or unknown); empty for a worker"`
	Address         string  `json:"address" doc:"The address the swarm reaches the node at; empty when unknown"`
	EngineVersion   string  `json:"engineVersion" doc:"The Docker Engine version the node reports"`
	OS              string  `json:"os" doc:"The node's operating system (node.platform.os)"`
	Arch            string  `json:"arch" doc:"The node's architecture as the engine reports it (x86_64, aarch64)"`
	Cpus            float64 `json:"cpus" doc:"The CPUs the node reports; 0 when it reports none"`
	MemoryBytes     int64   `json:"memoryBytes" doc:"The memory the node reports, in bytes; 0 when it reports none"`
	TasksRunning    int     `json:"tasksRunning" doc:"Tasks whose desired state is running that are running on the node. 0 when tasksKnown is false"`
	TasksNotRunning int     `json:"tasksNotRunning" doc:"Tasks whose desired state is running that are not: still starting, failed to place, or on a node that is not ready and so cannot say. 0 when tasksKnown is false"`
	AgentReporting  bool    `json:"agentReporting" doc:"True when TierPlan reaches the node now: a connected agent runs on it, or it is the local socket's own node"`
	TargetNodeID    string  `json:"targetNodeId" doc:"The node ID the container routes of this environment take (?node= of the container page) for this node; empty when nothing reaches it now"`
}

// NodeStatusCountsBody is how many nodes each status chip counts.
type NodeStatusCountsBody struct {
	Managers int `json:"managers" doc:"Nodes whose role is manager"`
	Workers  int `json:"workers" doc:"Nodes whose role is worker"`
	Drained  int `json:"drained" doc:"Nodes whose availability is drain"`
	Down     int `json:"down" doc:"Nodes whose state is down or disconnected"`
}

// NodeListBody is the answer of the nodes list.
type NodeListBody struct {
	Items           []NodeItem           `json:"items" doc:"The nodes that match the search and the status filter, sorted by host name then ID"`
	Total           int                  `json:"total" doc:"How many nodes match the search and the status filter"`
	UnfilteredTotal int                  `json:"unfilteredTotal" doc:"How many nodes the swarm has before the search and the status filter"`
	Counts          NodeStatusCountsBody `json:"counts" doc:"How many nodes each status chip counts, before the search and the status filter, so the numbers never shrink with what was typed"`
	TasksKnown      bool                 `json:"tasksKnown" doc:"False when the tasks could not be read (no manager is connected, or the read failed): tasksRunning and tasksNotRunning are then 0 and mean nothing"`
	NoManager       bool                 `json:"noManager" doc:"True when no manager of the swarm is connected: the rows are the last ones read, or none"`
	FetchedAt       string               `json:"fetchedAt" format:"date-time" doc:"When the rows were read from a manager; empty when none were"`
}

// NodeListOutput wraps NodeListBody for huma.
type NodeListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeListBody
}

// NodesListInput is the path of the nodes list with its search and status filter.
type NodesListInput struct {
	EnvironmentPathInput
	Q      string `query:"q" doc:"Search: a case-insensitive substring of the host name, the address or any label key or value (of the node's spec or of its engine), at most 128 characters (else 422 query_too_long)"`
	Status string `query:"status" maxLength:"64" doc:"Only nodes in one of these statuses, comma separated: managers, workers, drained (availability drain) or down (state down or disconnected). Any other value is refused 422 status_invalid"`
}

// NodeInput is the path of one node.
type NodeInput struct {
	EnvironmentPathInput
	NodeID string `path:"nodeId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The swarm node ID: the id of a row of the nodes list"`
}

// NodeAgentBody is what TierPlan knows of the agent that runs on a node.
type NodeAgentBody struct {
	Reporting bool   `json:"reporting" doc:"True when TierPlan reaches the node now: a connected agent runs on it, or it is the local socket's own node"`
	Version   string `json:"version" doc:"The agent's version; empty when no agent is connected there and for the local socket's node"`
	LastSeen  string `json:"lastSeen" format:"date-time" doc:"The agent's last sign of life this server run; empty when none"`
}

// NodeDetailBody is one swarm node.
type NodeDetailBody struct {
	ID            string            `json:"id" doc:"The swarm node ID"`
	Version       uint64            `json:"version" doc:"The node's object version, which a change to the node must send back"`
	Hostname      string            `json:"hostname" doc:"The node's host name"`
	Role          string            `json:"role" enum:"manager,worker" doc:"The node's role"`
	Leader        bool              `json:"leader" doc:"True for the manager that leads the swarm"`
	RunsTierPlan  bool              `json:"runsTierPlan" doc:"True for the node whose engine hosts TierPlan's own container (or, on the local socket, the node the socket is)"`
	State         string            `json:"state" doc:"The swarm's word for the node: ready, down, disconnected or unknown"`
	Availability  string            `json:"availability" enum:"active,pause,drain" doc:"What the node's spec lets the swarm do with it"`
	Reachability  string            `json:"reachability" doc:"A manager's reachability (reachable, unreachable or unknown); empty for a worker"`
	Address       string            `json:"address" doc:"The address the swarm reaches the node at; empty when unknown"`
	ManagerAddr   string            `json:"managerAddr" doc:"The address a manager listens on for the swarm; empty for a worker"`
	EngineVersion string            `json:"engineVersion" doc:"The Docker Engine version the node reports"`
	OS            string            `json:"os" doc:"The node's operating system"`
	Arch          string            `json:"arch" doc:"The node's architecture as the engine reports it (x86_64, aarch64)"`
	Cpus          float64           `json:"cpus" doc:"The CPUs the node reports; 0 when it reports none"`
	MemoryBytes   int64             `json:"memoryBytes" doc:"The memory the node reports, in bytes; 0 when it reports none"`
	Labels        map[string]string `json:"labels" doc:"The node's labels (node.labels.*), which can be changed; an empty object when it has none"`
	EngineLabels  map[string]string `json:"engineLabels" doc:"The labels of the node's engine (engine.labels.*), set in its daemon settings; an empty object when it has none"`
	CreatedAt     string            `json:"createdAt" format:"date-time" doc:"When the node joined the swarm"`
	UpdatedAt     string            `json:"updatedAt" format:"date-time" doc:"When the node last changed"`
	Agent         NodeAgentBody     `json:"agent" doc:"The TierPlan agent on the node"`
	TargetNodeID  string            `json:"targetNodeId" doc:"The node ID the container routes of this environment take (?node= of the container page) for this node; empty when nothing reaches it now"`
}

// NodeDetailOutput wraps NodeDetailBody for huma.
type NodeDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeDetailBody
}

// NodeInspectBody is the raw inspect document of one node.
type NodeInspectBody struct {
	Document map[string]any `json:"document" doc:"The manager's inspect document of the node: its spec, description, status and manager status, with the TLS trust root and certificate subject of its description"`
}

// NodeInspectOutput wraps NodeInspectBody for huma.
type NodeInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeInspectBody
}

// NodeTaskItem is one task of a node.
type NodeTaskItem struct {
	TaskItem
	ServiceID string `json:"serviceId" doc:"The service the task belongs to"`
	Service   string `json:"service" doc:"The service's name; empty when the swarm no longer lists the service"`
}

// NodeTasksBody is the tasks of one node.
type NodeTasksBody struct {
	Items  []NodeTaskItem `json:"items" doc:"The tasks whose desired state is running, newest first, then the 50 newest others, newest first"`
	Total  int            `json:"total" doc:"Every task the swarm holds for the node, current and earlier"`
	Capped bool           `json:"capped" doc:"True when earlier tasks were left out of items"`
}

// NodeTasksOutput wraps NodeTasksBody for huma.
type NodeTasksOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeTasksBody
}

// RegisterNodes mounts the swarm node routes: the reads, and the impact, availability and role
// routes that change a node (nodeactions.go).
func RegisterNodes(r *httpx.Router, d NodesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	need := rbac.Requirement{Perm: rbac.PermNodesRead, Scope: rbac.ScopeEnvironment}
	const swarmErrors = "409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. Needs nodes.read (Viewer) on the environment."

	httpx.Register(r, huma.Operation{
		OperationID: "listNodes", Method: http.MethodGet, Path: "/api/env/{envId}/nodes",
		Summary: "List the nodes of a swarm",
		Description: "Every node of a swarm with the facts the Nodes list shows: role, leader, state, availability, address, engine, platform, CPUs, memory, how many tasks run there and whether a TierPlan agent reports from it, " +
			"read through a manager's agent or through the local socket of a swarm manager, sorted by host name then ID. runsTierPlan marks the node whose engine hosts TierPlan's own container. " +
			"Optionally a search (q) and a status filter (status: managers, workers, drained, down); counts are the chip numbers before them. When no manager is connected the answer is 200 with noManager true and the rows last read, or none. " +
			"A task counts as running only on a node that is ready. " + swarmErrors,
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *NodesListInput) (*NodeListOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		body, err := listNodes(ctx, d, env.ID, in)
		if err != nil {
			return nil, err
		}
		return &NodeListOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getNode", Method: http.MethodGet, Path: "/api/env/{envId}/nodes/{nodeId}",
		Summary: "One swarm node",
		Description: "The facts of one node read live from a manager: its object version, role, state, availability, manager status, platform, resources, labels and engine labels, when it was created and last changed, and the TierPlan agent on it (whether it reports, its version, when it was last seen). " +
			"404 node_not_found when the swarm has no such node. " + swarmErrors,
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *NodeInput) (*NodeDetailOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		body, err := getNode(ctx, d, env.ID, in.NodeID)
		if err != nil {
			return nil, err
		}
		return &NodeDetailOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getNodeInspect", Method: http.MethodGet, Path: "/api/env/{envId}/nodes/{nodeId}/inspect",
		Summary: "The raw inspect document of a swarm node",
		Description: "The manager's inspect document of the node, for the Inspect tab. It holds the TLS trust root of the swarm and the node's certificate subject, which are public, and no join token. " +
			"404 node_not_found when the swarm has no such node. " + swarmErrors,
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *NodeInput) (*NodeInspectOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		body, err := getNodeInspect(ctx, d, env.ID, in.NodeID)
		if err != nil {
			return nil, err
		}
		return &NodeInspectOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listNodeTasks", Method: http.MethodGet, Path: "/api/env/{envId}/nodes/{nodeId}/tasks",
		Summary: "The tasks of a swarm node",
		Description: "The tasks the swarm holds for one node: those whose desired state is running first, newest first, then the 50 newest others, each with its service's name. " +
			"A task that has not ended on a node that is not ready reads unknown, never running; dockerState keeps Docker's own word. Docker's error and message text is cleaned of control characters and is to be shown as plain text. " +
			"404 node_not_found when the swarm has no such node. " + swarmErrors,
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *NodeInput) (*NodeTasksOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		body, err := listNodeTasks(ctx, d, env.ID, in.NodeID)
		if err != nil {
			return nil, err
		}
		return &NodeTasksOutput{CacheControl: "no-store", Body: body}, nil
	})

	registerNodeActions(r, d)
	registerNodeLabels(r, d)
}

// parseNodeStatuses reads the comma list of status=. An empty list means no filter and an
// unknown value is a 422.
func parseNodeStatuses(raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !slices.Contains(nodeStatuses, part) {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStatusInvalid,
				"status "+strconv.Quote(part)+" is not one of "+strings.Join(nodeStatuses, ", ")+".")
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out, nil
}

// nodeIsDown reports whether the swarm cannot reach the node: down and disconnected are the
// two states it shows as offline.
func nodeIsDown(state string) bool { return state == "down" || state == "disconnected" }

// nodeHasStatus reports whether a node is counted by one status chip.
func nodeHasStatus(n dockerx.NodeRow, status string) bool {
	switch status {
	case nodeStatusManagers:
		return n.Role == "manager"
	case nodeStatusWorkers:
		return n.Role == "worker"
	case nodeStatusDrained:
		return n.Availability == "drain"
	case nodeStatusDown:
		return nodeIsDown(n.State)
	}
	return false
}

// nodeCounts counts the rows by chip. It is called on every row, before the search and the
// status filter.
func nodeCounts(rows []dockerx.NodeRow) NodeStatusCountsBody {
	var c NodeStatusCountsBody
	for _, n := range rows {
		if nodeHasStatus(n, nodeStatusManagers) {
			c.Managers++
		}
		if nodeHasStatus(n, nodeStatusWorkers) {
			c.Workers++
		}
		if nodeHasStatus(n, nodeStatusDrained) {
			c.Drained++
		}
		if nodeHasStatus(n, nodeStatusDown) {
			c.Down++
		}
	}
	return c
}

// nodeMatchesQuery says whether q (already lower-cased) occurs in the host name, the address
// or any label key or value of the node or of its engine. An empty q matches every node.
func nodeMatchesQuery(n dockerx.NodeRow, q string) bool {
	if q == "" {
		return true
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	if has(n.Hostname) || has(n.Address) {
		return true
	}
	for _, labels := range []map[string]string{n.Labels, n.EngineLabels} {
		for k, v := range labels {
			if has(k) || has(v) {
				return true
			}
		}
	}
	return false
}

// filterNodes keeps the rows that match q and any of statuses, in their order.
func filterNodes(rows []dockerx.NodeRow, q string, statuses []string) []dockerx.NodeRow {
	if q == "" && len(statuses) == 0 {
		return rows
	}
	out := make([]dockerx.NodeRow, 0, len(rows))
	for _, n := range rows {
		if !nodeMatchesQuery(n, q) {
			continue
		}
		if len(statuses) > 0 && !slices.ContainsFunc(statuses, func(s string) bool { return nodeHasStatus(n, s) }) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// tierplanNode is the swarm node ID of the node whose engine hosts TierPlan, or empty when
// that is not known. The local socket, and the host folded into a swarm environment, are
// TierPlan's own host, so the node they reach is it. Otherwise it is the node of the
// container that is TierPlan's own, found in the environment's containers.
func tierplanNode(ctx context.Context, d NodesDeps, envID string, targets map[string]envs.NodeRef) string {
	for swarmID, ref := range targets {
		if ref.ID == envID || ref.ID == envs.SocketTarget(envID) {
			return swarmID
		}
	}
	if len(d.SelfContainerID) < minSelfIDLen || d.Aggregator == nil {
		return ""
	}
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	agg, _, err := d.Aggregator.Snapshot(readCtx, envID)
	if err != nil {
		d.Log.Debug("looking for TierPlan's own container failed", "environment", envID, "error", err)
		return ""
	}
	for _, c := range agg.Containers {
		if !isSelfContainer(d.SelfContainerID, c.ID) {
			continue
		}
		for swarmID, ref := range targets {
			if ref.ID == c.NodeID {
				return swarmID
			}
		}
	}
	return ""
}

// nodeTaskCounts counts, per node, the tasks meant to run that are running and those that
// are not. It reads once from a manager; a read that fails answers known false.
func nodeTaskCounts(ctx context.Context, d NodesDeps, envID string, nodes []dockerx.NodeRow) (running, notRunning map[string]int, known bool) {
	var rows []dockerx.TaskRow
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.Tasks(readCtx, dockerx.TaskFilter{DesiredState: "running"})
		return e
	})
	if err != nil {
		d.Log.Warn("reading the tasks of the nodes failed", "environment", envID, "error", err)
		return nil, nil, false
	}
	state := make(map[string]string, len(nodes))
	for _, n := range nodes {
		state[n.ID] = n.State
	}
	running, notRunning = map[string]int{}, map[string]int{}
	for _, r := range rows {
		st, listed := state[r.NodeID]
		if r.NodeID == "" || !listed {
			continue
		}
		if effectiveTaskState(r, st) == "running" {
			running[r.NodeID]++
		} else {
			notRunning[r.NodeID]++
		}
	}
	return running, notRunning, true
}

// listNodes answers the nodes of a swarm from the cluster reader.
func listNodes(ctx context.Context, d NodesDeps, envID string, in *NodesListInput) (NodeListBody, error) {
	if utf8.RuneCountInString(in.Q) > maxServiceQuery {
		return NodeListBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemQueryTooLong,
			"The search can be at most "+strconv.Itoa(maxServiceQuery)+" characters.")
	}
	statuses, err := parseNodeStatuses(in.Status)
	if err != nil {
		return NodeListBody{}, err
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return NodeListBody{}, err
	}
	st, err := d.Clusters.State(ctx, envID)
	noManager := false
	if err != nil {
		switch {
		case errors.Is(err, envs.ErrNoManager), errors.Is(err, envs.ErrOffline), envs.ReasonFor(err) == envs.ReasonNotAManager:
			noManager = true
		case st.FetchedAt.IsZero():
			// Nothing was ever read and the read failed for another reason.
			return NodeListBody{}, snapshotProblem(d.Log, envID, err)
		}
		// Otherwise the last read is served, with its age in fetchedAt.
	}
	all := st.Read.Nodes
	rows := filterNodes(all, strings.ToLower(strings.TrimSpace(in.Q)), statuses)
	body := NodeListBody{
		Items: make([]NodeItem, 0, len(rows)), Total: len(rows), UnfilteredTotal: len(all),
		Counts: nodeCounts(all), NoManager: noManager, FetchedAt: rfc3339(st.FetchedAt),
	}
	var running, notRunning map[string]int
	if !noManager {
		running, notRunning, body.TasksKnown = nodeTaskCounts(ctx, d, envID, all)
	}
	targets := swarmNodeTargets(ctx, d.services(), envID)
	runs := tierplanNode(ctx, d, envID, targets)
	for _, n := range rows {
		ref, reporting := targets[n.ID]
		body.Items = append(body.Items, NodeItem{
			ID: n.ID, Hostname: cleanTaskText(n.Hostname), Role: n.Role, Leader: n.Leader, RunsTierPlan: n.ID == runs,
			State: n.State, Availability: n.Availability, Reachability: n.Reachability, Address: n.Address,
			EngineVersion: n.EngineVersion, OS: n.OS, Arch: n.Arch, Cpus: float64(n.NanoCPUs) / 1e9, MemoryBytes: n.MemoryBytes,
			TasksRunning: running[n.ID], TasksNotRunning: notRunning[n.ID], AgentReporting: reporting, TargetNodeID: ref.ID,
		})
	}
	return body, nil
}

// nodeReadProblem turns the error of a cluster read of one node into the problem the node
// routes answer with.
func nodeReadProblem(d NodesDeps, envID string, err error) error {
	if errors.Is(err, dockerx.ErrNodeNotFound) {
		return nodeNotFound()
	}
	return serviceReadProblem(d.services(), envID, err)
}

func nodeNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemNodeNotFound, "This node does not exist. It may have left the swarm or been removed.")
}

// readNode reads one node from a manager. The errors are already problems.
func readNode(ctx context.Context, d NodesDeps, envID, nodeID string) (dockerx.NodeDetail, []byte, error) {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return dockerx.NodeDetail{}, nil, err
	}
	var (
		detail dockerx.NodeDetail
		raw    []byte
	)
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		detail, raw, e = c.NodeInspect(readCtx, nodeID)
		return e
	})
	if err != nil {
		return dockerx.NodeDetail{}, nil, nodeReadProblem(d, envID, err)
	}
	return detail, raw, nil
}

// getNode answers one node, with the agent that reports from it.
func getNode(ctx context.Context, d NodesDeps, envID, nodeID string) (NodeDetailBody, error) {
	n, _, err := readNode(ctx, d, envID, nodeID)
	if err != nil {
		return NodeDetailBody{}, err
	}
	targets := swarmNodeTargets(ctx, d.services(), envID)
	ref, reporting := targets[n.ID]
	b := NodeDetailBody{
		ID: n.ID, Version: n.Version, Hostname: cleanTaskText(n.Hostname), Role: n.Role, Leader: n.Leader,
		RunsTierPlan: n.ID == tierplanNode(ctx, d, envID, targets),
		State:        n.State, Availability: n.Availability, Reachability: n.Reachability, Address: n.Address, ManagerAddr: n.ManagerAddr,
		EngineVersion: n.EngineVersion, OS: n.OS, Arch: n.Arch, Cpus: float64(n.NanoCPUs) / 1e9, MemoryBytes: n.MemoryBytes,
		Labels: n.Labels, EngineLabels: n.EngineLabels, CreatedAt: rfc3339(n.CreatedAt), UpdatedAt: rfc3339(n.UpdatedAt),
		Agent: NodeAgentBody{Reporting: reporting}, TargetNodeID: ref.ID,
	}
	if reporting && d.Live != nil {
		for _, v := range d.Live.Views(envID) {
			if v.AgentID == ref.ID {
				b.Agent.Version, b.Agent.LastSeen = v.Version, rfc3339(v.LastSeen)
			}
		}
	}
	return b, nil
}

// getNodeInspect answers the raw inspect document of one node.
func getNodeInspect(ctx context.Context, d NodesDeps, envID, nodeID string) (NodeInspectBody, error) {
	_, raw, err := readNode(ctx, d, envID, nodeID)
	if err != nil {
		return NodeInspectBody{}, err
	}
	doc, err := decodeInspect(raw)
	if err != nil {
		d.Log.Error("reading a node inspect failed", "environment", envID, "error", err)
		return NodeInspectBody{}, huma.Error500InternalServerError("internal error")
	}
	return NodeInspectBody{Document: doc}, nil
}

// listNodeTasks reads the node list, the service list and the node's tasks in one cluster
// read. Nothing is written.
func listNodeTasks(ctx context.Context, d NodesDeps, envID, nodeID string) (NodeTasksBody, error) {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return NodeTasksBody{}, err
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
		if !slices.ContainsFunc(read.Nodes, func(n dockerx.NodeRow) bool { return n.ID == nodeID }) {
			return dockerx.ErrNodeNotFound
		}
		rows, e = c.Tasks(readCtx, dockerx.TaskFilter{NodeIDs: []string{nodeID}})
		return e
	})
	if err != nil {
		return NodeTasksBody{}, nodeReadProblem(d, envID, err)
	}

	nodeOf := make(map[string]dockerx.NodeRow, len(read.Nodes))
	for _, n := range read.Nodes {
		nodeOf[n.ID] = n
	}
	serviceName := make(map[string]string, len(read.Services))
	for _, s := range read.Services {
		serviceName[s.ID] = s.Name
	}
	targets := swarmNodeTargets(ctx, d.services(), envID)

	// The rows are newest first. The tasks meant to run come first, then the earlier ones, cut
	// to the newest few.
	var current, earlier []dockerx.TaskRow
	for _, r := range rows {
		if r.NodeID != nodeID {
			continue
		}
		if r.DesiredState == "running" {
			current = append(current, r)
		} else {
			earlier = append(earlier, r)
		}
	}
	body := NodeTasksBody{Total: len(current) + len(earlier), Capped: len(earlier) > maxEarlierNodeTasks}
	shown := append(current, earlier[:min(len(earlier), maxEarlierNodeTasks)]...)
	body.Items = make([]NodeTaskItem, 0, len(shown))
	for _, r := range shown {
		body.Items = append(body.Items, NodeTaskItem{
			TaskItem: taskItem(r, nodeOf, targets), ServiceID: r.ServiceID, Service: cleanTaskText(serviceName[r.ServiceID]),
		})
	}
	return body, nil
}
