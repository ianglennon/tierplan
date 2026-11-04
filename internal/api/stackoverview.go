// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// StackContainerBody is one container of a standalone service.
type StackContainerBody struct {
	ID     string `json:"id" doc:"The container ID"`
	Name   string `json:"name" doc:"The container name"`
	State  string `json:"state" doc:"The engine's word for the container: running, exited, restarting, paused, created, removing or dead"`
	Health string `json:"health" doc:"none, starting, healthy or unhealthy"`
}

// StackServiceBody is one service of a deployed stack, as the engine holds it now.
type StackServiceBody struct {
	ID          string               `json:"id" doc:"The swarm service ID, which the service page is at; empty on a standalone host"`
	Name        string               `json:"name" doc:"The full name: stack_web on a swarm, web on a standalone host"`
	Short       string               `json:"short" doc:"The name in the compose file, without the stack prefix"`
	Mode        string               `json:"mode" doc:"replicated, global, replicated-job or global-job on a swarm; empty on a standalone host"`
	State       string               `json:"state" enum:"running,degraded,not_running,stopped" doc:"The roll-up of its tasks or containers: running, degraded (some running, or an update that stopped, or a failing health check), not_running, or stopped (scaled to 0, or every container exited). A job counts as running"`
	UpdateState string               `json:"updateState" doc:"The state of a swarm service's last update or rollback; empty when it has none and on a standalone host"`
	Running     int                  `json:"running" minimum:"0" doc:"Tasks that run (a swarm) or containers that run (a standalone host)"`
	Desired     int                  `json:"desired" minimum:"0" doc:"Tasks the swarm wants running, or the containers a standalone service has"`
	Image       string               `json:"image" doc:"The image as the compose file wrote it"`
	Pinned      string               `json:"pinned" doc:"The reference the engine runs: the image with its digest, or the image ID of one no registry has"`
	PinState    string               `json:"pinState" enum:"pinned,new,local" doc:"pinned: the reference carries a digest. new: it has none and the next deploy resolves it. local: the engine runs an image ID, an image from no registry"`
	Ports       []string             `json:"ports" doc:"The published ports as published->target/protocol, with (host) after one published in host mode"`
	Containers  []StackContainerBody `json:"containers" doc:"The containers of a standalone service, by name; empty on a swarm"`
	Update      *UpdateInfoBody      `json:"update,omitempty" doc:"Left out when the service has no image that is judged (update checks not available, or none judged yet): what is known about the service's image. A standalone service carries the state of its containers, which run one image"`
}

// StackServicesBody is the services a stack runs now.
type StackServicesBody struct {
	Items []StackServiceBody `json:"items" doc:"The stack's services by name; empty for a stack that has nothing running"`
}

// StackServicesOutput wraps StackServicesBody for huma.
type StackServicesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackServicesBody
}

func registerStackOverview(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listStackServices", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/services",
		Summary: "What a stack runs now",
		Description: "The services the stack owns on the engine, read now: on a swarm each service with its task counts, its image as written and as pinned and its published ports; " +
			"on a standalone host each service with its containers. A service the engine holds under the stack's name that TierPlan did not make is not listed. " +
			"A stack that was never deployed answers an empty list. A stack ID of another environment is 404 stack_not_found. An environment that cannot be read is the environment's own problem answer. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackServicesOutput, error) {
			return listStackServices(ctx, d, in)
		})
}

func listStackServices(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackServicesOutput, error) {
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if errors.Is(err, stackops.ErrStackNotFound) {
		return nil, stackNotFound()
	}
	if err != nil {
		return nil, stackInternal(d, "read", in.EnvID, err)
	}
	rows, err := d.ops().Overview(ctx, in.EnvID, st)
	if err != nil {
		return nil, snapshotProblem(d.Log, in.EnvID, err)
	}
	body := StackServicesBody{Items: stackServiceBodies(rows, newStackReads(ctx, d, in.EnvID).updates().services[st.Name])}
	return &StackServicesOutput{CacheControl: "no-store", Body: body}, nil
}

// stackServiceBodies draws the services of a stack as the answers carry them. Only names, states, images and
// ports leave: never an environment value, a command or a log option of a service (D-47). judged are the
// verdicts of the stack's services, by swarm service ID or, on a standalone host, by service name.
func stackServiceBodies(rows []stackops.ServiceRow, judged map[string]updates.ServiceVerdict) []StackServiceBody {
	out := make([]StackServiceBody, 0, len(rows))
	for _, r := range rows {
		item := StackServiceBody{
			ID: r.ID, Name: r.Name, Short: r.Short, Mode: r.Mode, State: r.State, UpdateState: r.UpdateState,
			Running: r.Running, Desired: r.Desired, Image: r.Image, Pinned: r.Pinned, PinState: r.PinState,
			Ports: r.Ports, Containers: make([]StackContainerBody, 0, len(r.Containers)),
		}
		for _, c := range r.Containers {
			item.Containers = append(item.Containers, StackContainerBody{ID: c.ID, Name: c.Name, State: c.State, Health: c.Health})
		}
		key := r.ID
		if key == "" {
			key = r.Short
		}
		if v, ok := judged[key]; ok {
			item.Update = updateInfoOf(v.Verdict, v.Workload)
		}
		out = append(out, item)
	}
	return out
}
