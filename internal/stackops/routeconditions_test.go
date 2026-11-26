// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// A route's conditions are the five things that have to hold before it serves, in a fixed order, each ready, not ready
// or unknown with a stable reason (plan 27-02, D-08). They are derived beside the single legacy state by the same
// function, so the stack page and the Ingress page cannot disagree (D-09).

// conditionOrder is the order every route's conditions come in.
var conditionOrder = []string{
	stackops.CondClaimHeld,
	stackops.CondNetworkAttached,
	stackops.CondBackendHealthy,
	stackops.CondCertificateReady,
	stackops.CondApplied,
}

// servingInput is a route that serves: ingress on, the service runs and is attached, the route is in the current bundle
// and both active nodes applied it.
func servingInput() stackops.RouteStateInput {
	key := stackops.RouteKey{Hostname: "app.example.com", Path: "/"}
	return stackops.RouteStateInput{
		Enabled:      true,
		Routes:       []ingress.Route{{Hostname: "app.example.com", PathPrefix: "/", Service: "web"}},
		Running:      map[string]int{"web": 1},
		Attached:     map[string]bool{"web": true},
		InBundle:     map[stackops.RouteKey]bool{key: true},
		NodesApplied: true,
		BundleKnown:  true,
		ActiveNodes:  2,
		AppliedNodes: 2,
	}
}

func TestRouteConditionsServingRoute(t *testing.T) {
	t.Parallel()

	got := stackops.RouteStates(servingInput())

	if len(got) != 1 {
		t.Fatalf("states = %+v, want one", got)
	}
	s := got[0]
	if s.State != stackops.RouteServing {
		t.Errorf("state = %q, want %q", s.State, stackops.RouteServing)
	}
	if len(s.Conditions) != len(conditionOrder) {
		t.Fatalf("conditions = %+v, want %d", s.Conditions, len(conditionOrder))
	}
	for i, c := range s.Conditions {
		if c.Code != conditionOrder[i] {
			t.Errorf("condition %d = %q, want %q", i, c.Code, conditionOrder[i])
		}
		if c.State != stackops.CondReady || c.Reason != "" {
			t.Errorf("condition %q = %q reason %q, want ready with no reason", c.Code, c.State, c.Reason)
		}
	}
	applied := s.Conditions[4]
	if applied.Applied != 2 || applied.Total != 2 {
		t.Errorf("applied = %d of %d, want 2 of 2", applied.Applied, applied.Total)
	}
}

// cond is the state and reason one condition is expected to carry.
type cond struct{ state, reason string }

func TestRouteConditionsTable(t *testing.T) {
	t.Parallel()
	key := stackops.RouteKey{Hostname: "app.example.com", Path: "/"}
	ok := cond{stackops.CondReady, ""}
	off := cond{stackops.CondUnknown, stackops.ReasonIngressOff}
	tests := []struct {
		name       string
		mutate     func(*stackops.RouteStateInput)
		wantState  string
		wantStop   bool
		want       [5]cond
		wantCounts [2]int
		wantMsg    string
	}{
		{"serving", func(*stackops.RouteStateInput) {}, stackops.RouteServing, false,
			[5]cond{ok, ok, ok, ok, ok}, [2]int{2, 2}, ""},
		{"ingress off", func(in *stackops.RouteStateInput) { in.Enabled = false }, stackops.RouteIngressOff, false,
			[5]cond{ok, off, off, off, off}, [2]int{2, 2}, ""},
		{"stopped stack", func(in *stackops.RouteStateInput) { in.Stopped = true }, stackops.RouteBackendNotRunning, true,
			[5]cond{ok, {stackops.CondUnknown, stackops.ReasonStackStopped}, {stackops.CondNotReady, stackops.ReasonStackStopped}, ok, ok}, [2]int{2, 2}, ""},
		{"two faults: off the network and no replica keep the legacy state", func(in *stackops.RouteStateInput) {
			in.Attached = map[string]bool{"web": false}
			in.Running = map[string]int{"web": 0}
		}, stackops.RouteBackendNotRunning, false,
			[5]cond{ok, {stackops.CondNotReady, stackops.ReasonNotAttached}, {stackops.CondNotReady, stackops.ReasonNoRunningTask}, ok, ok}, [2]int{2, 2}, ""},
		{"not attached only", func(in *stackops.RouteStateInput) { in.Attached = map[string]bool{"web": false} }, stackops.RouteNotAttached, false,
			[5]cond{ok, {stackops.CondNotReady, stackops.ReasonNotAttached}, ok, ok, ok}, [2]int{2, 2}, ""},
		{"no running task only", func(in *stackops.RouteStateInput) { in.Running = nil }, stackops.RouteBackendNotRunning, false,
			[5]cond{ok, ok, {stackops.CondNotReady, stackops.ReasonNoRunningTask}, ok, ok}, [2]int{2, 2}, ""},
		{"waiting for a certificate: none yet", func(in *stackops.RouteStateInput) {
			in.Skipped = map[stackops.RouteKey]string{key: "no_certificate"}
			in.InBundle = nil
			in.CertMessages = map[string]string{"app.example.com": "ignored for this reason"}
		}, stackops.RouteWaitingCertificate, false,
			[5]cond{ok, ok, ok, {stackops.CondNotReady, stackops.ReasonNoCertificate}, {stackops.CondNotReady, stackops.ReasonNotInBundle}}, [2]int{2, 2}, ""},
		{"waiting for a certificate: not ready, with the server's sentence", func(in *stackops.RouteStateInput) {
			in.Skipped = map[stackops.RouteKey]string{key: "certificate_not_ready"}
			in.InBundle = nil
			in.CertMessages = map[string]string{"app.example.com": "the challenge is still pending"}
		}, stackops.RouteWaitingCertificate, false,
			[5]cond{ok, ok, ok, {stackops.CondNotReady, stackops.ReasonCertificateNotReady}, {stackops.CondNotReady, stackops.ReasonNotInBundle}}, [2]int{2, 2}, "the challenge is still pending"},
		{"no bundle yet", func(in *stackops.RouteStateInput) {
			in.BundleKnown, in.InBundle, in.NodesApplied = false, nil, false
		}, stackops.RoutePending, false,
			[5]cond{ok, ok, ok, {stackops.CondUnknown, stackops.ReasonNoBundle}, {stackops.CondUnknown, stackops.ReasonNoBundle}}, [2]int{2, 2}, ""},
		{"not in the bundle", func(in *stackops.RouteStateInput) { in.InBundle = nil }, stackops.RoutePending, false,
			[5]cond{ok, ok, ok, ok, {stackops.CondNotReady, stackops.ReasonNotInBundle}}, [2]int{2, 2}, ""},
		{"no active nodes", func(in *stackops.RouteStateInput) {
			in.ActiveNodes, in.AppliedNodes, in.NodesApplied = 0, 0, false
		}, stackops.RoutePending, false,
			[5]cond{ok, ok, ok, ok, {stackops.CondNotReady, stackops.ReasonNoNodes}}, [2]int{0, 0}, ""},
		{"nodes behind", func(in *stackops.RouteStateInput) {
			in.ActiveNodes, in.AppliedNodes, in.NodesApplied = 3, 1, false
		}, stackops.RoutePending, false,
			[5]cond{ok, ok, ok, ok, {stackops.CondNotReady, stackops.ReasonNodesBehind}}, [2]int{1, 3}, ""},
		{"engine unreadable", func(in *stackops.RouteStateInput) {
			in.EngineUnreadable, in.Running, in.Attached = true, nil, nil
		}, stackops.RouteBackendNotRunning, false,
			[5]cond{ok, {stackops.CondUnknown, stackops.ReasonEngineUnreadable}, {stackops.CondUnknown, stackops.ReasonEngineUnreadable}, ok, ok}, [2]int{2, 2}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := servingInput()
			tc.mutate(&in)

			got := stackops.RouteStates(in)

			if len(got) != 1 {
				t.Fatalf("states = %+v, want one", got)
			}
			s := got[0]
			if s.State != tc.wantState || s.Stopped != tc.wantStop {
				t.Errorf("state = %q stopped %v, want %q %v", s.State, s.Stopped, tc.wantState, tc.wantStop)
			}
			if len(s.Conditions) != len(conditionOrder) {
				t.Fatalf("conditions = %+v, want %d", s.Conditions, len(conditionOrder))
			}
			for i, c := range s.Conditions {
				if c.Code != conditionOrder[i] {
					t.Errorf("condition %d = %q, want %q", i, c.Code, conditionOrder[i])
				}
				if c.State != tc.want[i].state || c.Reason != tc.want[i].reason {
					t.Errorf("condition %q = %q reason %q, want %q %q", c.Code, c.State, c.Reason, tc.want[i].state, tc.want[i].reason)
				}
				// A condition that is ready or whose input was not read never carries a sentence.
				if c.Message != "" && c.Code != stackops.CondCertificateReady {
					t.Errorf("condition %q message = %q, want none", c.Code, c.Message)
				}
			}
			if msg := s.Conditions[3].Message; msg != tc.wantMsg {
				t.Errorf("certificate message = %q, want %q", msg, tc.wantMsg)
			}
			if a := s.Conditions[4]; a.Applied != tc.wantCounts[0] || a.Total != tc.wantCounts[1] {
				t.Errorf("applied = %d of %d, want %d of %d", a.Applied, a.Total, tc.wantCounts[0], tc.wantCounts[1])
			}
			// A condition that was not ready or not read never lets a route read serving.
			if s.State == stackops.RouteServing {
				for _, c := range s.Conditions {
					if c.State != stackops.CondReady {
						t.Errorf("serving route has %q %q", c.Code, c.State)
					}
				}
			}
		})
	}
}

func TestRouteConditionsAdjacentPaths(t *testing.T) {
	t.Parallel()
	in := servingInput()
	in.Routes = []ingress.Route{
		{Hostname: "app.example.com", PathPrefix: "/api", Service: "api"},
		{Hostname: "app.example.com", PathPrefix: "/", Service: "web"},
	}
	in.Running = map[string]int{"web": 1, "api": 0}
	in.Attached = map[string]bool{"web": true, "api": true}
	in.InBundle = map[stackops.RouteKey]bool{
		{Hostname: "app.example.com", Path: "/"}:    true,
		{Hostname: "app.example.com", Path: "/api"}: true,
	}

	got := stackops.RouteStates(in)

	if len(got) != 2 {
		t.Fatalf("states = %+v, want two", got)
	}
	if got[0].Path != "/" || got[1].Path != "/api" {
		t.Fatalf("paths = %q, %q, want / then /api", got[0].Path, got[1].Path)
	}
	for _, s := range got {
		if len(s.Conditions) != len(conditionOrder) {
			t.Fatalf("%s conditions = %+v, want %d", s.Path, s.Conditions, len(conditionOrder))
		}
		for i, c := range s.Conditions {
			if c.Code != conditionOrder[i] {
				t.Errorf("%s condition %d = %q, want %q", s.Path, i, c.Code, conditionOrder[i])
			}
		}
	}
	// Each route has its own conditions: the root serves, the api path has no running task.
	if got[0].State != stackops.RouteServing || got[0].Conditions[2].State != stackops.CondReady {
		t.Errorf("/ = %q backend %q, want serving and ready", got[0].State, got[0].Conditions[2].State)
	}
	if got[1].State != stackops.RouteBackendNotRunning || got[1].Conditions[2].Reason != stackops.ReasonNoRunningTask {
		t.Errorf("/api = %q backend reason %q, want backend_not_running and no_running_task", got[1].State, got[1].Conditions[2].Reason)
	}
}
