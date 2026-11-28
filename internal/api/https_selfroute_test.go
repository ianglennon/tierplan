// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// Settings > HTTPS names TierPlan's own ingress route (Phase 28, D-16, UI-SPEC A-06).

// httpsSelfRouteAnswer is the selfRoute object of the HTTPS status.
type httpsSelfRouteAnswer struct {
	EnvID string `json:"envId"`
	Host  string `json:"host"`
	State string `json:"state"`
}

// TestHTTPSStatusCarriesTheSelfRouteLine is the tracer of A-06: while TierPlan's own route is on, the HTTPS status names the
// environment by its ID, the hostname and the state, derived by the same helper as the route's card; with the route off there is
// no line. The line holds those three fields only: no switch, no snippet and no token.
func TestHTTPSStatusCarriesTheSelfRouteLine(t *testing.T) {
	t.Parallel()
	src, facts := &routeSourcesFake{}, &selfFactsFake{}
	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"localhost"}, nil)
	f, e := stackEnvCfg(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "ing-net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm"})
	}, func(ef *envFix) {
		ef.stackIngressRoutes, ef.stackSelfRoute = src, facts
		ef.https = api.HTTPSDeps{
			Served: func() api.ServedCertificate { return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf} },
			// The server's wiring: the ingress deploy deps over the stack routes' deps, read when the status is.
			SelfRoute: func(ctx context.Context) (string, string, string, bool) {
				return api.IngressDeployDeps{Routes: ef.stackDeps}.SelfRouteLine(ctx)
			},
		}
	})
	seed := seedIngressWorld(t, f, e)
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	*src = *systemServedBundle(ingresshub.Node{Status: "active", AppliedBundleID: "b1"}, ingresshub.Node{Status: "active", AppliedBundleID: "b1"})
	cookie, _ := f.signIn(t, "admin", false)

	read := func(t *testing.T) (*httpsSelfRouteAnswer, string) {
		t.Helper()
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status", cookie: cookie})
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/https/status: %d %s", rec.Code, rec.Body)
		}
		var body struct {
			SelfRoute json.RawMessage `json:"selfRoute"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.SelfRoute == nil {
			return nil, ""
		}
		var fields map[string]any
		if err := json.Unmarshal(body.SelfRoute, &fields); err != nil {
			t.Fatal(err)
		}
		for k := range fields {
			if k != "envId" && k != "host" && k != "state" {
				t.Errorf("selfRoute carries %q; it holds envId, host and state only", k)
			}
		}
		var s httpsSelfRouteAnswer
		if err := json.Unmarshal(body.SelfRoute, &s); err != nil {
			t.Fatal(err)
		}
		return &s, string(body.SelfRoute)
	}

	// Off: no line, even with facts that would say serving.
	facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, Attached: true, AttachedKnown: true})
	if s, raw := read(t); s != nil {
		t.Fatalf("with the route off selfRoute = %s, want none", raw)
	}

	claimSystemRoute(t, f, e, seed)

	t.Run("on and serving", func(t *testing.T) {
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Kind: ingressdeploy.SelfKindSwarmService,
			Attached: true, AttachedKnown: true,
		})
		s, raw := read(t)
		if s == nil || s.EnvID != e.ID || s.Host != systemRouteHost || s.State != stackops.RouteServing {
			t.Fatalf("selfRoute = %s, want {%s %s serving}", raw, e.ID, systemRouteHost)
		}
	})

	t.Run("not attached", func(t *testing.T) {
		snippet := &ingressdeploy.SelfSnippet{Kind: ingressdeploy.SelfSnippetStackFile, Text: ingressdeploy.StackFileSnippet("tierplan")}
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Kind: ingressdeploy.SelfKindSwarmService,
			AttachedKnown: true, Snippet: snippet,
		})
		s, raw := read(t)
		if s == nil || s.EnvID != e.ID || s.Host != systemRouteHost || s.State != "self_not_attached" {
			t.Fatalf("selfRoute = %s, want self_not_attached", raw)
		}
		if strings.Contains(raw, "tierplan-self") || strings.Contains(raw, "snippet") {
			t.Errorf("selfRoute = %s carries the next step; the HTTPS line never does", raw)
		}
	})

	t.Run("attachment not observed yet", func(t *testing.T) {
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Attached: true})
		if s, raw := read(t); s == nil || s.State != "unknown" {
			t.Errorf("selfRoute = %s, want unknown, never serving", raw)
		}
	})

	t.Run("route switched off again", func(t *testing.T) {
		if _, _, err := f.stackDeps.Ingress.ClearSystemClaim(context.Background(), e.ID, "admin", f.clk.now(), nil); err != nil {
			t.Fatal(err)
		}
		if s, raw := read(t); s != nil {
			t.Errorf("after the route is off selfRoute = %s, want none", raw)
		}
	})
}
