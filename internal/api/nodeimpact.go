// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// The changes a node impact is asked for. Activate has no entry: it sends nothing away and needs
// no dialog, so the server judges it as a change with no impact.
const (
	impactDrain   = "drain"
	impactPause   = "pause"
	impactPromote = "promote"
	impactDemote  = "demote"
)

// What a drain does to one task of the node.
const (
	taskMoves       = "moves"
	taskStops       = "stops"
	taskCannotPlace = "cannot_place"
	taskStays       = "stays"
)

// The warnings a node change can carry.
const (
	warnQuorumAtRisk     = "quorum_at_risk"
	warnEvenCount        = "even_count"
	warnRunsBayplan      = "runs_bayplan"
	warnOnlyManagerAgent = "only_manager_agent"
	warnFactsUnreadable  = "facts_unreadable"
)

// NodeImpactTask is one task of the node and what the change does to it.
type NodeImpactTask struct {
	ID        string `json:"id" doc:"The task ID"`
	ServiceID string `json:"serviceId" doc:"The service the task belongs to"`
	Service   string `json:"service" doc:"The service's name; its ID when the swarm no longer lists the service"`
	Slot      int    `json:"slot" doc:"The replica number of a replicated service's task; 0 for a global service's"`
	Outcome   string `json:"outcome" enum:"moves,stops,cannot_place,stays" doc:"What happens to it: moves (another active, ready node meets the service's constraints), stops (a global service's task is not moved), cannot_place (no other node can take it) or stays (a pause leaves it running)"`
}

// NodeImpactManagers are the manager counts the change is judged on.
type NodeImpactManagers struct {
	Total     int `json:"total" doc:"Nodes whose role is manager now"`
	Reachable int `json:"reachable" doc:"Managers whose reachability is reachable now"`
	After     int `json:"after" doc:"Managers there would be after the change"`
}

// NodeImpactBody is what a change to one node will do.
type NodeImpactBody struct {
	Action       string             `json:"action" enum:"drain,pause,promote,demote,activate" doc:"The change judged"`
	Hostname     string             `json:"hostname" doc:"The node's host name, which is what is typed to confirm"`
	Version      uint64             `json:"version" doc:"The node's object version as read for this judgement: the version a change made on the strength of it must send back"`
	Tasks        []NodeImpactTask   `json:"tasks" doc:"For drain and pause, the node's tasks whose desired state is running, by service then slot; empty otherwise"`
	Managers     NodeImpactManagers `json:"managers" doc:"The manager counts"`
	Warnings     []string           `json:"warnings" doc:"What to warn of: quorum_at_risk (a demote could leave no majority), even_count (a demote leaves an even number of managers), runs_bayplan (the node hosts Bayplan), only_manager_agent (it has the only manager agent connected to Bayplan), facts_unreadable (a manager's state is not known, so the first cannot be judged). Never null"`
	NeedsConfirm bool               `json:"needsConfirm" doc:"True when the node's name must be typed to make the change: a drain or demote of Bayplan's node or of the only manager agent, a demote that puts quorum at risk, and a demote whose quorum facts could not be read. The server refuses the change without it"`
}

// nodeImpactInput is everything nodeImpact judges.
type nodeImpactInput struct {
	Action string
	Node   dockerx.NodeRow
	// Version is the node's object version at the read.
	Version uint64
	// Nodes are every node of the swarm, the node included.
	Nodes []dockerx.NodeRow
	// Tasks are the node's tasks whose desired state is running.
	Tasks []dockerx.TaskRow
	// Services are the swarm's services by ID.
	Services                      map[string]dockerx.ServiceRow
	RunsBayplan, OnlyManagerAgent bool
}

// majority is how many of n managers make a quorum.
func majority(n int) int { return n/2 + 1 }

// quorumAtRisk reports whether demoting one reachable manager of total, of which reachable
// can be reached, could leave the swarm without a majority: the managers left that can be
// reached (reachable-1) must still be a majority of those left (total-1).
func quorumAtRisk(total, reachable int) bool {
	return reachable-1 < majority(total-1)
}

// nodeImpact judges one change to one node. It is the one place the dialog's warnings and the
// write's typed-name check come from.
func nodeImpact(in nodeImpactInput) NodeImpactBody {
	b := NodeImpactBody{Action: in.Action, Hostname: cleanTaskText(in.Node.Hostname), Version: in.Version, Tasks: []NodeImpactTask{}, Warnings: []string{}}

	unknown := 0
	for _, n := range in.Nodes {
		if n.Role != "manager" {
			continue
		}
		b.Managers.Total++
		switch n.Reachability {
		case "reachable":
			b.Managers.Reachable++
		case "unreachable":
		default:
			unknown++
		}
	}
	b.Managers.After = b.Managers.Total
	isManager := in.Node.Role == "manager"

	switch in.Action {
	case impactDrain, impactPause:
		b.Tasks = impactTasks(in)
		if in.Action == impactDrain {
			if in.RunsBayplan {
				b.Warnings = append(b.Warnings, warnRunsBayplan)
			}
			if in.OnlyManagerAgent {
				b.Warnings = append(b.Warnings, warnOnlyManagerAgent)
			}
			b.NeedsConfirm = len(b.Warnings) > 0
		}
	case impactPromote:
		if !isManager {
			b.Managers.After++
		}
	case impactDemote:
		if !isManager {
			break
		}
		b.Managers.After--
		// The node being demoted counts as one of the reachable managers it leaves behind
		// unless it is itself the one that cannot be reached.
		reachable := b.Managers.Reachable
		if in.Node.Reachability != "reachable" {
			reachable++
		}
		atRisk := quorumAtRisk(b.Managers.Total, reachable)
		if atRisk {
			b.Warnings = append(b.Warnings, warnQuorumAtRisk)
		}
		if b.Managers.After > 0 && b.Managers.After%2 == 0 {
			b.Warnings = append(b.Warnings, warnEvenCount)
		}
		if in.RunsBayplan {
			b.Warnings = append(b.Warnings, warnRunsBayplan)
		}
		if in.OnlyManagerAgent {
			b.Warnings = append(b.Warnings, warnOnlyManagerAgent)
		}
		if unknown > 0 {
			b.Warnings = append(b.Warnings, warnFactsUnreadable)
		}
		b.NeedsConfirm = atRisk || in.RunsBayplan || in.OnlyManagerAgent || unknown > 0
	}
	return b
}

// impactTasks says what the change does to each of the node's tasks, by service name, slot
// then task ID. A pause leaves every task running. A drain stops a global service's task, moves
// a replicated one when another active, ready node meets all the service's constraints, and
// otherwise cannot place it. A constraint that does not parse, or a service the read does not
// list, matches no node.
func impactTasks(in nodeImpactInput) []NodeImpactTask {
	var targets []envs.NodeFacts
	if in.Action == impactDrain {
		for _, n := range in.Nodes {
			if n.ID != in.Node.ID && n.Availability == "active" && n.State == "ready" {
				targets = append(targets, envs.NodeFactsOf(n))
			}
		}
	}
	canMove := map[string]bool{}
	movable := func(svc dockerx.ServiceRow) bool {
		if v, done := canMove[svc.ID]; done {
			return v
		}
		ok := slices.ContainsFunc(targets, func(f envs.NodeFacts) bool {
			matches, err := envs.MatchesAll(svc.Constraints, f)
			return err == nil && matches
		})
		canMove[svc.ID] = ok
		return ok
	}
	out := make([]NodeImpactTask, 0, len(in.Tasks))
	for _, tk := range in.Tasks {
		svc, listed := in.Services[tk.ServiceID]
		item := NodeImpactTask{ID: tk.ID, ServiceID: tk.ServiceID, Service: cleanTaskText(svc.Name), Slot: tk.Slot}
		if !listed {
			item.Service = tk.ServiceID
		}
		switch {
		case in.Action == impactPause:
			item.Outcome = taskStays
		case listed && (svc.Mode == "global" || svc.Mode == "global-job"):
			item.Outcome = taskStops
		case listed && movable(svc):
			item.Outcome = taskMoves
		default:
			item.Outcome = taskCannotPlace
		}
		out = append(out, item)
	}
	slices.SortStableFunc(out, func(a, b NodeImpactTask) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// onlyManagerAgent reports whether the node has the only manager connection Bayplan has to the
// swarm: one agent of a manager is connected, and it is the node's. The local socket is not an
// agent (Bayplan's own node is warned of as that), but the host folded into a swarm
// environment is a connection, so it counts as another.
func onlyManagerAgent(targets map[string]envs.NodeRef, envID, nodeID string) bool {
	count, theNodes := 0, false
	for swarmID, ref := range targets {
		if ref.ID == envID || (ref.Role != "manager" && ref.Role != dockerx.TypeSwarmManager) {
			continue
		}
		count++
		theNodes = theNodes || swarmID == nodeID
	}
	return count == 1 && theNodes
}

// gatherImpact reads what nodeImpact judges from a manager in one cluster read: the nodes, the
// services and, for a drain or a pause, the node's tasks that are meant to run. A node the swarm
// does not have is ErrNodeNotFound (already a problem through nodeReadProblem).
func gatherImpact(ctx context.Context, d NodesDeps, envID, nodeID, action string) (nodeImpactInput, error) {
	var (
		read    dockerx.ClusterRead
		tasks   []dockerx.TaskRow
		version uint64
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
		// The cluster read does not carry the object version a change must send back.
		detail, _, e := c.NodeInspect(readCtx, nodeID)
		if e != nil {
			return e
		}
		version = detail.Version
		if action == impactDrain || action == impactPause {
			tasks, e = c.Tasks(readCtx, dockerx.TaskFilter{NodeIDs: []string{nodeID}, DesiredState: "running"})
		}
		return e
	})
	if err != nil {
		return nodeImpactInput{}, err
	}
	in := nodeImpactInput{Action: action, Version: version, Nodes: read.Nodes, Services: make(map[string]dockerx.ServiceRow, len(read.Services))}
	for _, n := range read.Nodes {
		if n.ID == nodeID {
			in.Node = n
		}
	}
	for _, s := range read.Services {
		in.Services[s.ID] = s
	}
	for _, tk := range tasks {
		if tk.NodeID == nodeID {
			in.Tasks = append(in.Tasks, tk)
		}
	}
	targets := swarmNodeTargets(ctx, d.services(), envID)
	in.RunsBayplan = bayplanNode(ctx, d, envID, targets) == nodeID
	in.OnlyManagerAgent = onlyManagerAgent(targets, envID, nodeID)
	return in, nil
}

// NodeImpactInput is the path of a node's impact with the change asked about.
type NodeImpactInput struct {
	NodeInput
	Action string `query:"action" required:"true" enum:"drain,pause,promote,demote" doc:"The change to judge: drain, pause, promote or demote"`
}

// NodeImpactOutput wraps NodeImpactBody for huma.
type NodeImpactOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeImpactBody
}

// registerNodeImpact mounts the impact route.
func registerNodeImpact(r *httpx.Router, d NodesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getNodeActionImpact", Method: http.MethodGet, Path: "/api/env/{envId}/nodes/{nodeId}/impact",
		Summary: "What a change to a swarm node will do",
		Description: "Judges a drain, pause, promote or demote of one node from a live read of the swarm, and is the same judgement the change itself enforces. " +
			"For a drain or a pause it lists the node's tasks that are meant to run with what happens to each: moves (another active, ready node meets the service's placement constraints), stops (a global service's task is not moved), cannot_place, or stays (a pause). " +
			"managers gives the manager counts now and after the change. warnings are quorum_at_risk, even_count, runs_bayplan, only_manager_agent and facts_unreadable, and needsConfirm says the node's name must be typed for the change to be accepted. " +
			"404 node_not_found when the swarm has no such node; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. Needs nodes.manage (Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNodesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NodeImpactInput) (*NodeImpactOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getNodeActionImpact(ctx, d, env.ID, in.NodeID, in.Action)
			if err != nil {
				return nil, err
			}
			return &NodeImpactOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// getNodeActionImpact answers the impact of one change to one node. Nothing is written.
func getNodeActionImpact(ctx context.Context, d NodesDeps, envID, nodeID, action string) (NodeImpactBody, error) {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return NodeImpactBody{}, err
	}
	in, err := gatherImpact(ctx, d, envID, nodeID, action)
	if err != nil {
		return NodeImpactBody{}, nodeReadProblem(d, envID, err)
	}
	return nodeImpact(in), nil
}
