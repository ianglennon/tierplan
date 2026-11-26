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
	// Message is the server's own sentence for a certificate that is not ready, otherwise empty.
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
	case in.ActiveNodes <= 0:
		return notReady(CondApplied, ReasonNoNodes)
	case in.AppliedNodes < in.ActiveNodes:
		return notReady(CondApplied, ReasonNodesBehind)
	default:
		return ready(CondApplied)
	}
}
