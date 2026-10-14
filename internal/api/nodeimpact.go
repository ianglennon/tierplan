// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"github.com/ianglennon/bayplan/internal/dockerx"
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
	Service   string `json:"service" doc:"The service's name"`
	Slot      int    `json:"slot" doc:"The replica number of a replicated service's task; 0 for a global service's"`
	Outcome   string `json:"outcome" enum:"moves,stops,cannot_place,stays" doc:"What happens to it"`
}

// NodeImpactManagers are the manager counts the change is judged on.
type NodeImpactManagers struct {
	Total     int `json:"total" doc:"Nodes whose role is manager now"`
	Reachable int `json:"reachable" doc:"Managers whose reachability is reachable now"`
	After     int `json:"after" doc:"Managers there would be after the change"`
}

// NodeImpactBody is what a change to one node will do.
type NodeImpactBody struct {
	Action       string             `json:"action" enum:"drain,pause,promote,demote" doc:"The change judged"`
	Hostname     string             `json:"hostname" doc:"The node's host name"`
	Tasks        []NodeImpactTask   `json:"tasks" doc:"For drain and pause, the node's tasks whose desired state is running; empty otherwise"`
	Managers     NodeImpactManagers `json:"managers" doc:"The manager counts"`
	Warnings     []string           `json:"warnings" doc:"Codes: quorum_at_risk, even_count, runs_bayplan, only_manager_agent, facts_unreadable"`
	NeedsConfirm bool               `json:"needsConfirm" doc:"True when the node's name must be typed to make the change"`
}

// nodeImpactInput is everything nodeImpact judges.
type nodeImpactInput struct {
	Action string
	Node   dockerx.NodeRow
	// Nodes are every node of the swarm, the node included.
	Nodes []dockerx.NodeRow
	// Tasks are the node's tasks whose desired state is running.
	Tasks []dockerx.TaskRow
	// Services are the swarm's services by ID.
	Services                      map[string]dockerx.ServiceRow
	RunsBayplan, OnlyManagerAgent bool
}

// quorumAtRisk reports whether demoting one reachable manager of total, of which reachable
// can be reached, could leave the swarm without a majority.
func quorumAtRisk(total, reachable int) bool { return false }

// nodeImpact judges one change to one node.
func nodeImpact(in nodeImpactInput) NodeImpactBody { return NodeImpactBody{} }
