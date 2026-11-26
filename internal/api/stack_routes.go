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
	State    string `json:"state" enum:"serving,pending,waiting_certificate,backend_not_running,not_attached,ingress_off" doc:"serving: every active ingress node applied a bundle that holds the route. pending: the route is not in the current bundle yet, or not every active node has applied it. waiting_certificate: the route is left out until its hostname has a certificate, and reason says why. backend_not_running: the service runs no replica, or the stack is stopped. not_attached: the service is not on the ingress network, so it needs a redeploy. ingress_off: the environment's ingress is switched off. Derived when read, stored nowhere"`
	Reason   string `json:"reason" doc:"For waiting_certificate, why the certificate is not served: no_certificate (none yet) or certificate_not_ready (one exists and cannot be served yet). Empty for every other state"`
	Stopped  bool   `json:"stopped" doc:"True when state is backend_not_running because the stack is stopped. A stopped stack keeps its hostnames"`
	// Conditions are the route's five conditions, derived by the same call that derives the state.
	Conditions []RouteConditionBody `json:"conditions" doc:"The route's five conditions in a fixed order: claim_held, network_attached, backend_healthy, certificate_ready, applied. They are derived by the same call as state, so the Ingress page's route list answers the same ones for the same route"`
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
	registerIngressRoutes(r, d)
	httpx.Register(r, huma.Operation{
		OperationID: "getStackRoutes", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/routes",
		Summary: "What each route of a stack is doing now",
		Description: "Answers each route the stack holds with its hostname, path, service and a state: serving, pending, waiting_certificate (with the reason), backend_not_running (stopped is true when the stack is stopped), not_attached, or ingress_off. " +
			"The first of these that holds is the state, in this order: ingress off, then the backend not running, then not attached, then waiting for a certificate, then pending. " +
			"The state is worked out when asked from the stored claims, the services on the swarm or the containers on the standalone host, the proxy's current bundle and what each active node applied; nothing is stored. " +
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
	}
	// What runs matters only to a route that gets that far in the order of the states. A swarm stack is read from the
	// swarm, a standalone one from the host's own containers. What the bundle and the nodes say is read whenever ingress
	// is on, so a stopped stack's conditions say the same as the Ingress page's.
	if input.Enabled {
		if !input.Stopped {
			read := readRunning
			if st.Target != stacks.TargetSwarm {
				read = readRunningStandalone
			}
			if err := read(ctx, d, env.ID, st, &input); err != nil {
				return nil, snapshotProblem(d.Log, env.ID, err)
			}
		}
		d.readBundleState(ctx, env.ID, &input)
		d.readCertMessages(ctx, env.ID, &input)
	}
	for _, s := range stackops.RouteStates(input) {
		out.Body.Routes = append(out.Body.Routes, StackRouteBody{
			Hostname: s.Hostname, Path: s.Path, Service: s.Service, State: s.State, Reason: s.Reason, Stopped: s.Stopped,
			Conditions: conditionBodies(s.Conditions, s.Service),
		})
	}
	return out, nil
}

// readRunning reads, in one read of the swarm, how many replicas of each of the stack's services run and whether each
// is on the ingress network.
func readRunning(ctx context.Context, d StacksDeps, envID string, st stackops.Stack, in *stackops.RouteStateInput) error {
	return readSwarm(ctx, d, envID, func(cluster dockerx.ClusterRead, nets []dockerx.NetworkRow) {
		in.Running, in.Attached = serviceFactsOf(cluster, nets, st.Name)
	})
}

// readSwarm reads the swarm once, its services and its networks, and hands both to use. The environment read and the
// stack read take their facts from the same read.
func readSwarm(ctx context.Context, d StacksDeps, envID string, use func(dockerx.ClusterRead, []dockerx.NetworkRow)) error {
	if d.Clusters == nil {
		return errors.New("api: no cluster reader to read the swarm with")
	}
	return d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cluster, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		nets, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		use(cluster, nets)
		return nil
	})
}

// serviceFactsOf groups what one swarm read says by stack: how many replicas of each service of the stack run and
// whether each is on the ingress network, by compose service name. A service belongs to the stack when it carries the
// stack's namespace label and its name starts with the stack's name and an underscore. The ingress network is found by
// name or by ID, since a service's spec may name either.
func serviceFactsOf(cluster dockerx.ClusterRead, nets []dockerx.NetworkRow, stackName string) (running map[string]int, attached map[string]bool) {
	running, attached = map[string]int{}, map[string]bool{}
	var netID string
	for _, n := range nets {
		if n.Name == ingress.NetworkName {
			netID = n.ID
			break
		}
	}
	prefix := stackName + "_"
	for _, row := range cluster.Services {
		if row.Stack == stackName && strings.HasPrefix(row.Name, prefix) {
			running[strings.TrimPrefix(row.Name, prefix)] = int(min(row.Running, math.MaxInt32))
		}
	}
	for _, ref := range cluster.ServiceRefs {
		if !strings.HasPrefix(ref.Name, prefix) {
			continue
		}
		for _, n := range ref.Networks {
			if n == ingress.NetworkName || netID != "" && n == netID {
				attached[strings.TrimPrefix(ref.Name, prefix)] = true
			}
		}
	}
	return running, attached
}

// readRunningStandalone reads, from the environment's one host, how many containers of each of the stack's services run
// and whether any of them is on the ingress network. The containers are the host's own, found by the Compose project
// label the stack's name is; it answers counts and one flag per service and nothing else of a container (T-25-10).
func readRunningStandalone(ctx context.Context, d StacksDeps, envID string, st stackops.Stack, in *stackops.RouteStateInput) error {
	facts, err := hostServiceFacts(ctx, d, envID, []string{st.Name})
	if err != nil {
		return err
	}
	in.Running, in.Attached = facts[st.Name].running, facts[st.Name].attached
	return nil
}

// serviceFacts is what one host says of one stack's services: how many containers of each run and whether any is on the
// ingress network, by compose service name.
type serviceFacts struct {
	running  map[string]int
	attached map[string]bool
}

// hostServiceFacts reads the environment's one host once, in one call to its node, for every stack named: the
// containers of each Compose project, running or not, as counts and one flag per service (T-25-10). A stack with no
// container still has an entry, with nothing running.
func hostServiceFacts(ctx context.Context, d StacksDeps, envID string, stackNames []string) (map[string]serviceFacts, error) {
	if d.Aggregator == nil {
		return nil, errors.New("api: no aggregator to read the host with")
	}
	node, err := d.ops().StandaloneNode(ctx, envID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]serviceFacts, len(stackNames))
	err = d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		for _, name := range stackNames {
			f := serviceFacts{running: map[string]int{}, attached: map[string]bool{}}
			rows, err := c.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
			if err != nil {
				return err
			}
			for _, row := range rows {
				service := row.Labels[stacks.LabelComposeService]
				if service == "" || row.State != "running" {
					continue
				}
				f.running[service]++
				for _, n := range row.Networks {
					if n.Name == ingress.NetworkName {
						f.attached[service] = true
					}
				}
			}
			out[name] = f
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readBundleState fills in what the reconciler and the nodes say: the routes the current bundle left out and why, the
// ones it holds, how many active nodes there are and how many applied it, and whether the environment has a bundle at
// all. Only the certificate reasons count as a wait. With no sources the routes are pending: nothing says a node has
// them.
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
	in.BundleKnown = true
	in.InBundle = map[stackops.RouteKey]bool{}
	for _, r := range bundle.Manifest.Routes {
		in.InBundle[stackops.RouteKey{Hostname: r.Hostname, Path: pathOrRoot(r.PathPrefix)}] = true
	}
	nodes, err := src.ListNodes(ctx, envID)
	if err != nil {
		d.Log.WarnContext(ctx, "the ingress nodes could not be read for a stack's route states", "environment", envID, "error", err)
		return
	}
	in.NodesApplied = true
	for _, n := range nodes {
		if n.Status != "active" {
			continue
		}
		in.ActiveNodes++
		if n.AppliedBundleID == bundle.ID {
			in.AppliedNodes++
		} else {
			in.NodesApplied = false
		}
	}
	if in.ActiveNodes == 0 {
		in.NodesApplied = false
	}
}

// readCertMessages fills in, for each hostname the current bundle left out because its certificate is not ready, the
// certificate's own failure hint. It reads the certificate state and never the certificate. A read that fails leaves
// the messages out: the condition still names its reason.
func (d StacksDeps) readCertMessages(ctx context.Context, envID string, in *stackops.RouteStateInput) {
	ops := d.ingressOps()
	if ops == nil {
		return
	}
	var hosts []string
	for key, reason := range in.Skipped {
		if reason == stackops.ReasonCertificateNotReady {
			hosts = append(hosts, key.Hostname)
		}
	}
	if len(hosts) == 0 {
		return
	}
	states, err := ops.CertStates(ctx, envID, hosts)
	if err != nil {
		d.Log.WarnContext(ctx, "the certificates could not be read for route conditions", "environment", envID, "error", err)
		return
	}
	in.CertMessages = map[string]string{}
	for host, st := range states {
		if st.State == stackops.CertWaiting && st.Reason != "" {
			in.CertMessages[host] = st.Reason
		}
	}
}

// pathOrRoot is a route's path prefix, "/" for none.
func pathOrRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
