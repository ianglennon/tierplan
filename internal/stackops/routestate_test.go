// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The state of a route is derived on read from what is stored, what runs and what the nodes applied, and kept
// nowhere (plan 24-25, D-24). The precedence is ingress_off, backend_not_running, not_attached, waiting_certificate,
// pending, serving.

func TestRouteStateMatrix(t *testing.T) {
	t.Parallel()
	web := ingress.Route{Hostname: "app.example.com", PathPrefix: "/", Service: "web"}
	key := stackops.RouteKey{Hostname: "app.example.com", Path: "/"}
	// base is a route that serves: ingress on, the service runs and is attached, the route is in the bundle and every
	// active node applied it.
	base := func() stackops.RouteStateInput {
		return stackops.RouteStateInput{
			Enabled:      true,
			Routes:       []ingress.Route{web},
			Running:      map[string]int{"web": 2},
			Attached:     map[string]bool{"web": true},
			InBundle:     map[stackops.RouteKey]bool{key: true},
			NodesApplied: true,
		}
	}
	tests := []struct {
		name        string
		mutate      func(*stackops.RouteStateInput)
		wantState   string
		wantReason  string
		wantStopped bool
	}{
		{"serving", func(*stackops.RouteStateInput) {}, stackops.RouteServing, "", false},
		{"pending: a node has not applied the bundle", func(in *stackops.RouteStateInput) { in.NodesApplied = false }, stackops.RoutePending, "", false},
		{"pending: the route is not in the bundle yet", func(in *stackops.RouteStateInput) { in.InBundle = nil }, stackops.RoutePending, "", false},
		{"waiting for a certificate: none yet", func(in *stackops.RouteStateInput) {
			in.Skipped = map[stackops.RouteKey]string{key: "no_certificate"}
			in.InBundle = nil
		}, stackops.RouteWaitingCertificate, "no_certificate", false},
		{"waiting for a certificate: not ready", func(in *stackops.RouteStateInput) {
			in.Skipped = map[stackops.RouteKey]string{key: "certificate_not_ready"}
			in.InBundle = nil
		}, stackops.RouteWaitingCertificate, "certificate_not_ready", false},
		{"backend not running: no replica", func(in *stackops.RouteStateInput) { in.Running = map[string]int{"web": 0} }, stackops.RouteBackendNotRunning, "", false},
		{"backend not running: the service is not on the swarm", func(in *stackops.RouteStateInput) { in.Running = nil }, stackops.RouteBackendNotRunning, "", false},
		{"backend not running: the stack is stopped", func(in *stackops.RouteStateInput) { in.Stopped = true }, stackops.RouteBackendNotRunning, "", true},
		{"not attached", func(in *stackops.RouteStateInput) { in.Attached = map[string]bool{"web": false} }, stackops.RouteNotAttached, "", false},
		{"ingress off", func(in *stackops.RouteStateInput) { in.Enabled = false }, stackops.RouteIngressOff, "", false},

		// Precedence.
		{"ingress off beats stopped", func(in *stackops.RouteStateInput) { in.Enabled, in.Stopped = false, true }, stackops.RouteIngressOff, "", false},
		{"stopped beats not attached", func(in *stackops.RouteStateInput) {
			in.Stopped = true
			in.Attached = map[string]bool{"web": false}
		}, stackops.RouteBackendNotRunning, "", true},
		{"not running beats not attached", func(in *stackops.RouteStateInput) {
			in.Running = map[string]int{"web": 0}
			in.Attached = map[string]bool{"web": false}
		}, stackops.RouteBackendNotRunning, "", false},
		{"not attached beats waiting", func(in *stackops.RouteStateInput) {
			in.Attached = map[string]bool{"web": false}
			in.Skipped = map[stackops.RouteKey]string{key: "no_certificate"}
		}, stackops.RouteNotAttached, "", false},
		{"waiting beats pending", func(in *stackops.RouteStateInput) {
			in.NodesApplied = false
			in.Skipped = map[stackops.RouteKey]string{key: "certificate_not_ready"}
		}, stackops.RouteWaitingCertificate, "certificate_not_ready", false},
		{"a route with an empty path is the root path", func(in *stackops.RouteStateInput) {
			in.Routes = []ingress.Route{{Hostname: "app.example.com", PathPrefix: "", Service: "web"}}
		}, stackops.RouteServing, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := base()
			tc.mutate(&in)

			got := stackops.RouteStates(in)

			if len(got) != 1 {
				t.Fatalf("states = %+v, want one", got)
			}
			s := got[0]
			if s.State != tc.wantState || s.Reason != tc.wantReason || s.Stopped != tc.wantStopped {
				t.Errorf("state = %q reason %q stopped %v, want %q %q %v", s.State, s.Reason, s.Stopped, tc.wantState, tc.wantReason, tc.wantStopped)
			}
			if s.Hostname != "app.example.com" || s.Path != "/" || s.Service != "web" {
				t.Errorf("route = %+v, want app.example.com / web", s)
			}
		})
	}
}

func TestRouteStatesAreSortedByHostnameThenPath(t *testing.T) {
	t.Parallel()
	in := stackops.RouteStateInput{
		Enabled: true,
		Routes: []ingress.Route{
			{Hostname: "b.example.com", PathPrefix: "/", Service: "web"},
			{Hostname: "a.example.com", PathPrefix: "/zz", Service: "web"},
			{Hostname: "a.example.com", PathPrefix: "/", Service: "web"},
		},
	}

	got := stackops.RouteStates(in)

	var order []string
	for _, s := range got {
		order = append(order, s.Hostname+s.Path)
	}
	want := []string{"a.example.com/", "a.example.com/zz", "b.example.com/"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
