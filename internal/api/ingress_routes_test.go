// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
	if !got.Enabled || len(got.Routes) != 1 {
		t.Fatalf("answer = %+v, want enabled with exactly one row", got)
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
