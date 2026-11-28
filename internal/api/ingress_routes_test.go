// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// GET /api/env/{envId}/ingress/routes answers every route of the environment, derived by the same call the stack page's
// routes are (plan 27-10, D-08, D-09).

type routeConditionAnswer struct {
	Code    string `json:"code"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Failed  bool   `json:"failed"`
	Applied *struct {
		Nodes int `json:"nodes"`
		Total int `json:"total"`
	} `json:"applied"`
}

type ingressRouteAnswer struct {
	Kind       string                 `json:"kind"`
	Hostname   string                 `json:"hostname"`
	Path       string                 `json:"path"`
	StackID    string                 `json:"stackId"`
	StackName  string                 `json:"stackName"`
	Service    string                 `json:"service"`
	Port       int                    `json:"port"`
	Scheme     string                 `json:"scheme"`
	Strip      bool                   `json:"strip"`
	OwnerKind  string                 `json:"ownerKind"`
	State      string                 `json:"state"`
	Reason     string                 `json:"reason"`
	Stopped    bool                   `json:"stopped"`
	Conditions []routeConditionAnswer `json:"conditions"`
}

type ingressRoutesAnswer struct {
	Enabled bool                 `json:"enabled"`
	Routes  []ingressRouteAnswer `json:"routes"`
}

// swarmRoutesRig is a swarm environment whose engine holds the ingress network and the given services, with one swarm
// stack named shop that claims host to its service web, and ingress switched on.
func swarmRoutesRig(t *testing.T, host string, services ...dockertest.FakeService) (*envFix, envBody, *roleCaller, *routeSourcesFake, ingressWorldSeed) {
	t.Helper()
	src := &routeSourcesFake{}
	f, e := stackEnvCfg(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "ing-net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm"})
		c.ServiceRows = append(c.ServiceRows, services...)
	}, func(f *envFix) { f.stackIngressRoutes = src })
	seed := seedIngressWorld(t, f, e)
	if _, err := f.st.Write.Exec(
		`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'swarm', 'tester', 1)`,
		"stack-shop", e.ID, "shop"); err != nil {
		t.Fatalf("seed the swarm stack: %v", err)
	}
	decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 8080}
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(context.Background(), ingressstore.ReplaceInput{EnvID: e.ID, StackID: "stack-shop", Actor: "admin", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatalf("claim %s: %v", host, err)
	}
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	return f, e, f.caller(t, e, "viewer-1", "viewer"), src, seed
}

func shopService(running uint64, networks ...string) dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-shop-web", Name: "shop_web", Mode: "replicated", Running: running, Desired: 1, Image: "nginx:1.27",
		Labels: map[string]string{"com.docker.stack.namespace": "shop"}, Networks: networks,
	}
}

func servedBundle(host string, nodes ...ingresshub.Node) *routeSourcesFake {
	b := ingressbundle.Bundle{ID: "b1"}
	b.Manifest.Routes = append(b.Manifest.Routes, ingressbundle.RouteEntry{Hostname: host, PathPrefix: "/", Stack: "shop"})
	return &routeSourcesFake{bundle: b, have: true, nodes: nodes}
}

func ingressRoutesOf(t *testing.T, c *roleCaller) (ingressRoutesAnswer, string) {
	t.Helper()
	rec := c.do(http.MethodGet, "ingress/routes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ingress/routes: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	return answerOf[ingressRoutesAnswer](t, rec), rec.Body.String()
}

func TestListIngressRoutesSwarmTracer(t *testing.T) {
	t.Parallel()
	const host = "app.apps.example.com"
	_, _, viewer, src, _ := swarmRoutesRig(t, host, shopService(1, ingress.NetworkName))
	*src = *servedBundle(host, ingresshub.Node{Status: "active", AppliedBundleID: "b1"}, ingresshub.Node{Status: "active", AppliedBundleID: "b1"})

	got, raw := ingressRoutesOf(t, viewer)
	// The route, then the one name the environment's domain reserves.
	if !got.Enabled || len(got.Routes) != 2 || got.Routes[1].Kind != "reserved" {
		t.Fatalf("answer = %+v, want enabled with the route and the reserved name", got)
	}
	r := got.Routes[0]
	if r.Kind != "route" || r.Hostname != host || r.Path != "/" || r.StackID != "stack-shop" || r.StackName != "shop" ||
		r.Service != "web" || r.Port != 8080 || r.Scheme != "http" || r.Strip || r.OwnerKind != "stack" || r.State != "serving" || r.Reason != "" {
		t.Errorf("row = %+v, want the served route of stack shop", r)
	}
	codes := []string{"claim_held", "network_attached", "backend_healthy", "certificate_ready", "applied"}
	if len(r.Conditions) != len(codes) {
		t.Fatalf("conditions = %+v, want the five in order", r.Conditions)
	}
	for i, c := range r.Conditions {
		if c.Code != codes[i] || c.State != "ready" || c.Reason != "" || c.Message != "" {
			t.Errorf("condition %d = %+v, want %s ready with no reason", i, c, codes[i])
		}
		if (c.Code == "applied") != (c.Applied != nil) {
			t.Errorf("condition %s applied = %+v, want it on the applied condition only", c.Code, c.Applied)
		}
	}
	if a := r.Conditions[4].Applied; a == nil || a.Nodes != 2 || a.Total != 2 {
		t.Errorf("applied = %+v, want 2 of 2", a)
	}
	for _, other := range []string{ingressOtherStackName, ingressTaken, ingressOtherEnvID} {
		if strings.Contains(raw, other) {
			t.Errorf("the answer names %q of another environment: %s", other, raw)
		}
	}
}

// claimWithoutRoute writes a hostname claim of the environment with no route under it, the way a stack that holds a name
// it serves nothing on does; a blank stackID makes it a system claim.
func claimWithoutRoute(t *testing.T, f *envFix, e envBody, seed ingressWorldSeed, host, stackID string) {
	t.Helper()
	kind, stack := "stack", any(stackID)
	if stackID == "" {
		kind, stack = "system", nil
	}
	if _, err := f.st.Write.Exec(
		`INSERT INTO ingress_hostnames (hostname, env_id, domain_id, owner_kind, stack_id, claimed_by, claimed_at) VALUES (?, ?, ?, ?, ?, 'tester', 1)`,
		host, e.ID, seed.domainID, kind, stack); err != nil {
		t.Fatalf("claim %s without a route: %v", host, err)
	}
}

func TestListIngressRoutesClaimsAndReserved(t *testing.T) {
	t.Parallel()
	const routed, held, sys = "a.apps.example.com", "b.apps.example.com", "c.apps.example.com"
	f, e, viewer, _, seed := swarmRoutesRig(t, routed, shopService(1, ingress.NetworkName))
	claimWithoutRoute(t, f, e, seed, held, "stack-shop")
	claimWithoutRoute(t, f, e, seed, sys, "")

	got, raw := ingressRoutesOf(t, viewer)
	if len(got.Routes) != 4 {
		t.Fatalf("rows = %+v, want the route, two claims and the reserved name", got.Routes)
	}
	if r := got.Routes[0]; r.Kind != "route" || r.Hostname != routed || r.State == "" || len(r.Conditions) != 5 {
		t.Errorf("row 0 = %+v, want the route of %s with its state and five conditions", r, routed)
	}
	if r := got.Routes[1]; r.Kind != "claim" || r.Hostname != held || r.StackID != "stack-shop" || r.StackName != "shop" ||
		r.OwnerKind != "stack" || r.State != "" || r.Path != "" || r.Service != "" || r.Port != 0 || len(r.Conditions) != 0 {
		t.Errorf("row 1 = %+v, want the stack claim of %s with no state and no conditions", r, held)
	}
	if r := got.Routes[2]; r.Kind != "claim" || r.Hostname != sys || r.StackID != "" || r.StackName != "" || r.OwnerKind != "system" || r.State != "" {
		t.Errorf("row 2 = %+v, want the system claim of %s with no stack", r, sys)
	}
	if r := got.Routes[3]; r.Kind != "reserved" || r.Hostname != seed.reservedHost || r.StackID != "" || r.OwnerKind != "" || r.State != "" || len(r.Conditions) != 0 {
		t.Errorf("row 3 = %+v, want the reserved name %s with no owner", r, seed.reservedHost)
	}
	if !strings.Contains(raw, `"conditions":[]`) || strings.Contains(raw, `"conditions":null`) {
		t.Errorf("the claim and reserved rows must carry an empty conditions list, not null: %s", raw)
	}
	for _, other := range []string{ingressOtherStackName, ingressTaken, ingressOtherEnvID} {
		if strings.Contains(raw, other) {
			t.Errorf("the answer names %q of another environment: %s", other, raw)
		}
	}
}

func TestListIngressRoutesEmpty(t *testing.T) {
	t.Parallel()
	f, e := stackEnvCfg(t, func(c *dockertest.FakeConfig) {}, func(f *envFix) { f.stackIngressRoutes = &routeSourcesFake{} })
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	got, raw := ingressRoutesOf(t, f.caller(t, e, "viewer-1", "viewer"))
	if !got.Enabled || len(got.Routes) != 0 {
		t.Errorf("answer = %+v, want enabled with no rows", got)
	}
	if !strings.Contains(raw, `"routes":[]`) {
		t.Errorf("routes must be an empty array, not null: %s", raw)
	}
}

// standaloneRoutesRigOf is a standalone environment whose host holds the containers, with ingress on and one stack per
// name in stacks, each claiming <name>.apps.example.com for its service web.
func standaloneRoutesRigOf(t *testing.T, containers []dockertest.FakeContainer, stackNames ...string) *roleCaller {
	t.Helper()
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	cfg.Containers = containers
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.stackIngressRoutes = &routeSourcesFake{}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	seedIngressWorld(t, f, e)
	ctx := context.Background()
	for _, name := range stackNames {
		if _, err := f.st.Write.Exec(
			`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES (?, ?, ?, 'standalone', 'tester', 1)`,
			"stack-"+name, e.ID, name); err != nil {
			t.Fatalf("seed the standalone stack %s: %v", name, err)
		}
		decl := ingress.Declaration{Service: "web", Host: name + ".apps.example.com", Label: "tierplan.ingress.host", Port: 80}
		if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: e.ID, StackID: "stack-" + name, Actor: "admin", Decls: []ingress.Declaration{decl}}); err != nil {
			t.Fatalf("claim for %s: %v", name, err)
		}
	}
	if _, err := f.stackDeps.Ingress.SetEnabled(ctx, e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	return f.caller(t, e, "viewer-1", "viewer")
}

func condOf(t *testing.T, r ingressRouteAnswer, code string) routeConditionAnswer {
	t.Helper()
	for _, c := range r.Conditions {
		if c.Code == code {
			return c
		}
	}
	t.Fatalf("route %+v has no %s condition", r, code)
	return routeConditionAnswer{}
}

func TestListIngressRoutesStandalone(t *testing.T) {
	t.Parallel()
	web := func(id, state, project string, networks ...string) dockertest.FakeContainer {
		eps := map[string]dockertest.FakeEndpoint{}
		for _, n := range networks {
			eps[n] = dockertest.FakeEndpoint{NetworkID: "id-" + n}
		}
		return dockertest.FakeContainer{
			ID: id, Names: []string{"/" + id}, Image: "nginx:1.27", State: state, Networks: eps,
			Labels: map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": "web"},
		}
	}
	viewer := standaloneRoutesRigOf(t, []dockertest.FakeContainer{
		web("up1", "running", "alpha", ingress.NetworkName),
		web("down1", "exited", "beta", ingress.NetworkName),
	}, "alpha", "beta")

	got, _ := ingressRoutesOf(t, viewer)
	byHost := map[string]ingressRouteAnswer{}
	for _, r := range got.Routes {
		if r.Kind == "route" {
			byHost[r.Hostname] = r
		}
	}
	if len(byHost) != 2 {
		t.Fatalf("rows = %+v, want one route per stack", got.Routes)
	}
	// No bundle has been published, so a running attached backend is pending: the same rule a swarm follows.
	a := byHost["alpha.apps.example.com"]
	if a.State != "pending" || condOf(t, a, "network_attached").State != "ready" || condOf(t, a, "backend_healthy").State != "ready" {
		t.Errorf("alpha = %+v, want pending with its network and backend ready", a)
	}
	b := byHost["beta.apps.example.com"]
	if b.State != "backend_not_running" {
		t.Errorf("beta state = %q, want backend_not_running", b.State)
	}
	if c := condOf(t, b, "backend_healthy"); c.State != "not_ready" || c.Reason != "no_running_task" {
		t.Errorf("beta backend_healthy = %+v, want not_ready no_running_task", c)
	}
}

func TestListIngressRoutesAdjacentPaths(t *testing.T) {
	t.Parallel()
	const host = "app.apps.example.com"
	f, e, viewer, _, _ := swarmRoutesRig(t, host, shopService(1, ingress.NetworkName))
	decls := []ingress.Declaration{
		{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 8080},
		{Service: "web", Name: "api", Host: host, Label: "tierplan.ingress.api.host", PathPrefix: "/api", Port: 8081},
	}
	if _, err := f.stackDeps.Ingress.ReplaceStackRoutes(context.Background(), ingressstore.ReplaceInput{EnvID: e.ID, StackID: "stack-shop", Actor: "admin", Decls: decls}); err != nil {
		t.Fatalf("claim both paths: %v", err)
	}
	got, _ := ingressRoutesOf(t, viewer)
	var routes []ingressRouteAnswer
	for _, r := range got.Routes {
		if r.Kind == "route" {
			routes = append(routes, r)
		}
	}
	if len(routes) != 2 || routes[0].Path != "/" || routes[1].Path != "/api" || routes[0].Hostname != host || routes[1].Hostname != host {
		t.Fatalf("routes = %+v, want / then /api on %s", routes, host)
	}
	for _, r := range routes {
		if len(r.Conditions) != 5 {
			t.Errorf("route %s %s has %d conditions, want its own five", r.Hostname, r.Path, len(r.Conditions))
		}
	}
	if routes[0].Port != 8080 || routes[1].Port != 8081 {
		t.Errorf("ports = %d and %d, want each row its own route's port", routes[0].Port, routes[1].Port)
	}
}

func TestListIngressRoutesIngressOff(t *testing.T) {
	t.Parallel()
	const host = "app.apps.example.com"
	f, e, viewer, _, seed := swarmRoutesRig(t, host, shopService(1, ingress.NetworkName))
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, false, "admin", nil); err != nil {
		t.Fatal(err)
	}
	got, _ := ingressRoutesOf(t, viewer)
	if got.Enabled || len(got.Routes) != 2 {
		t.Fatalf("answer = %+v, want ingress off with the route and the reserved name", got)
	}
	r := got.Routes[0]
	if r.Kind != "route" || r.State != "ingress_off" || len(r.Conditions) != 5 {
		t.Fatalf("route = %+v, want ingress_off with five conditions", r)
	}
	if c := r.Conditions[0]; c.Code != "claim_held" || c.State != "ready" {
		t.Errorf("first condition = %+v, want claim_held ready", c)
	}
	for _, c := range r.Conditions[1:] {
		if c.State != "unknown" || c.Reason != "ingress_off" {
			t.Errorf("condition %+v, want unknown ingress_off", c)
		}
	}
	if res := got.Routes[1]; res.Kind != "reserved" || res.Hostname != seed.reservedHost {
		t.Errorf("last row = %+v, want the reserved name still listed", res)
	}
}

func TestListIngressRoutesDegrades(t *testing.T) {
	t.Parallel()
	const host = "app.apps.example.com"
	nodes := []ingresshub.Node{{Status: "active", AppliedBundleID: "b1"}}

	t.Run("an engine that cannot be read leaves the engine's conditions unknown and still answers 200", func(t *testing.T) {
		t.Parallel()
		f, _, viewer, src, _ := swarmRoutesRig(t, host, shopService(1, ingress.NetworkName))
		*src = *servedBundle(host, nodes...)
		f.fake.LoseQuorum()

		got, raw := ingressRoutesOf(t, viewer) // fails the test on any status but 200
		if len(got.Routes) < 1 {
			t.Fatalf("answer = %+v, want the route", got)
		}
		r := got.Routes[0]
		if r.Kind != "route" || r.State != "unknown" || r.Reason != "" {
			t.Errorf("route = %+v, want state unknown, never serving or a backend fault", r)
		}
		for _, code := range []string{"network_attached", "backend_healthy"} {
			if c := condOf(t, r, code); c.State != "unknown" || c.Reason != "engine_unreadable" || c.Message == "" {
				t.Errorf("%s = %+v, want unknown engine_unreadable with a sentence", code, c)
			}
		}
		// What the engine does not decide is derived as usual.
		for _, code := range []string{"claim_held", "certificate_ready", "applied"} {
			if c := condOf(t, r, code); c.State != "ready" {
				t.Errorf("%s = %+v, want ready, derived from the bundle and the nodes", code, c)
			}
		}
		if strings.Contains(raw, "quorum") || strings.Contains(raw, "leader") {
			t.Errorf("the answer carries the engine's own words: %s", raw)
		}
	})

	t.Run("with no current bundle the bundle conditions are unknown and the state follows the legacy rules", func(t *testing.T) {
		t.Parallel()
		_, _, viewer, _, _ := swarmRoutesRig(t, host, shopService(1, ingress.NetworkName))
		got, _ := ingressRoutesOf(t, viewer)
		r := got.Routes[0]
		if r.State != "pending" {
			t.Errorf("state = %q, want pending: the engine reads fine and nothing has been published", r.State)
		}
		for _, code := range []string{"certificate_ready", "applied"} {
			if c := condOf(t, r, code); c.State != "unknown" || c.Reason != "no_bundle" {
				t.Errorf("%s = %+v, want unknown no_bundle", code, c)
			}
		}
	})

}
