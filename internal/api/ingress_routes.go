// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The environment routes endpoint (Phase 27, D-08, D-09): every route an environment's stacks hold, each with the state
// the stack page reads and the five conditions behind it. It is derived by the same stackops.RouteStates call the stack
// page's routes are, so the two pages never disagree. Nothing here carries a certificate, a key or a token.

// RouteAppliedBody is how many active ingress nodes applied the bundle that holds a route.
type RouteAppliedBody struct {
	Nodes int `json:"nodes" doc:"The active ingress nodes that applied the current bundle"`
	Total int `json:"total" doc:"The active ingress nodes the environment has"`
}

// RouteConditionBody is one of a route's five conditions.
type RouteConditionBody struct {
	Code    string            `json:"code" enum:"claim_held,network_attached,backend_healthy,certificate_ready,applied" doc:"Which condition this is. claim_held: the stack holds the claim the route sits under. network_attached: the service is on the ingress network. backend_healthy: the service runs at least one replica. certificate_ready: the hostname has a certificate the bundle can carry. applied: every active ingress node applied a bundle that holds the route. A route's conditions always come in this order"`
	State   string            `json:"state" enum:"ready,not_ready,unknown" doc:"ready: the condition holds. not_ready: it was read and does not hold. unknown: what it depends on was not read, for example the engine could not be reached or ingress is off; it is never reported ready"`
	Reason  string            `json:"reason" doc:"Why the condition is not ready or unknown; empty when it is ready. One of not_attached, stack_stopped, no_running_task, no_certificate, certificate_not_ready, not_in_bundle, no_nodes, nodes_behind, ingress_off, engine_unreadable or no_bundle"`
	Message string            `json:"message" doc:"A short sentence from the server saying why, for a person; empty when the condition is ready. For certificate_not_ready it is the certificate's own failure hint"`
	Applied *RouteAppliedBody `json:"applied,omitempty" doc:"How many active ingress nodes applied the bundle; present on the applied condition only"`
}

// IngressRouteBody is one row of the environment's route list.
type IngressRouteBody struct {
	Kind       string               `json:"kind" enum:"route,claim,reserved" doc:"route: a route a stack serves. claim: a hostname a stack holds with no route to serve. reserved: a hostname reserved under a domain that no stack holds"`
	Hostname   string               `json:"hostname" doc:"The row's hostname"`
	Path       string               `json:"path" doc:"The path prefix a route serves, / for the whole host; empty for a claim or a reserved name"`
	StackID    string               `json:"stackId" doc:"The ID of the stack that owns the row; empty for a reserved name and for a system claim"`
	StackName  string               `json:"stackName" doc:"The name of that stack; empty when stackId is"`
	Service    string               `json:"service" doc:"The service's name in the compose file; empty for a claim or a reserved name"`
	Port       int                  `json:"port" doc:"The port the proxy sends the route's requests to; 0 for a claim or a reserved name"`
	Scheme     string               `json:"scheme" enum:"http,https," doc:"The scheme the proxy speaks to the backend; empty for a claim or a reserved name"`
	Strip      bool                 `json:"strip" doc:"True when the proxy removes the path prefix before it forwards a request"`
	OwnerKind  string               `json:"ownerKind" enum:"stack,system," doc:"stack: a stack owns the hostname. system: TierPlan itself does. Empty for a reserved name, which nothing owns"`
	State      string               `json:"state" enum:"serving,pending,waiting_certificate,backend_not_running,not_attached,ingress_off,unknown," doc:"What the route is doing now, the same state the stack page reads: serving, pending, waiting_certificate, backend_not_running, not_attached or ingress_off. unknown: the engine could not be read, so what runs is not known. Empty for a claim, a reserved name and a system route. Derived when read, stored nowhere"`
	Reason     string               `json:"reason" doc:"For waiting_certificate, why the certificate is not served: no_certificate or certificate_not_ready. Empty for every other state"`
	Stopped    bool                 `json:"stopped" doc:"True when state is backend_not_running because the stack is stopped"`
	Conditions []RouteConditionBody `json:"conditions" doc:"The route's five conditions in their fixed order; empty for a claim, a reserved name and a system route"`
}

// IngressRoutesBody is the environment's route list.
type IngressRoutesBody struct {
	Enabled bool               `json:"enabled" doc:"True while the environment's ingress is switched on"`
	Routes  []IngressRouteBody `json:"routes" doc:"The rows of the environment, never another environment's: the routes in hostname, path then service order, then the claims with no route in hostname order, then the reserved names in hostname order; empty when there are none"`
}

// IngressRoutesOutput wraps IngressRoutesBody for huma.
type IngressRoutesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressRoutesBody
}

// ListIngressRoutesInput is the path of the route list.
type ListIngressRoutesInput struct {
	EnvironmentPathInput
}

func registerIngressRoutes(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listIngressRoutes",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/routes",
		Summary:     "List the routes of an environment and what each is doing now",
		Description: "Answers every route the environment's stacks hold, each with its hostname, path, owner, backend and the state the stack page reads, " +
			"and the five conditions behind it: claim held, network attached, backend healthy, certificate ready and applied. " +
			"The state and the conditions are worked out when asked, by the same call as the stack page's routes, from the stored claims, the engine, the proxy's current bundle and what each active node applied; nothing is stored. " +
			"When the engine cannot be read the state is unknown and the conditions it decides are unknown. The answer never carries a certificate, a key or a token, and never another environment's names. " +
			"Needs ingress.read on the environment (Viewer).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressRead), func(ctx context.Context, in *ListIngressRoutesInput) (*IngressRoutesOutput, error) {
		return listIngressRoutes(ctx, d, in)
	})
}

// routeRowKey names one route of the environment: its hostname, path and service.
type routeRowKey struct {
	key     stackops.RouteKey
	service string
}

func listIngressRoutes(ctx context.Context, d StacksDeps, _ *ListIngressRoutesInput) (*IngressRoutesOutput, error) {
	env := httpx.EnvFrom(ctx)
	if env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	out := &IngressRoutesOutput{CacheControl: "no-store", Body: IngressRoutesBody{Routes: []IngressRouteBody{}}}
	if d.Ingress == nil {
		return out, nil
	}
	snap, err := d.Ingress.Snapshot(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "read the routes of", env.ID, err)
	}
	out.Body.Enabled = snap.Settings.Enabled

	// The stacks the routes belong to, to know which are stopped; a stack the list lacks counts as running.
	stacksByID := map[string]stackops.Stack{}
	if len(snap.Routes) > 0 {
		own, err := d.repo().Stacks(ctx, env.ID)
		if err != nil {
			return nil, stackInternal(d, "list the stacks of", env.ID, err)
		}
		for _, st := range own {
			stacksByID[st.ID] = st
		}
	}

	// What the bundle and the nodes say is environment-wide: read once, and every stack's input starts from it.
	var shared stackops.RouteStateInput
	shared.Enabled = snap.Settings.Enabled
	if shared.Enabled {
		d.readBundleState(ctx, env.ID, &shared)
		d.readCertMessages(ctx, env.ID, &shared)
	}

	type group struct {
		stack  stackops.Stack
		routes []ingress.Route
	}
	var order []string
	groups := map[string]*group{}
	byKey := map[routeRowKey]ingress.SnapshotRoute{}
	var system []ingress.SnapshotRoute
	for _, r := range snap.Routes {
		if r.OwnerKind != ingress.OwnerStack {
			system = append(system, r)
			continue
		}
		g := groups[r.StackID]
		if g == nil {
			st, ok := stacksByID[r.StackID]
			if !ok {
				st = stackops.Stack{ID: r.StackID, EnvID: env.ID, Name: r.StackName, Target: stacks.Target(r.Target)}
			}
			g = &group{stack: st}
			groups[r.StackID] = g
			order = append(order, r.StackID)
		}
		g.routes = append(g.routes, r.Route)
		byKey[routeRowKey{stackops.RouteKey{Hostname: r.Hostname, Path: pathOrRoot(r.PathPrefix)}, r.Service}] = r
	}

	// The engine is read once for the whole environment: the swarm once for every swarm stack, the host's containers
	// for each standalone one. A read that fails leaves the stacks that needed it unknown, never running or not.
	inputs := map[string]stackops.RouteStateInput{}
	var swarmStacks []string
	for _, id := range order {
		g := groups[id]
		in := shared
		in.Stopped, in.Routes = g.stack.Stopped, g.routes
		inputs[id] = in
		if in.Enabled && !in.Stopped && g.stack.Target == stacks.TargetSwarm {
			swarmStacks = append(swarmStacks, id)
		}
	}
	unreadable := map[string]bool{}
	if len(swarmStacks) > 0 {
		err := readSwarm(ctx, d, env.ID, func(cluster dockerx.ClusterRead, nets []dockerx.NetworkRow) {
			for _, id := range swarmStacks {
				in := inputs[id]
				in.Running, in.Attached = serviceFactsOf(cluster, nets, groups[id].stack.Name)
				inputs[id] = in
			}
		})
		if err != nil {
			warnEngineUnreadable(ctx, d.Log, env.ID, err)
			for _, id := range swarmStacks {
				unreadable[id] = true
			}
		}
	}
	// A standalone host is read once for every stack that needs it, in one call to its one node.
	var hostStacks []string
	for _, id := range order {
		if in := inputs[id]; in.Enabled && !in.Stopped && groups[id].stack.Target != stacks.TargetSwarm {
			hostStacks = append(hostStacks, id)
		}
	}
	if len(hostStacks) > 0 {
		names := make([]string, len(hostStacks))
		for i, id := range hostStacks {
			names[i] = groups[id].stack.Name
		}
		facts, err := hostServiceFacts(ctx, d, env.ID, names)
		if err != nil {
			warnEngineUnreadable(ctx, d.Log, env.ID, err)
		}
		for _, id := range hostStacks {
			if err != nil {
				unreadable[id] = true
				continue
			}
			in := inputs[id]
			in.Running, in.Attached = facts[groups[id].stack.Name].running, facts[groups[id].stack.Name].attached
			inputs[id] = in
		}
	}

	rows := make([]IngressRouteBody, 0, len(snap.Routes))
	for _, id := range order {
		g, in := groups[id], inputs[id]
		in.EngineUnreadable = unreadable[id]
		for _, s := range stackops.RouteStates(in) {
			r := byKey[routeRowKey{stackops.RouteKey{Hostname: s.Hostname, Path: s.Path}, s.Service}]
			row := IngressRouteBody{
				Kind: "route", Hostname: s.Hostname, Path: s.Path, StackID: g.stack.ID, StackName: cmp.Or(g.stack.Name, r.StackName),
				Service: s.Service, Port: r.Port, Scheme: string(r.Scheme), Strip: r.Strip, OwnerKind: string(ingress.OwnerStack),
				State: s.State, Reason: s.Reason, Stopped: s.Stopped, Conditions: conditionBodies(s.Conditions, s.Service),
			}
			if in.EngineUnreadable {
				row.State = "unknown"
			}
			rows = append(rows, row)
		}
	}
	for _, r := range system {
		rows = append(rows, IngressRouteBody{
			Kind: "route", Hostname: r.Hostname, Path: pathOrRoot(r.PathPrefix), Service: r.Service, Port: r.Port,
			Scheme: string(r.Scheme), Strip: r.Strip, OwnerKind: string(ingress.OwnerSystem), Conditions: []RouteConditionBody{},
		})
	}
	slices.SortStableFunc(rows, func(a, b IngressRouteBody) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Service, b.Service))
	})
	rows, err = appendUnroutedRows(ctx, d, env.ID, snap, rows)
	if err != nil {
		return nil, err
	}
	out.Body.Routes = rows
	return out, nil
}

// appendUnroutedRows adds, after the route rows, one claim row per hostname the environment holds that no route sits
// under, then one reserved row per name its domains reserve. Each block is in hostname order, and both read the one
// snapshot the routes were read from, so a deploy replacing a stack's claims shows either its old set or its new one.
// A claim or reserved row has no state and no conditions: there is nothing to serve.
func appendUnroutedRows(ctx context.Context, d StacksDeps, envID string, snap ingress.Snapshot, rows []IngressRouteBody) ([]IngressRouteBody, error) {
	routed := map[string]bool{}
	for _, r := range snap.Routes {
		routed[r.Hostname] = true
	}
	claimed := map[string]bool{}
	var claims []IngressRouteBody
	for _, c := range snap.Claims {
		claimed[c.Hostname] = true
		if routed[c.Hostname] {
			continue
		}
		claims = append(claims, IngressRouteBody{
			Kind: "claim", Hostname: c.Hostname, StackID: c.StackID, StackName: c.StackName, OwnerKind: string(c.OwnerKind),
			Conditions: []RouteConditionBody{},
		})
	}
	doms, err := d.Ingress.Domains(ctx, envID)
	if err != nil {
		return nil, stackInternal(d, "read the reserved names of", envID, err)
	}
	var reserved []IngressRouteBody
	for _, dom := range doms {
		for _, h := range dom.Reserved {
			if claimed[h] {
				continue
			}
			reserved = append(reserved, IngressRouteBody{Kind: "reserved", Hostname: h, Conditions: []RouteConditionBody{}})
		}
	}
	byHost := func(a, b IngressRouteBody) int { return cmp.Compare(a.Hostname, b.Hostname) }
	slices.SortStableFunc(claims, byHost)
	slices.SortStableFunc(reserved, byHost)
	return append(append(rows, claims...), reserved...), nil
}

// warnEngineUnreadable logs that the engine could not be read for the route list, which then answers unknown.
func warnEngineUnreadable(ctx context.Context, log *slog.Logger, envID string, err error) {
	if log == nil {
		return
	}
	log.WarnContext(ctx, "the engine could not be read for the environment's route list", "environment", envID, "error", err)
}

// conditionBodies maps a route's conditions to their bodies, in the order given. The list is never nil.
func conditionBodies(conds []stackops.Condition, service string) []RouteConditionBody {
	out := make([]RouteConditionBody, 0, len(conds))
	for _, c := range conds {
		b := RouteConditionBody{Code: c.Code, State: c.State, Reason: c.Reason}
		if c.State != stackops.CondReady {
			b.Message = conditionMessage(c.Reason, service)
			if c.Reason == stackops.ReasonCertificateNotReady && c.Message != "" {
				b.Message = c.Message
			}
		}
		if c.Code == stackops.CondApplied {
			b.Applied = &RouteAppliedBody{Nodes: c.Applied, Total: c.Total}
		}
		out = append(out, b)
	}
	return out
}

// conditionMessage is the server's short sentence for a reason code. They are fixed sentences: never an authority's or
// an engine's own words (T-27-22). certificate_not_ready has a default here; the certificate's own failure hint replaces
// it when there is one.
func conditionMessage(reason, service string) string {
	switch reason {
	case stackops.ReasonNotAttached:
		return "Service " + service + " is not on the ingress network, so the proxy cannot reach it. Redeploy the stack."
	case stackops.ReasonStackStopped:
		return "The stack is stopped."
	case stackops.ReasonNoRunningTask:
		return "Service " + service + " runs no replica."
	case stackops.ReasonNoCertificate:
		return "The hostname has no certificate yet."
	case stackops.ReasonCertificateNotReady:
		return certNotReady
	case stackops.ReasonNotInBundle:
		return "The route is not in the proxy's current bundle yet."
	case stackops.ReasonNoNodes:
		return "No ingress node is active to serve the route."
	case stackops.ReasonNodesBehind:
		return "Some ingress nodes have not applied the current bundle yet."
	case stackops.ReasonIngressOff:
		return "Ingress is switched off for this environment."
	case stackops.ReasonEngineUnreadable:
		return "The engine could not be read, so this is not known."
	case stackops.ReasonNoBundle:
		return "No bundle has been published for this environment yet."
	}
	return ""
}
