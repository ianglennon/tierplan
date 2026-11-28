// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import "github.com/ianglennon/tierplan/internal/ingress"

// The conditions of a route, in the order a route's conditions are always returned (D-08).
const (
	// CondClaimHeld: the stack holds the claim the route sits under.
	CondClaimHeld = "claim_held"
	// CondNetworkAttached: the service is on the ingress network, so the proxy can reach it.
	CondNetworkAttached = "network_attached"
	// CondBackendHealthy: the service runs at least one replica (or container).
	CondBackendHealthy = "backend_healthy"
	// CondCertificateReady: the route's hostname has a certificate the bundle can carry.
	CondCertificateReady = "certificate_ready"
	// CondApplied: every active node applied a bundle that holds the route.
	CondApplied = "applied"
)

// The states a condition is in. Unknown is a condition whose input was not read; it is never reported ready.
const (
	CondReady    = "ready"
	CondNotReady = "not_ready"
	CondUnknown  = "unknown"
)

// The stable reason codes of a condition that is not ready or unknown. A ready condition has no reason. The two
// certificate reasons are the renderer's skipped reasons, spelled the same.
const (
	ReasonNotAttached         = "not_attached"
	ReasonStackStopped        = "stack_stopped"
	ReasonNoRunningTask       = "no_running_task"
	ReasonNoCertificate       = "no_certificate"
	ReasonCertificateNotReady = "certificate_not_ready"
	ReasonNotInBundle         = "not_in_bundle"
	ReasonNoNodes             = "no_nodes"
	ReasonNodesBehind         = "nodes_behind"
	ReasonIngressOff          = "ingress_off"
	ReasonEngineUnreadable    = "engine_unreadable"
	ReasonNoBundle            = "no_bundle"
)

// Condition is one of a route's five conditions.
type Condition struct {
	// Code is one of the Cond constants naming the condition.
	Code string
	// State is CondReady, CondNotReady or CondUnknown.
	State string
	// Reason is a stable Reason code, empty when the condition is ready.
	Reason string
	// Message is the hint of the failure of a certificate that is not ready because its last order failed, otherwise
	// empty.
	Message string
	// Applied and Total are the active nodes that applied the bundle and the active nodes there are. They are set on the
	// CondApplied condition only.
	Applied, Total int
}

// ready is a condition that holds.
func ready(code string) Condition { return Condition{Code: code, State: CondReady} }

// notReady is a condition that was read and does not hold.
func notReady(code, reason string) Condition {
	return Condition{Code: code, State: CondNotReady, Reason: reason}
}

// unknown is a condition whose input was not read.
func unknown(code, reason string) Condition {
	return Condition{Code: code, State: CondUnknown, Reason: reason}
}

// routeConditions derives the five conditions of one route, in order. It reads the same input RouteStates does, and its
// answers never contradict the legacy state: a route is serving only when all five are ready. A condition whose input
// was not read (ingress off, the engine unreadable, no bundle yet) is unknown with a reason, never ready.
func routeConditions(in RouteStateInput, r ingress.Route, key RouteKey) []Condition {
	// A route row exists only under a claim, so the claim is held for every route there is.
	claim := ready(CondClaimHeld)
	applied := withCounts(appliedCondition(in, key), in)

	if !in.Enabled {
		return []Condition{
			claim,
			unknown(CondNetworkAttached, ReasonIngressOff),
			unknown(CondBackendHealthy, ReasonIngressOff),
			unknown(CondCertificateReady, ReasonIngressOff),
			withCounts(unknown(CondApplied, ReasonIngressOff), in),
		}
	}

	var attached, healthy Condition
	switch {
	case in.Stopped:
		attached = unknown(CondNetworkAttached, ReasonStackStopped)
		healthy = notReady(CondBackendHealthy, ReasonStackStopped)
	case in.EngineUnreadable:
		attached = unknown(CondNetworkAttached, ReasonEngineUnreadable)
		healthy = unknown(CondBackendHealthy, ReasonEngineUnreadable)
	default:
		attached = ready(CondNetworkAttached)
		if !in.Attached[r.Service] {
			attached = notReady(CondNetworkAttached, ReasonNotAttached)
		}
		healthy = ready(CondBackendHealthy)
		if in.Running[r.Service] <= 0 {
			healthy = notReady(CondBackendHealthy, ReasonNoRunningTask)
		}
	}

	return []Condition{claim, attached, healthy, certificateCondition(in, key), applied}
}

// withCounts sets the applied count of a condition from the input.
func withCounts(c Condition, in RouteStateInput) Condition {
	c.Applied, c.Total = in.AppliedNodes, in.ActiveNodes
	return c
}

// certificateCondition is whether the route's hostname has a certificate the bundle carries. The renderer's skipped
// reason is the answer; without a bundle there is nothing to read it from.
func certificateCondition(in RouteStateInput, key RouteKey) Condition {
	if !in.BundleKnown {
		return unknown(CondCertificateReady, ReasonNoBundle)
	}
	reason := in.Skipped[key]
	if reason == "" {
		return ready(CondCertificateReady)
	}
	c := notReady(CondCertificateReady, reason)
	if reason == ReasonCertificateNotReady {
		c.Message = in.CertMessages[key.Hostname]
	}
	return c
}

// appliedCondition is whether every active node applied a bundle that holds the route.
func appliedCondition(in RouteStateInput, key RouteKey) Condition {
	switch {
	case !in.BundleKnown:
		return unknown(CondApplied, ReasonNoBundle)
	case !in.InBundle[key]:
		return notReady(CondApplied, ReasonNotInBundle)
	case in.NodesUnreadable:
		return unknown(CondApplied, ReasonEngineUnreadable)
	case in.ActiveNodes <= 0:
		return notReady(CondApplied, ReasonNoNodes)
	case in.AppliedNodes < in.ActiveNodes:
		return notReady(CondApplied, ReasonNodesBehind)
	default:
		return ready(CondApplied)
	}
}

// The reasons only TierPlan's own route (a system route) gives. Its backend is TierPlan itself, reached over the tierplan-self
// network, so what its network and backend conditions say comes from whether TierPlan is on that network, not from a stack's
// services.
const (
	// ReasonSelfNotAttached: TierPlan is not on the tierplan-self network, so the proxy cannot reach it. It is spelled as the
	// renderer's skipped reason for the route (ingressrender.SkipSelfNotAttached).
	ReasonSelfNotAttached = "self_not_attached"
	// ReasonNotObserved: no pass has read whether TierPlan is on the tierplan-self network since it started.
	ReasonNotObserved = "not_observed"
	// ReasonSelfInputInvalid: what TierPlan knows of itself cannot be rendered as its route (a base path in the public URL,
	// or no name of TierPlan's on the self network), so the proxy has no way to reach it. It is spelled as the renderer's
	// skipped reason (ingressrender.SkipSelfInputInvalid); the environment's stack routes are rendered all the same (WR-09).
	ReasonSelfInputInvalid = "self_input_invalid"
)

// RouteUnknown is the state of a route whose conditions could not all be read, so whether it serves is not known.
const RouteUnknown = "unknown"

// SystemRouteConditions derives the five conditions of TierPlan's own route, in the order every route's come in (Phase 28
// A-07). The claim is held, since the row exists only under the system claim. The network is attached when TierPlan is on
// the tierplan-self network; attachedKnown false (no observation yet) leaves it unknown, never ready. The backend is TierPlan
// itself, healthy once the proxy can reach it. The certificate and applied conditions read the same input a stack route's
// do, so the two never disagree about the bundle.
func SystemRouteConditions(in RouteStateInput, r ingress.Route, attached, attachedKnown bool) []Condition {
	key := RouteKey{Hostname: r.Hostname, Path: routePath(r.PathPrefix)}
	claim := ready(CondClaimHeld)
	if !in.Enabled {
		return []Condition{
			claim,
			unknown(CondNetworkAttached, ReasonIngressOff),
			unknown(CondBackendHealthy, ReasonIngressOff),
			unknown(CondCertificateReady, ReasonIngressOff),
			withCounts(unknown(CondApplied, ReasonIngressOff), in),
		}
	}
	var network, backend Condition
	switch {
	case !attachedKnown:
		network, backend = unknown(CondNetworkAttached, ReasonNotObserved), unknown(CondBackendHealthy, ReasonNotObserved)
	case !attached:
		// The proxy cannot reach TierPlan, so whether TierPlan answers it is not known.
		network, backend = notReady(CondNetworkAttached, ReasonSelfNotAttached), unknown(CondBackendHealthy, ReasonSelfNotAttached)
	default:
		network, backend = ready(CondNetworkAttached), ready(CondBackendHealthy)
	}
	cert := certificateCondition(in, key)
	if in.BundleKnown && in.Skipped[key] == ReasonSelfInputInvalid {
		// The route is left out for TierPlan's own input, not for its certificate, which was then not looked at.
		backend, cert = notReady(CondBackendHealthy, ReasonSelfInputInvalid), unknown(CondCertificateReady, ReasonSelfInputInvalid)
	}
	return []Condition{claim, network, backend, cert, withCounts(appliedCondition(in, key), in)}
}

// SystemRouteState is the state and reason of a system route from its conditions, with RouteStates' precedence: ingress off,
// then the backend not running, then not attached (reason self_not_attached), then unknown when the network or the backend
// could not be read, then waiting for a certificate (reason the certificate's), then pending, and serving last. A condition
// list that is not the five of SystemRouteConditions is unknown.
func SystemRouteState(conds []Condition) (state, reason string) {
	by := map[string]Condition{}
	for _, c := range conds {
		by[c.Code] = c
	}
	network, backend, cert, applied := by[CondNetworkAttached], by[CondBackendHealthy], by[CondCertificateReady], by[CondApplied]
	if len(by) != 5 || by[CondClaimHeld].State != CondReady {
		return RouteUnknown, ""
	}
	switch {
	case network.Reason == ReasonIngressOff:
		return RouteIngressOff, ""
	case backend.State == CondNotReady:
		return RouteBackendNotRunning, ""
	case network.State == CondNotReady:
		return RouteNotAttached, network.Reason
	case network.State == CondUnknown || backend.State == CondUnknown:
		return RouteUnknown, ""
	case cert.State == CondNotReady:
		return RouteWaitingCertificate, cert.Reason
	case cert.State != CondReady || applied.State != CondReady:
		return RoutePending, ""
	}
	return RouteServing, ""
}
