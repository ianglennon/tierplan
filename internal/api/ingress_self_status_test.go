// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
