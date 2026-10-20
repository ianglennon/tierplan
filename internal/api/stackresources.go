// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackResourceTaskBody is the current task of one slot of a swarm stack's service.
type StackResourceTaskBody struct {
	Task      TaskItem `json:"task" doc:"The task, as the service's Tasks tab shows it"`
	ServiceID string   `json:"serviceId" doc:"The swarm service ID, which the service page is at"`
	Service   string   `json:"service" doc:"The service's name in the compose file, without the stack prefix"`
	Global    bool     `json:"global" doc:"True for a global service, whose rows are nodes rather than slots"`
}

// StackTasksCardBody is the Tasks card of a swarm stack.
type StackTasksCardBody struct {
	Items []StackResourceTaskBody `json:"items" doc:"The current task of each slot of each service of the stack (of each node for a global service), by service then slot. The tasks Docker keeps in history are not listed"`
	Error string                  `json:"error" doc:"Empty when the card was read. Otherwise why it was not: no_manager, no_quorum, node_unavailable or read_failed; Items is then empty"`
}

// StackContainerResourceBody is one container of a standalone stack.
type StackContainerResourceBody struct {
	ID      string   `json:"id" doc:"The container ID, which the container page is at"`
	Name    string   `json:"name" doc:"The container name"`
	Service string   `json:"service" doc:"The service's name in the compose file"`
	State   string   `json:"state" doc:"The engine's word for the container: running, exited, restarting, paused, created, removing or dead"`
	Health  string   `json:"health" doc:"none, starting, healthy or unhealthy"`
	Image   string   `json:"image" doc:"The image the container runs, as the engine reports it"`
	Ports   []string `json:"ports" doc:"The published ports as published->target/protocol, without the host address"`
	Created string   `json:"created" format:"date-time" doc:"When the container was created"`
}

// StackContainersCardBody is the Containers card of a standalone stack.
type StackContainersCardBody struct {
	Items []StackContainerResourceBody `json:"items" doc:"The containers of the stack's Compose project, stopped ones too, one-off containers (compose run) left out, by name"`
	Error string                       `json:"error" doc:"Empty when the card was read. Otherwise why it was not: node_unavailable or read_failed; Items is then empty"`
}

// StackNetworkResourceBody is one network the stack owns or uses.
type StackNetworkResourceBody struct {
	ID        string `json:"id" doc:"The network ID, which the network page is at"`
	Name      string `json:"name" doc:"The network name"`
	Driver    string `json:"driver" doc:"The network driver"`
	Scope     string `json:"scope" doc:"swarm for a network of the whole cluster, local for one of a host"`
	Ownership string `json:"ownership" enum:"stack,external" doc:"stack: the network carries the stack's own labels. external: the compose file refers to a network created outside the stack, which Bayplan never changes or removes"`
}

// StackNetworksCardBody is the Networks card.
type StackNetworksCardBody struct {
	Items []StackNetworkResourceBody `json:"items" doc:"The networks of the stack, by name"`
	Error string                     `json:"error" doc:"Empty when the card was read. Otherwise why it was not: no_manager, no_quorum, node_unavailable or read_failed; Items is then empty"`
}

// StackResourcesBody is everything a stack owns on its environment, one card for each kind. Each card is read
// on its own and fails on its own.
type StackResourcesBody struct {
	Tasks      *StackTasksCardBody      `json:"tasks,omitempty" doc:"The Tasks card; present for a swarm stack"`
	Containers *StackContainersCardBody `json:"containers,omitempty" doc:"The Containers card; present for a standalone stack"`
	Networks   StackNetworksCardBody    `json:"networks" doc:"The Networks card"`
}

// StackResourcesOutput wraps StackResourcesBody for huma.
type StackResourcesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackResourcesBody
}

func registerStackResources(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listStackResources", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/resources",
		Summary: "What a stack owns on its environment",
		Description: "The tasks (a swarm) or containers (a standalone host) and the networks of the stack, found by the stack's labels: its swarm namespace or Compose project together with its Bayplan ID, so an object of another stack, or one somebody made under the same name, is never listed as the stack's. " +
			"Each card is read on its own and carries its own error, so one read that fails never hides the others. " +
			"A stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackResourcesOutput, error) {
			return listStackResources(ctx, d, in)
		})
}

func listStackResources(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackResourcesOutput, error) {
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if errors.Is(err, stackops.ErrStackNotFound) {
		return nil, stackNotFound()
	}
	if err != nil {
		return nil, stackInternal(d, "read", in.EnvID, err)
	}
	decl, err := d.repo().Declared(ctx, st)
	if err != nil {
		return nil, stackInternal(d, "read", in.EnvID, err)
	}
	res := d.ops().Resources(ctx, in.EnvID, st, decl)
	body := StackResourcesBody{Networks: StackNetworksCardBody{Items: make([]StackNetworkResourceBody, 0, len(res.Networks.Items)), Error: res.Networks.Error}}
	for _, n := range res.Networks.Items {
		body.Networks.Items = append(body.Networks.Items, StackNetworkResourceBody{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Ownership: n.Ownership})
	}
	if st.Target == stacks.TargetSwarm {
		targets := map[string]envs.NodeRef(nil)
		if d.Aggregator != nil && len(res.Tasks.Items) > 0 {
			var terr error
			if targets, terr = d.Aggregator.SwarmNodeTargets(ctx, in.EnvID); terr != nil {
				// A node nothing reaches is a task that cannot be opened, not a failed read.
				d.Log.Warn("mapping swarm nodes to agents failed", "environment", in.EnvID, "error", terr)
				targets = nil
			}
		}
		card := StackTasksCardBody{Items: make([]StackResourceTaskBody, 0, len(res.Tasks.Items)), Error: res.Tasks.Error}
		for _, t := range res.Tasks.Items {
			nodes := map[string]dockerx.NodeRow{}
			if t.NodeListed {
				nodes[t.Task.NodeID] = t.Node
			}
			card.Items = append(card.Items, StackResourceTaskBody{
				Task: taskItem(t.Task, nodes, targets), ServiceID: t.ServiceID, Service: t.Service, Global: t.Global,
			})
		}
		body.Tasks = &card
	} else {
		card := StackContainersCardBody{Items: make([]StackContainerResourceBody, 0, len(res.Containers.Items)), Error: res.Containers.Error}
		for _, c := range res.Containers.Items {
			card.Items = append(card.Items, StackContainerResourceBody{
				ID: c.ID, Name: c.Name, Service: c.Service, State: c.State, Health: c.Health, Image: c.Image,
				Ports: c.Ports, Created: rfc3339(c.Created),
			})
		}
		body.Containers = &card
	}
	return &StackResourcesOutput{CacheControl: "no-store", Body: body}, nil
}
