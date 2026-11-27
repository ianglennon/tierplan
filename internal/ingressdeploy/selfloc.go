// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

// CodeSelfRouteNotHere refuses to switch TierPlan's own route on in an environment TierPlan does not run in, or when it
// cannot be told whether it does: a route that guesses wrong sends the hostname to a place TierPlan cannot be reached
// from (D-01).
const CodeSelfRouteNotHere = "self_route_not_here"

// CodeSelfRouteNeedsService refuses to switch TierPlan's own route on when TierPlan runs as a plain container on a
// swarm node: the route needs a swarm service with the self network attached (D-03).
const CodeSelfRouteNeedsService = "self_route_needs_service"

// SelfFacts are what the local engine says about the container TierPlan runs in (D-03, D-04, D-05).
type SelfFacts struct {
	// ContainerID is TierPlan's own container ID.
	ContainerID string
	// ServiceName, ServiceID and StackNamespace are the swarm task labels of the container; they are empty for a
	// container that is not a swarm task, and the namespace for a service that is in no stack.
	ServiceName, ServiceID, StackNamespace string
	// Networks are the names of the networks the container is attached to.
	Networks []string
	// Swarm is true when the container carries swarm task labels, so it is one task of a swarm service.
	Swarm bool
}

// SelfURLs are the instance's own addresses, their environment locks and the port TIERPLAN_LISTEN names, for the route's
// backend and the checks that read the public URL (D-07).
type SelfURLs struct {
	PublicURL, AgentURL       string
	PublicLocked, AgentLocked bool
	// ListenPort is the port the server listens on, 0 when it is not known.
	ListenPort int
}
