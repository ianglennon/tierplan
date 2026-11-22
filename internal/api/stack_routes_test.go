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
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// GET /api/env/{envId}/stacks/{stackId}/routes answers what each route of a stack is doing now (plan 24-25, D-24).

func routesPath(stackID string) string { return "stacks/" + stackID + "/routes" }

type stackRoutesAnswer struct {
	Routes []struct {
		Hostname string `json:"hostname"`
		Path     string `json:"path"`
		Service  string `json:"service"`
		State    string `json:"state"`
		Reason   string `json:"reason"`
		Stopped  bool   `json:"stopped"`
	} `json:"routes"`
}

// routeSourcesFake is the reconciler's bundle and the nodes' reports, as a test sets them.
type routeSourcesFake struct {
	skipped []ingressrender.Skipped
	bundle  ingressbundle.Bundle
	have    bool
	nodes   []ingresshub.Node
}

func (s *routeSourcesFake) Skipped(string) []ingressrender.Skipped { return s.skipped }
func (s *routeSourcesFake) Current(string) (ingressbundle.Bundle, bool) {
	return s.bundle, s.have
}
func (s *routeSourcesFake) ListNodes(context.Context, string) ([]ingresshub.Node, error) {
	return s.nodes, nil
}

// routeStates reads the routes of the stack as the caller and returns hostname+path to state.
func routeStates(t *testing.T, c *roleCaller, stackID string) (map[string]string, stackRoutesAnswer) {
	t.Helper()
	rec := c.do(http.MethodGet, routesPath(stackID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET routes: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackRoutesAnswer](t, rec)
	out := map[string]string{}
	for _, r := range got.Routes {
		out[r.Hostname+r.Path] = r.State
	}
	return out, got
}

func TestStackRoutesRoute(t *testing.T) {
	t.Parallel()
	const (
		hostA = "a.apps.example.com"
		hostB = "b.apps.example.com"
	)
	src := &routeSourcesFake{}
	// The ingress network is on the swarm, as it is once ingress was switched on, so a deploy that attaches finds it.
	f, e := stackEnvCfg(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "ing-net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm"})
	}, func(f *envFix) { f.stackIngressRoutes = src })
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "routes-stack")
	plain := newStack(t, admin, "routes-plain")
	f.deployGood(t, admin, st.ID, ingressCompose(hostB, hostA))
	f.deployGood(t, admin, plain.ID, composeWebAndApi)

	t.Run("a viewer reads the routes in hostname order, ingress off", func(t *testing.T) {
		states, got := routeStates(t, viewer, st.ID)
		if len(got.Routes) != 2 || got.Routes[0].Hostname != hostA || got.Routes[1].Hostname != hostB {
			t.Fatalf("routes = %+v, want %s then %s", got.Routes, hostA, hostB)
		}
		for key, state := range states {
			if state != "ingress_off" {
				t.Errorf("%s = %q, want ingress_off while the environment's ingress is off", key, state)
			}
		}
		if got.Routes[0].Path != "/" || got.Routes[0].Service == "" {
			t.Errorf("route = %+v, want the root path and the service", got.Routes[0])
		}
	})

	t.Run("a stack that holds no route answers an empty list", func(t *testing.T) {
		rec := viewer.do(http.MethodGet, routesPath(plain.ID), "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"routes":[]`) {
			t.Errorf("GET routes: %d %s, want 200 and an empty routes array", rec.Code, rec.Body)
		}
	})

	t.Run("a stranger is refused and a stack of another environment is 404", func(t *testing.T) {
		f.addUser(t, "stranger-1", false)
		cookie, csrf := f.signIn(t, "stranger-1", false)
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/" + routesPath(st.ID), cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("a caller with no grant: %d %s, want 403 or 404", rec.Code, rec.Body)
		}
		if rec := viewer.do(http.MethodGet, routesPath("no-such-stack"), ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET routes of a missing stack: %d %s, want 404", rec.Code, rec.Body)
		}
	})

	// With ingress on, the stack that was deployed before was not attached.
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}

	t.Run("a service that is not on the ingress network is not attached", func(t *testing.T) {
		states, _ := routeStates(t, viewer, st.ID)
		for key, state := range states {
			if state != "not_attached" {
				t.Errorf("%s = %q, want not_attached", key, state)
			}
		}
	})

	t.Run("a stopped stack keeps its routes and they read backend not running", func(t *testing.T) {
		if _, err := f.st.Write.Exec(`UPDATE stacks SET stopped = 1 WHERE id = ?`, st.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.st.Write.Exec(`UPDATE stacks SET stopped = 0 WHERE id = ?`, st.ID) })
		_, got := routeStates(t, viewer, st.ID)
		if len(got.Routes) != 2 {
			t.Fatalf("routes = %+v, want both kept", got.Routes)
		}
		for _, r := range got.Routes {
			if r.State != "backend_not_running" || !r.Stopped {
				t.Errorf("route = %+v, want backend_not_running with stopped true", r)
			}
		}
		if n := len(claimsOf(t, f, e.ID, st.ID)); n != 2 {
			t.Errorf("claims = %d, want the stopped stack to keep both", n)
		}
	})

	t.Run("a standalone stack's routes read not served yet, whatever the swarm says", func(t *testing.T) {
		if _, err := f.st.Write.Exec(`UPDATE stacks SET target = 'standalone' WHERE id = ?`, st.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.st.Write.Exec(`UPDATE stacks SET target = 'swarm' WHERE id = ?`, st.ID) })
		_, got := routeStates(t, viewer, st.ID)
		if len(got.Routes) != 2 {
			t.Fatalf("routes = %+v, want both", got.Routes)
		}
		for _, r := range got.Routes {
			if r.State != "not_served_yet" || r.Stopped {
				t.Errorf("route = %+v, want not_served_yet, not stopped", r)
			}
		}
	})

	t.Run("the body never carries a certificate, a key or a token", func(t *testing.T) {
		rec := viewer.do(http.MethodGet, routesPath(st.ID), "")
		for _, bad := range []string{"PRIVATE KEY", "BEGIN CERTIFICATE", "token", "chain"} {
			if strings.Contains(rec.Body.String(), bad) {
				t.Errorf("body names %q: %s", bad, rec.Body)
			}
		}
	})

	// A stack deployed with ingress on is attached: its states then follow the proxy's bundle and the nodes.
	att := newStack(t, admin, "routes-attached")
	f.deployGood(t, admin, att.ID, ingressCompose("c.apps.example.com"))
	key := "c.apps.example.com/"
	bundle := func(id string, hosts ...string) ingressbundle.Bundle {
		b := ingressbundle.Bundle{ID: id}
		for _, h := range hosts {
			b.Manifest.Routes = append(b.Manifest.Routes, ingressbundle.RouteEntry{Hostname: h, PathPrefix: "/", Stack: "routes-attached"})
		}
		return b
	}
	node := func(applied string) ingresshub.Node {
		return ingresshub.Node{Status: "active", AppliedBundleID: applied}
	}
	cases := []struct {
		name  string
		set   func()
		state string
		why   string
	}{
		{"no bundle yet is pending", func() { *src = routeSourcesFake{} }, "pending", ""},
		{"a bundle no node applied is pending", func() {
			*src = routeSourcesFake{bundle: bundle("b1", "c.apps.example.com"), have: true, nodes: []ingresshub.Node{node("")}}
		}, "pending", ""},
		{"every active node on the bundle is serving", func() {
			*src = routeSourcesFake{bundle: bundle("b1", "c.apps.example.com"), have: true, nodes: []ingresshub.Node{node("b1"), {Status: "revoked", AppliedBundleID: "old"}}}
		}, "serving", ""},
		{"one active node behind is pending", func() {
			*src = routeSourcesFake{bundle: bundle("b2", "c.apps.example.com"), have: true, nodes: []ingresshub.Node{node("b2"), node("b1")}}
		}, "pending", ""},
		{"no active node is pending", func() {
			*src = routeSourcesFake{bundle: bundle("b1", "c.apps.example.com"), have: true}
		}, "pending", ""},
		{"a route left out for a certificate is waiting, with the reason", func() {
			*src = routeSourcesFake{
				bundle: bundle("b1"), have: true, nodes: []ingresshub.Node{node("b1")},
				skipped: []ingressrender.Skipped{{Hostname: "c.apps.example.com", PathPrefix: "/", Reason: ingressrender.SkipCertificateNotReady}},
			}
		}, "waiting_certificate", "certificate_not_ready"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			_, got := routeStates(t, viewer, att.ID)
			if len(got.Routes) != 1 || got.Routes[0].Hostname+got.Routes[0].Path != key {
				t.Fatalf("routes = %+v, want the one route", got.Routes)
			}
			if got.Routes[0].State != tc.state || got.Routes[0].Reason != tc.why {
				t.Errorf("route = %+v, want %s %q", got.Routes[0], tc.state, tc.why)
			}
		})
	}
}
