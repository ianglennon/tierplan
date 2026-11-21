// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"cmp"
	"slices"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// The states of a route, as the stack page reads them (D-24). They are derived on read and stored nowhere.
const (
	// RouteServing: every active node applied a bundle that holds the route.
	RouteServing = "serving"
	// RoutePending: the route is not in the current bundle yet, or not every active node has applied it.
	RoutePending = "pending"
	// RouteWaitingCertificate: the route is left out of the bundle until its hostname has a certificate; Reason says why.
	RouteWaitingCertificate = "waiting_certificate"
	// RouteBackendNotRunning: the service the route points at runs no replica, or the stack is stopped.
	RouteBackendNotRunning = "backend_not_running"
	// RouteNotAttached: the service is not on the ingress network, so the proxy cannot reach it.
	RouteNotAttached = "not_attached"
	// RouteIngressOff: the environment's ingress is switched off.
	RouteIngressOff = "ingress_off"
)

// RouteKey names a route by its hostname and path prefix, the pair the renderer's skipped list and the bundle's manifest
// name a route by. An empty path is the root path "/".
type RouteKey struct{ Hostname, Path string }

// routePath is the path prefix of a stored route, "/" for none.
func routePath(p string) string { return cmp.Or(p, "/") }

// RouteStateInput is everything the state of a stack's routes is derived from.
type RouteStateInput struct {
	// Enabled is true when the environment's ingress is switched on.
	Enabled bool
	// Stopped is true when a person stopped the stack: it keeps its hostnames and its routes read backend not running.
	Stopped bool
	// Routes are the routes the stack holds.
	Routes []ingress.Route
	// Running is how many replicas of each service run now, by compose service name; a service that is absent runs none.
	Running map[string]int
	// Attached says, by compose service name, whether the service is on the ingress network.
	Attached map[string]bool
	// Skipped is the reason each route the current bundle leaves out was left out (no_certificate, certificate_not_ready).
	Skipped map[RouteKey]string
	// InBundle says which routes the current bundle holds.
	InBundle map[RouteKey]bool
	// NodesApplied is true when there is at least one active node and every active node's applied bundle is the current one.
	NodesApplied bool
}

// RouteState is what one route is doing now.
type RouteState struct {
	Hostname, Path, Service string
	// State is one of the Route constants.
	State string
	// Reason is the certificate reason of a route that is waiting for one, otherwise empty.
	Reason string
	// Stopped is true when State is RouteBackendNotRunning because the stack is stopped.
	Stopped bool
}

// RouteStates derives the state of each route of a stack, in hostname then path order. The first test that holds
// decides: ingress off, then the backend not running (a stopped stack's routes all read so, since it keeps its claims),
// then not attached, then waiting for a certificate, then pending, and serving last.
func RouteStates(in RouteStateInput) []RouteState {
	out := make([]RouteState, 0, len(in.Routes))
	for _, r := range in.Routes {
		key := RouteKey{Hostname: r.Hostname, Path: routePath(r.PathPrefix)}
		s := RouteState{Hostname: key.Hostname, Path: key.Path, Service: r.Service}
		switch reason := in.Skipped[key]; {
		case !in.Enabled:
			s.State = RouteIngressOff
		case in.Stopped || in.Running[r.Service] <= 0:
			s.State, s.Stopped = RouteBackendNotRunning, in.Stopped
		case !in.Attached[r.Service]:
			s.State = RouteNotAttached
		case reason != "":
			s.State, s.Reason = RouteWaitingCertificate, reason
		case !in.InBundle[key] || !in.NodesApplied:
			s.State = RoutePending
		default:
			s.State = RouteServing
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RouteState) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Service, b.Service))
	})
	return out
}
