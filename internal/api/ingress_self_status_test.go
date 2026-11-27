// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/ingressrender"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TierPlan's own route through the API every route is read through (Phase 28, SELF-01, A-07, UI-SPEC A-01): a full row on the
// environment's route list, derived by the same call as the ingress status's selfRoute object.

// selfFactsFake is TierPlan's own route's facts by environment, standing in for the deployer's reader.
type selfFactsFake struct {
	mu    sync.Mutex
	byEnv map[string]ingressdeploy.SelfRouteFacts
}

// set gives envID its facts.
func (s *selfFactsFake) set(envID string, f ingressdeploy.SelfRouteFacts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byEnv == nil {
		s.byEnv = map[string]ingressdeploy.SelfRouteFacts{}
	}
	s.byEnv[envID] = f
}

func (s *selfFactsFake) SelfRouteFacts(_ context.Context, envID string) (ingressdeploy.SelfRouteFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byEnv[envID]
	if !ok {
		// An environment the fake was told nothing about is one TierPlan does not run in.
		return ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereNo}, nil
	}
	return f, nil
}

const systemRouteHost = "tp." + ingressDomain

// systemRouteRig is a swarm environment with the ingress network, the domain of seedIngressWorld and ingress on, whose route
// sources and self route facts the test sets. No stack holds a route.
func systemRouteRig(t *testing.T) (*envFix, envBody, *roleCaller, *routeSourcesFake, *selfFactsFake, ingressWorldSeed) {
	t.Helper()
	src, facts := &routeSourcesFake{}, &selfFactsFake{}
	f, e := stackEnvCfg(t, func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "ing-net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm"})
	}, func(f *envFix) { f.stackIngressRoutes, f.stackSelfRoute = src, facts })
	seed := seedIngressWorld(t, f, e)
	if _, err := f.stackDeps.Ingress.SetEnabled(context.Background(), e.ID, true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	return f, e, f.caller(t, e, "viewer-1", "viewer"), src, facts, seed
}

// claimSystemRoute stores TierPlan's own route on systemRouteHost in e.
func claimSystemRoute(t *testing.T, f *envFix, e envBody, seed ingressWorldSeed) {
	t.Helper()
	if err := f.stackDeps.Ingress.SetSystemClaim(testsupport.Context(t), ingressstore.SystemClaimInput{
		EnvID: e.ID, Hostname: systemRouteHost, DomainID: seed.domainID, Service: "tierplan", Port: 7480, Actor: "admin",
	}, nil); err != nil {
		t.Fatalf("SetSystemClaim: %v", err)
	}
}

// systemServedBundle is a bundle that holds TierPlan's own route, applied by every node given.
func systemServedBundle(nodes ...ingresshub.Node) *routeSourcesFake {
	b := ingressbundle.Bundle{ID: "b1"}
	b.Manifest.Routes = append(b.Manifest.Routes, ingressbundle.RouteEntry{Hostname: systemRouteHost, PathPrefix: "/"})
	return &routeSourcesFake{bundle: b, have: true, nodes: nodes}
}

// systemRow is the route row TierPlan owns, if any.
func systemRow(rows []ingressRouteAnswer) (ingressRouteAnswer, bool) {
	for _, r := range rows {
		if r.Kind == "route" && r.OwnerKind == string(ingress.OwnerSystem) {
			return r, true
		}
	}
	return ingressRouteAnswer{}, false
}

// TestRoutesListDerivesTheSystemRow is the tracer of A-07: with TierPlan's own route on, the routes list has one row owned by
// the system with the five conditions in order and the state derived like any route's; with it off there is no such row, and
// an attachment no pass has read is unknown, never ready.
func TestRoutesListDerivesTheSystemRow(t *testing.T) {
	t.Parallel()
	f, e, viewer, src, facts, seed := systemRouteRig(t)
	applied := []ingresshub.Node{{Status: "active", AppliedBundleID: "b1"}, {Status: "active", AppliedBundleID: "b1"}}
	*src = *systemServedBundle(applied...)

	got, _ := ingressRoutesOf(t, viewer)
	if r, ok := systemRow(got.Routes); ok {
		t.Fatalf("with the route off the list has a system row %+v", r)
	}

	claimSystemRoute(t, f, e, seed)
	facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Attached: true, AttachedKnown: true})
	got, _ = ingressRoutesOf(t, viewer)
	r, ok := systemRow(got.Routes)
	if !ok {
		t.Fatalf("rows = %+v, want TierPlan's own route", got.Routes)
	}
	if r.Hostname != systemRouteHost || r.Path != "/" || r.StackID != "" || r.StackName != "" || r.Service != "tierplan" ||
		r.Port != 7480 || r.Scheme != "https" || r.State != stackops.RouteServing || r.Reason != "" {
		t.Errorf("row = %+v, want TierPlan's own route serving", r)
	}
	wantCodes := []string{stackops.CondClaimHeld, stackops.CondNetworkAttached, stackops.CondBackendHealthy, stackops.CondCertificateReady, stackops.CondApplied}
	if len(r.Conditions) != len(wantCodes) {
		t.Fatalf("conditions = %+v, want five", r.Conditions)
	}
	for i, c := range r.Conditions {
		if c.Code != wantCodes[i] || c.State != stackops.CondReady || c.Reason != "" || c.Message != "" {
			t.Errorf("condition %d = %+v, want %s ready", i, c, wantCodes[i])
		}
	}
	if a := r.Conditions[4].Applied; a == nil || a.Nodes != 2 || a.Total != 2 {
		t.Errorf("applied = %+v, want 2 of 2", a)
	}

	t.Run("not attached", func(t *testing.T) {
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, AttachedKnown: true})
		src.skipped = []ingressrender.Skipped{{Hostname: systemRouteHost, PathPrefix: "/", Reason: ingressrender.SkipSelfNotAttached}}
		src.bundle.Manifest.Routes = nil
		defer func() { *src = *systemServedBundle(applied...) }()
		got, _ := ingressRoutesOf(t, viewer)
		r, ok := systemRow(got.Routes)
		if !ok || r.State != stackops.RouteNotAttached || r.Reason != stackops.ReasonSelfNotAttached || len(r.Conditions) != 5 {
			t.Fatalf("row = %+v, want not_attached for self_not_attached", r)
		}
		if c := r.Conditions[1]; c.State != stackops.CondNotReady || c.Reason != stackops.ReasonSelfNotAttached || c.Message == "" {
			t.Errorf("network_attached = %+v, want not_ready self_not_attached with a sentence", c)
		}
		if c := r.Conditions[3]; c.State != stackops.CondReady {
			t.Errorf("certificate_ready = %+v: the self skip is not a certificate wait", c)
		}
	})

	t.Run("attachment not observed", func(t *testing.T) {
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Attached: true})
		got, _ := ingressRoutesOf(t, viewer)
		r, ok := systemRow(got.Routes)
		if !ok || r.State != stackops.RouteUnknown || len(r.Conditions) != 5 {
			t.Fatalf("row = %+v, want unknown", r)
		}
		for _, c := range r.Conditions[1:3] {
			if c.State != stackops.CondUnknown || c.Reason != stackops.ReasonNotObserved {
				t.Errorf("%s = %+v, want unknown not_observed", c.Code, c)
			}
		}
	})

	t.Run("no bundle yet", func(t *testing.T) {
		facts.set(e.ID, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereYes, On: true, Host: systemRouteHost, Attached: true, AttachedKnown: true})
		*src = routeSourcesFake{}
		defer func() { *src = *systemServedBundle(applied...) }()
		got, _ := ingressRoutesOf(t, viewer)
		r, _ := systemRow(got.Routes)
		if r.State != stackops.RoutePending || r.Conditions[3].State != stackops.CondUnknown || r.Conditions[4].State != stackops.CondUnknown {
			t.Errorf("row = %+v, want pending with the certificate and applied unknown", r)
		}
	})

}

// selfStatusAnswer is the selfRoute object of the ingress status.
type selfStatusAnswer struct {
	Here          string `json:"here"`
	On            bool   `json:"on"`
	Host          string `json:"host"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	DirectAddress string `json:"directAddress"`
	Kind          string `json:"kind"`
	Attached      bool   `json:"attached"`
	Network       string `json:"network"`
	Refusal       *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"refusal"`
	Snippet *struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"snippet"`
	CarveOut bool `json:"carveOut"`
}

// selfStatusOf reads an environment's ingress status as u and returns its selfRoute object, nil when the answer has none.
func selfStatusOf(t *testing.T, f *selfRouteFix, u credentialUser, envID string) *selfStatusAnswer {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, "/api/env/"+envID+"/ingress/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ingress/status: %d %s", rec.Code, rec.Body)
	}
	return decodeBody[struct {
		SelfRoute *selfStatusAnswer `json:"selfRoute"`
	}](t, rec).SelfRoute
}

// TestIngressStatusCarriesTheSelfRoute: the ingress status carries TierPlan's own route for its card (UI-SPEC A-01) in the
// environment TierPlan runs in or cannot tell, derived like the route's row, and nothing where TierPlan runs elsewhere. A
// Viewer reads the same object (ingress.read).
func TestIngressStatusCarriesTheSelfRoute(t *testing.T) {
	t.Parallel()
	const direct = "https://tierplan.example.org:7480"
	facts := &selfFactsFake{}
	src := &routeSourcesFake{}
	f := newSelfRouteFix(t, func(df *deployFix, _ *ingressdeploy.Deps) {
		df.stackSelfRoute, df.stackIngressRoutes = facts, src
	})
	viewer := f.asRoleOn(t, "self-viewer", f.envA, "viewer")
	b := ingressbundle.Bundle{ID: "b1"}
	b.Manifest.Routes = []ingressbundle.RouteEntry{{Hostname: selfRouteHost, PathPrefix: "/"}}
	*src = routeSourcesFake{bundle: b, have: true, nodes: []ingresshub.Node{{Status: "active", AppliedBundleID: "b1"}}}
	snippet := &ingressdeploy.SelfSnippet{Kind: ingressdeploy.SelfSnippetStackFile, Text: ingressdeploy.StackFileSnippet("tierplan")}

	t.Run("on and attached", func(t *testing.T) {
		facts.set(f.envA, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, On: true, Host: selfRouteHost, Direct: direct, Kind: ingressdeploy.SelfKindSwarmService,
			Attached: true, AttachedKnown: true, CarveOut: true,
		})
		for _, u := range []credentialUser{f.admin, viewer} {
			s := selfStatusOf(t, f, u, f.envA)
			if s == nil || s.Here != "yes" || !s.On || s.Host != selfRouteHost || s.State != stackops.RouteServing || s.Reason != "" ||
				!s.Attached || s.Network != ingress.SelfNetworkName || s.DirectAddress != direct || s.Kind != ingressdeploy.SelfKindSwarmService ||
				!s.CarveOut || s.Refusal != nil || s.Snippet != nil {
				t.Errorf("selfRoute = %+v, want on, attached and serving", s)
			}
		}
	})

	t.Run("off", func(t *testing.T) {
		facts.set(f.envA, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, Direct: direct, Kind: ingressdeploy.SelfKindSwarmService, AttachedKnown: true, Snippet: snippet,
		})
		s := selfStatusOf(t, f, viewer, f.envA)
		if s == nil || s.On || s.Host != "" || s.State != "off" || s.DirectAddress != direct || s.Snippet != nil {
			t.Errorf("selfRoute = %+v, want off with the direct address and no snippet", s)
		}
	})

	t.Run("not attached on a swarm", func(t *testing.T) {
		facts.set(f.envA, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, On: true, Host: selfRouteHost, Direct: direct, Kind: ingressdeploy.SelfKindSwarmService,
			AttachedKnown: true, Snippet: snippet,
		})
		s := selfStatusOf(t, f, viewer, f.envA)
		if s == nil || s.State != "self_not_attached" || s.Reason != stackops.ReasonSelfNotAttached || s.Attached {
			t.Fatalf("selfRoute = %+v, want self_not_attached", s)
		}
		if s.Snippet == nil || s.Snippet.Kind != ingressdeploy.SelfSnippetStackFile || s.Snippet.Text != snippet.Text {
			t.Errorf("snippet = %+v, want the stack file lines", s.Snippet)
		}
	})

	t.Run("attachment not observed yet", func(t *testing.T) {
		facts.set(f.envA, ingressdeploy.SelfRouteFacts{
			Here: ingressdeploy.SelfHereYes, On: true, Host: selfRouteHost, Kind: ingressdeploy.SelfKindSwarmService, Attached: true,
		})
		if s := selfStatusOf(t, f, viewer, f.envA); s == nil || s.State != "unknown" || s.Reason != "" {
			t.Errorf("selfRoute = %+v, want unknown, never serving", s)
		}
	})

	t.Run("detection cannot tell", func(t *testing.T) {
		refusal := ingressdeploy.Finding{Code: ingressdeploy.CodeSelfRouteNotHere, Message: "TierPlan can't tell whether it runs in this environment."}
		facts.set(f.envA, ingressdeploy.SelfRouteFacts{Here: ingressdeploy.SelfHereUnknown, Direct: direct, Refusal: &refusal})
		s := selfStatusOf(t, f, viewer, f.envA)
		if s == nil || s.Here != "unknown" || s.On || s.State != "off" || s.Kind != "" || s.Refusal == nil ||
			s.Refusal.Code != ingressdeploy.CodeSelfRouteNotHere || s.Refusal.Message != refusal.Message {
			t.Errorf("selfRoute = %+v, refusal %+v, want here unknown with the refusal", s, s.Refusal)
		}
	})

	t.Run("another environment", func(t *testing.T) {
		if s := selfStatusOf(t, f, f.admin, f.envB); s != nil {
			t.Errorf("selfRoute of the other environment = %+v, want none", s)
		}
	})
}

// TestIngressStatusSelfRouteFromTheDeployer: with no reader of its own the status reads the deployer's facts. The fixture's
// deployer cannot read TierPlan's own container, so it cannot tell whether TierPlan runs here: here unknown, with the refusal.
func TestIngressStatusSelfRouteFromTheDeployer(t *testing.T) {
	t.Parallel()
	f := newSelfRouteFix(t, nil)
	s := selfStatusOf(t, f, f.admin, f.envA)
	if s == nil || s.Here != "unknown" || s.State != "off" || s.Network != ingress.SelfNetworkName || s.Refusal == nil ||
		s.Refusal.Code != ingressdeploy.CodeSelfRouteNotHere || s.DirectAddress != selfRoutePublic {
		t.Errorf("selfRoute = %+v, want here unknown with self_route_not_here", s)
	}
}

// TestIngressPreviewNotices: the ingress switch-on and switch-off reviews carry the deployer's neutral notes (UI-SPEC S4): the
// HTTP-01 carve-out on the switch-on when TierPlan runs here on the self network and its own certificate renews over HTTP-01,
// and the release of TierPlan's own route on the switch-off while it is on. Neither carries a note otherwise.
func TestIngressPreviewNotices(t *testing.T) {
	t.Parallel()

	t.Run("switch-on", func(t *testing.T) {
		t.Parallel()
		for _, http01 := range []bool{true, false} {
			f := newDeployFixDeps(t, false, nil, func(_ *deployFix, d *ingressdeploy.Deps) {
				d.SelfURLs = func() ingressdeploy.SelfURLs {
					return ingressdeploy.SelfURLs{PublicURL: selfRoutePublic, ListenPort: 7480}
				}
				d.OwnHTTP01InUse = func(context.Context) (bool, error) { return http01, nil }
				// TierPlan runs on this swarm as a service on the self network.
				d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
					return envs.SwarmMembership{Known: true, Member: true, ClusterID: "cluster-1", NodeID: "node-1"}, nil
				}
				d.Self = func(context.Context) (ingressdeploy.SelfFacts, error) {
					return ingressdeploy.SelfFacts{
						ContainerID: "c-self", ServiceName: "tierplan_tierplan", ServiceID: "svc-self", StackNamespace: "tierplan",
						Networks: []string{ingress.SelfNetworkName}, Swarm: true,
					}, nil
				}
			})
			rec := f.credCall(t, f.adminA, http.MethodPost, "/api/env/"+f.envA+"/ingress/enable/preview", `{}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("enable preview: %d %s", rec.Code, rec.Body)
			}
			pv := decodeBody[struct {
				Notice   string `json:"notice"`
				Findings []struct {
					Code, Severity string
				} `json:"findings"`
			}](t, rec)
			for _, fd := range pv.Findings {
				if fd.Severity != "error" && fd.Severity != "warning" {
					t.Errorf("finding %+v has a severity outside the enum", fd)
				}
			}
			switch {
			case http01 && (!strings.Contains(pv.Notice, "HTTP-01") || !strings.Contains(pv.Notice, "tierplan.example.org")):
				t.Errorf("notice = %q, want the carve-out naming tierplan.example.org", pv.Notice)
			case !http01 && strings.Contains(rec.Body.String(), `"notice"`):
				t.Errorf("without HTTP-01 the preview carries a notice: %s", rec.Body)
			}
		}
	})

	t.Run("switch-off", func(t *testing.T) {
		t.Parallel()
		f := newSelfRouteFix(t, nil)
		path := "/api/env/" + f.envA + "/ingress/disable/preview"
		rec := f.credCall(t, f.adminA, http.MethodPost, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("disable preview: %d %s", rec.Code, rec.Body)
		}
		if body := rec.Body.String(); strings.Contains(body, `"notice"`) || strings.Contains(body, `"selfRouteHost"`) {
			t.Errorf("with the route off the preview carries a notice: %s", body)
		}

		f.claim(t)
		rec = f.credCall(t, f.adminA, http.MethodPost, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("disable preview: %d %s", rec.Code, rec.Body)
		}
		pv := decodeBody[struct {
			Notice        string `json:"notice"`
			SelfRouteHost string `json:"selfRouteHost"`
		}](t, rec)
		want := "TierPlan's own route on " + selfRouteHost + " is released first. Direct access on " + selfRoutePublic + " keeps working."
		if pv.Notice != want || pv.SelfRouteHost != selfRouteHost {
			t.Errorf("notice = %q, host %q; want %q, %q", pv.Notice, pv.SelfRouteHost, want, selfRouteHost)
		}
	})
}
