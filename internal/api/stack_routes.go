// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// RouteSources is what the state of a stack's routes is read from besides the store and the engine: the reconciler's
// current bundle and its skipped list, and the environment's ingress nodes (D-24). The server wires the reconciler and
// the hub service in. It is never written through.
type RouteSources interface {
	// Skipped is the routes the environment's current bundle leaves out, with their reasons.
	Skipped(envID string) []ingressrender.Skipped
	// Current is the environment's current bundle and whether it has one.
	Current(envID string) (ingressbundle.Bundle, bool)
	// ListNodes is the environment's ingress nodes, revoked ones included.
	ListNodes(ctx context.Context, envID string) ([]ingresshub.Node, error)
}

// StackRouteBody is one route of a stack and what it is doing now.
type StackRouteBody struct {
	Hostname string `json:"hostname" doc:"The route's hostname"`
	Path     string `json:"path" doc:"The path prefix the route serves, / for the whole host"`
	Service  string `json:"service" doc:"The service's name in the compose file"`
	State    string `json:"state" enum:"serving,pending,waiting_certificate,backend_not_running,not_attached,ingress_off,not_served_yet" doc:"serving: every active ingress node applied a bundle that holds the route. pending: the route is not in the current bundle yet, or not every active node has applied it. waiting_certificate: the route is left out until its hostname has a certificate, and reason says why. backend_not_running: the service runs no replica, or the stack is stopped. not_attached: the service is not on the ingress network, so it needs a redeploy. ingress_off: the environment's ingress is switched off. not_served_yet: the stack runs on a standalone host, which ingress does not serve yet; its routes keep their hostnames and the other states are not worked out for it. Derived when read, stored nowhere"`
	Reason   string `json:"reason" doc:"For waiting_certificate, why the certificate is not served: no_certificate (none yet) or certificate_not_ready (one exists and cannot be served yet). Empty for every other state"`
	Stopped  bool   `json:"stopped" doc:"True when state is backend_not_running because the stack is stopped. A stopped stack keeps its hostnames"`
}

// StackRoutesBody is the routes a stack holds.
type StackRoutesBody struct {
	Routes []StackRouteBody `json:"routes" doc:"The routes the stack holds, in hostname then path order; empty when it holds none"`
}

// StackRoutesOutput wraps StackRoutesBody for huma.
type StackRoutesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRoutesBody
}

func registerStackRoutes(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackRoutes", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/routes",
		Summary: "What each route of a stack is doing now",
		Description: "Answers each route the stack holds with its hostname, path, service and a state: serving, pending, waiting_certificate (with the reason), backend_not_running (stopped is true when the stack is stopped), not_attached, ingress_off or not_served_yet (a stack on a standalone host). " +
			"The first of these that holds is the state, in that order of the list: not served yet, ingress off, then the backend not running, then not attached, then waiting for a certificate, then pending. " +
			"The state is worked out when asked from the stored claims, the services on the swarm, the proxy's current bundle and what each active node applied; nothing is stored. " +
			"A stack that holds no route answers an empty list. The answer never carries a certificate, a key or a token. A stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackRoutesOutput, error) {
			return getStackRoutes(ctx, d, in)
		})
}

func getStackRoutes(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackRoutesOutput, error) {
	env := httpx.EnvFrom(ctx)
	if env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	st, err := d.repo().Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	out := &StackRoutesOutput{CacheControl: "no-store", Body: StackRoutesBody{Routes: []StackRouteBody{}}}
	if d.Ingress == nil {
		return out, nil
	}
	snap, err := d.Ingress.Snapshot(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "read the routes of", env.ID, err)
	}
	var routes []ingress.Route
	for _, r := range snap.Routes {
		if r.OwnerKind == ingress.OwnerStack && r.StackID == st.ID {
			routes = append(routes, r.Route)
		}
	}
	if len(routes) == 0 {
		return out, nil
	}
	input := stackops.RouteStateInput{
		Enabled: snap.Settings.Enabled, Stopped: st.Stopped, Routes: routes,
		// Standalone serving arrives with the Phase 25 work; until then no route of a standalone stack is worked out
		// against the swarm, which would read a healthy running container as no backend.
		NotServed: st.Target != stacks.TargetSwarm,
	}
	// What runs and what the nodes hold matter only to a route that gets that far in the order of the states.
	if input.Enabled && !input.Stopped && !input.NotServed {
		if err := readRunning(ctx, d, env.ID, st, &input); err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		d.readBundleState(ctx, env.ID, &input)
	}
	for _, s := range stackops.RouteStates(input) {
		out.Body.Routes = append(out.Body.Routes, StackRouteBody{
			Hostname: s.Hostname, Path: s.Path, Service: s.Service, State: s.State, Reason: s.Reason, Stopped: s.Stopped,
		})
	}
	return out, nil
}

// readRunning reads, in one read of the swarm, how many replicas of each of the stack's services run and whether each
// is on the ingress network.
func readRunning(ctx context.Context, d StacksDeps, envID string, st stackops.Stack, in *stackops.RouteStateInput) error {
	if d.Clusters == nil {
		return errors.New("api: no cluster reader to read the swarm with")
	}
	in.Running, in.Attached = map[string]int{}, map[string]bool{}
	return d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cluster, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		nets, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		var netID string
		for _, n := range nets {
			if n.Name == ingress.NetworkName {
				netID = n.ID
				break
			}
		}
		prefix := st.Name + "_"
		for _, row := range cluster.Services {
			if row.Stack == st.Name && strings.HasPrefix(row.Name, prefix) {
				in.Running[strings.TrimPrefix(row.Name, prefix)] = int(min(row.Running, math.MaxInt32))
			}
		}
		for _, ref := range cluster.ServiceRefs {
			if !strings.HasPrefix(ref.Name, prefix) {
				continue
			}
			for _, n := range ref.Networks {
				if n == ingress.NetworkName || netID != "" && n == netID {
					in.Attached[strings.TrimPrefix(ref.Name, prefix)] = true
				}
			}
		}
		return nil
	})
}

// readBundleState fills in what the reconciler and the nodes say: the routes the current bundle left out and why, the
// ones it holds, and whether every active node applied it. Only the certificate reasons count as a wait. With no
// sources the routes are pending: nothing says a node has them.
func (d StacksDeps) readBundleState(ctx context.Context, envID string, in *stackops.RouteStateInput) {
	src := d.IngressRoutes
	if src == nil {
		return
	}
	in.Skipped = map[stackops.RouteKey]string{}
	for _, s := range src.Skipped(envID) {
		switch s.Reason {
		case ingressrender.SkipNoCertificate, ingressrender.SkipCertificateNotReady:
			in.Skipped[stackops.RouteKey{Hostname: s.Hostname, Path: pathOrRoot(s.PathPrefix)}] = string(s.Reason)
		}
	}
	bundle, have := src.Current(envID)
	if !have || bundle.ID == "" {
		return
	}
	in.InBundle = map[stackops.RouteKey]bool{}
	for _, r := range bundle.Manifest.Routes {
		in.InBundle[stackops.RouteKey{Hostname: r.Hostname, Path: pathOrRoot(r.PathPrefix)}] = true
	}
	nodes, err := src.ListNodes(ctx, envID)
	if err != nil {
		d.Log.WarnContext(ctx, "the ingress nodes could not be read for a stack's route states", "environment", envID, "error", err)
		return
	}
	active := 0
	in.NodesApplied = true
	for _, n := range nodes {
		if n.Status != "active" {
			continue
		}
		active++
		if n.AppliedBundleID != bundle.ID {
			in.NodesApplied = false
		}
	}
	if active == 0 {
		in.NodesApplied = false
	}
}

// pathOrRoot is a route's path prefix, "/" for none.
func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
