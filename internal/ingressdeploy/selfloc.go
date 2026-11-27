// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"fmt"
	"slices"

	"github.com/ianglennon/tierplan/internal/envs"
)

// selfNetworkName is the network TierPlan's own container is attached to for its route. 28-08's exported constant replaces
// this one when the ensure loop is wired (28-22).
const selfNetworkName = "tierplan-self"

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

// SelfHere is the answer to "does TierPlan run in this environment".
type SelfHere string

const (
	// SelfHereYes is TierPlan runs in the environment.
	SelfHereYes SelfHere = "yes"
	// SelfHereNo is TierPlan runs somewhere else.
	SelfHereNo SelfHere = "no"
	// SelfHereUnknown is the local membership or the self read cannot be told, so nothing is assumed.
	SelfHereUnknown SelfHere = "unknown"
)

// The kinds of place TierPlan can run in, for SelfPlace.Kind.
const (
	SelfKindSwarmService   = "swarm_service"
	SelfKindSwarmContainer = "swarm_container"
	SelfKindStandalone     = "standalone"
)

// SelfPlace is where TierPlan runs, as far as it could be told.
type SelfPlace struct {
	Here SelfHere
	// Kind is SelfKindSwarmService, SelfKindSwarmContainer (a plain container on a swarm node) or SelfKindStandalone; it is
	// empty unless Here is yes.
	Kind string
	// EnvName is the environment TierPlan runs in, when it is known.
	EnvName string
	// ServiceName, ServiceID and Namespace are TierPlan's own swarm service and stack, for a swarm service.
	ServiceName, ServiceID, Namespace string
	// ContainerID is TierPlan's own container, for a standalone host.
	ContainerID string
	// Attached is true when the container is on the self network.
	Attached bool
	// EnvHere is true when the engine says the environment is the one TierPlan runs in, even if TierPlan's own container
	// could not be read, so Here is unknown. The HTTP-01 checks refuse on it rather than warn: TierPlan's renewal is on this
	// port 80 whatever its container reads.
	EnvHere bool
}

// SelfEnvironmentNamer is the optional second face of Deps.Envs that names the environment TierPlan runs in, so a refusal
// can say where the route can be switched on. An Envs without it makes the refusal say TierPlan cannot tell.
type SelfEnvironmentNamer interface {
	SelfEnvironmentName(ctx context.Context) (string, bool)
}

const (
	selfNotHereMessage = "TierPlan's own route can only be switched on in the environment TierPlan runs in: %s. A route here would send the hostname to a place TierPlan can't be reached from."
	selfUnknownMessage = "TierPlan can't tell which environment it runs in, so it won't switch the route on. A wrong guess would leave the hostname unable to reach TierPlan."
	selfNeedsService   = "TierPlan runs as a plain container on a swarm node. The route needs it to run as a swarm service, with the tierplan-self network attached. Deploy TierPlan from the stack file in install-in-swarm.md, then try again."
)

// SelfLocation says whether TierPlan runs in the environment, as what, and whether it is on the self network (D-01, D-03,
// D-04, D-05). Anything it cannot tell answers unknown with a refusal, never yes: a wrong guess would make a route that cannot
// reach its backend. The findings are errors, so the switch is refused rather than warned. They are empty when TierPlan runs
// here as a swarm service or a standalone container.
func (s *Service) SelfLocation(ctx context.Context, info EnvInfo) (SelfPlace, []Finding, error) {
	if s.d.Self == nil {
		return s.selfUnknown(), selfRefusal(selfUnknownMessage), nil
	}
	var here SelfHere
	if info.Swarm {
		here = s.swarmHere(ctx, info)
	} else {
		here = s.standaloneHere(ctx, info)
	}
	switch here {
	case SelfHereNo:
		if name, ok := s.selfEnvironmentName(ctx); ok {
			return SelfPlace{Here: SelfHereNo, EnvName: name}, selfRefusal(fmt.Sprintf(selfNotHereMessage, name)), nil
		}
		return SelfPlace{Here: SelfHereNo}, selfRefusal(selfUnknownMessage), nil
	case SelfHereUnknown:
		return s.selfUnknown(), selfRefusal(selfUnknownMessage), nil
	}
	facts, err := s.d.Self(ctx)
	if err != nil {
		s.log().Warn("could not read TierPlan's own container for its route", "error", err)
		return s.selfUnknownHere(), selfRefusal(selfUnknownMessage), nil
	}
	place := SelfPlace{Here: SelfHereYes, EnvName: info.Name, Attached: slices.Contains(facts.Networks, selfNetworkName), EnvHere: true}
	if info.Swarm {
		if !facts.Swarm {
			place.Kind = SelfKindSwarmContainer
			place.ContainerID = facts.ContainerID
			return place, []Finding{{Code: CodeSelfRouteNeedsService, Message: selfNeedsService, Severity: SeverityError}}, nil
		}
		place.Kind = SelfKindSwarmService
		place.ServiceName, place.ServiceID, place.Namespace = facts.ServiceName, facts.ServiceID, facts.StackNamespace
		return place, nil, nil
	}
	if facts.ContainerID == "" {
		// A host TierPlan is said to run on, with no container found for it: nothing to put the route on.
		return s.selfUnknownHere(), selfRefusal(selfUnknownMessage), nil
	}
	place.Kind = SelfKindStandalone
	place.ContainerID = facts.ContainerID
	return place, nil, nil
}

func (s *Service) selfUnknown() SelfPlace { return SelfPlace{Here: SelfHereUnknown} }

// selfUnknownHere is an environment the engine says TierPlan runs in, whose own container could not be read.
func (s *Service) selfUnknownHere() SelfPlace { return SelfPlace{Here: SelfHereUnknown, EnvHere: true} }

func selfRefusal(msg string) []Finding {
	return []Finding{{Code: CodeSelfRouteNotHere, Message: msg, Severity: SeverityError}}
}

func (s *Service) selfEnvironmentName(ctx context.Context) (string, bool) {
	n, ok := s.d.Envs.(SelfEnvironmentNamer)
	if !ok {
		return "", false
	}
	name, ok := n.SelfEnvironmentName(ctx)
	return name, ok && name != ""
}

// localMembership reads the server's own engine, and whether that answered.
func (s *Service) localMembership(ctx context.Context) (envs.SwarmMembership, bool) {
	if s.d.LocalSwarm == nil {
		return envs.SwarmMembership{}, false
	}
	local, err := s.d.LocalSwarm(ctx)
	if err != nil {
		s.log().Warn("could not read TierPlan's own engine for its route", "error", err)
		return envs.SwarmMembership{}, false
	}
	return local, local.Known
}

// swarmHere is whether TierPlan runs on the swarm of the environment: its engine's cluster is the environment's, or, for a
// worker (which does not learn the cluster) or a manager of an environment whose cluster is not known, its node is one of the
// environment's. They are the only copy of these rules: checkHTTP01 reads them through SelfLocation.
func (s *Service) swarmHere(ctx context.Context, info EnvInfo) SelfHere {
	local, ok := s.localMembership(ctx)
	if !ok {
		return SelfHereUnknown
	}
	if !local.Member {
		// A standalone engine: TierPlan is on no swarm node.
		return SelfHereNo
	}
	match, known := sameSwarm(local, info)
	switch {
	case match:
		return SelfHereYes
	case known:
		return SelfHereNo
	}
	return SelfHereUnknown
}

// sameSwarm compares the local node with the environment's swarm: whether it is the same swarm, and whether that could be
// told. A worker of some swarm whose node the environment does not list, or a manager of an environment whose cluster is not
// known, may or may not be on it, so that is not known.
func sameSwarm(local envs.SwarmMembership, info EnvInfo) (match, known bool) {
	sameCluster := local.ClusterID != "" && info.ClusterID != "" && local.ClusterID == info.ClusterID
	otherCluster := local.ClusterID != "" && info.ClusterID != "" && local.ClusterID != info.ClusterID
	ownNode := false
	for _, n := range info.Nodes {
		if local.NodeID != "" && n.SwarmNodeID == local.NodeID {
			ownNode = true
		}
	}
	switch {
	case sameCluster || ownNode:
		return true, true
	case otherCluster:
		return false, true
	}
	return false, false
}

// standaloneHere is whether TierPlan runs on the standalone host: the environment is TierPlan's own local socket, or its
// engine is the one TierPlan's engine is. They are the only copy of these rules: checkStandaloneHTTP01 reads them through
// SelfLocation.
func (s *Service) standaloneHere(ctx context.Context, info EnvInfo) SelfHere {
	if info.Local {
		return SelfHereYes
	}
	if info.EngineID == "" {
		return SelfHereUnknown
	}
	local, ok := s.localMembership(ctx)
	if !ok || local.EngineID == "" {
		return SelfHereUnknown
	}
	if sameEngine(local, info) {
		return SelfHereYes
	}
	return SelfHereNo
}

// sameEngine is true when the standalone environment's engine is the server's own.
func sameEngine(local envs.SwarmMembership, info EnvInfo) bool {
	return local.EngineID != "" && local.EngineID == info.EngineID
}
