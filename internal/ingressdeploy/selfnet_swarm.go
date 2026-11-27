// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// SelfNetworkPrefix is the prefix length of the self network's subnet: a /24. It holds one address per proxy task and
// TierPlan's own, so a /24 leaves room on any swarm TierPlan manages, and it comes from the same planner as the ingress
// network's /22.
const SelfNetworkPrefix = 24

// SelfNetworkSpec is the self network on the given subnet (D-02): a swarm-scoped overlay that is not attachable, so a plain
// `docker run --network tierplan-self` is refused and only service specs join it, carrying SelfNetworkLabels. It is created
// without the overlay's `encrypted` option: the proxy speaks TLS to TierPlan over it already (RESEARCH A2), and IPsec on top
// would only cost every request.
func SelfNetworkSpec(subnet string) dockerx.NetworkCreateSpec {
	return dockerx.NetworkCreateSpec{
		Name:       ingress.SelfNetworkName,
		Driver:     "overlay",
		Scope:      dockerx.ScopeSwarm,
		Attachable: false,
		Labels:     ingress.SelfNetworkLabels(),
		Subnet:     subnet,
	}
}

// selfNet is what the self network step found or made on one environment.
type selfNet struct {
	// attach is whether the proxy is to be on the network: it is TierPlan's and wanted.
	attach bool
	// id is the network's ID when TierPlan's own exists, "" otherwise.
	id string
	// foreign is a network of the reserved name that is not TierPlan's: it is never attached, adopted or removed.
	foreign bool
}

// selfPass is what one ensure pass knows of the self network before it reads the swarm.
type selfPass struct {
	info EnvInfo
	// place is where TierPlan runs, from SelfLocation.
	place SelfPlace
	// wanted is whether the self network is wanted; known is false when that could not be read, and then nothing is made or
	// removed and the proxy keeps the attachment it has.
	wanted, known bool
	// net is what the step under the lock found, when it ran.
	net selfNet
}

// selfPassOf reads where TierPlan runs and whether the environment wants the self network.
func (s *Service) selfPassOf(ctx context.Context, info EnvInfo) *selfPass {
	place, _, err := s.SelfLocation(ctx, info)
	if err != nil {
		place = s.selfUnknown()
	}
	sp := &selfPass{info: info, place: place}
	wanted, err := s.selfWanted(ctx, info, place)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not tell whether TierPlan's own network is wanted", "environment", info.ID, "error", err)
		return sp
	}
	sp.wanted, sp.known = wanted, true
	return sp
}

// selfWanted is whether the environment needs the self network: TierPlan's own route (the system claim) is held in it, or
// TierPlan's own certificate renews over HTTP-01 and TierPlan runs here, so the proxy carries the challenge to it (the
// carve-out, SELF-04).
func (s *Service) selfWanted(ctx context.Context, info EnvInfo, place SelfPlace) (bool, error) {
	claim, ok, err := s.d.Store.SystemClaim(ctx)
	if err != nil {
		return false, err
	}
	if ok && claim.EnvID == info.ID {
		return true, nil
	}
	if s.d.OwnHTTP01InUse == nil || place.Here != SelfHereYes {
		return false, nil
	}
	return s.d.OwnHTTP01InUse(ctx)
}

// attachWanted is whether the proxy is to be on the self network as the swarm was read in l: the network is TierPlan's and
// wanted. When wanted could not be read, the proxy keeps what it has.
func (sp *selfPass) attachWanted(l live) bool {
	row, ok := ownSelfNetwork(l.networks)
	if !ok {
		return false
	}
	if !sp.known {
		return l.service != nil && specNamesNetwork(l.service.Spec, row)
	}
	return sp.wanted
}

// selfWork is whether the self network has a write waiting: it is wanted and missing.
func (sp *selfPass) selfWork(l live) bool {
	if !sp.known || !sp.wanted {
		return false
	}
	_, ok := selfNetworkRowOf(l.networks)
	return !ok
}

// selfNetworkRowOf is the network of the reserved name among rows, TierPlan's or not.
func selfNetworkRowOf(rows []dockerx.NetworkRow) (dockerx.NetworkRow, bool) {
	for _, row := range rows {
		if row.Name == ingress.SelfNetworkName {
			return row, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// wantNow is the wanted value the step under the lock is given: what selfWanted said, or, when that could not be read,
// whether the proxy is on the network already, so nothing is made and nothing taken away.
func (sp *selfPass) wantNow(l live) bool {
	if sp.known {
		return sp.wanted
	}
	return sp.attachWanted(l)
}

// ensureSelfNetwork is the self network step of a swarm pass (D-02), before the proxy spec is built. TierPlan's own network
// (the name and the label pair, OwnsSelfNetwork) is adopted whatever its subnet; one of the name that is not TierPlan's is
// foreign and is never attached. When wanted and absent, a /24 is made from the swarm's pools under the address-space lock,
// by the planner the ingress network's /22 comes from. c is a manager's client, inside one write.
func (s *Service) ensureSelfNetwork(ctx context.Context, c *dockerx.Client, info EnvInfo, wanted bool) (selfNet, error) {
	rows, err := c.Networks(ctx)
	if err != nil {
		return selfNet{}, err
	}
	for _, row := range rows {
		if row.Name != ingress.SelfNetworkName {
			continue
		}
		if !ingress.OwnsSelfNetwork(row.Name, row.Labels) {
			return selfNet{foreign: true}, nil
		}
		return selfNet{attach: wanted, id: row.ID}, nil
	}
	if !wanted {
		return selfNet{}, nil
	}
	if s.d.AddressSpaces != nil {
		defer s.d.AddressSpaces.LockAllocation(info.ID)()
	}
	subnet, err := s.chooseSubnetSized(ctx, info.ID, true, SelfNetworkPrefix)
	if err != nil {
		return selfNet{}, fmt.Errorf("choose the self network's subnet: %w", err)
	}
	id, _, err := c.NetworkCreateAllocated(ctx, SelfNetworkSpec(subnet), s.d.AllocationWait)
	if err != nil {
		return selfNet{}, fmt.Errorf("create the self network: %w", err)
	}
	return selfNet{attach: true, id: id}, nil
}

// ownSelfNetwork is TierPlan's own self network among rows.
func ownSelfNetwork(rows []dockerx.NetworkRow) (dockerx.NetworkRow, bool) {
	for _, row := range rows {
		if ingress.OwnsSelfNetwork(row.Name, row.Labels) {
			return row, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// specNamesNetwork is whether a stored service spec attaches to the network, which swarm stores by ID and a spec may name.
func specNamesNetwork(stored json.RawMessage, row dockerx.NetworkRow) bool {
	var spec swarm.ServiceSpec
	if json.Unmarshal(stored, &spec) != nil {
		return false
	}
	for _, t := range spec.TaskTemplate.Networks {
		if (row.ID != "" && t.Target == row.ID) || t.Target == row.Name {
			return true
		}
	}
	return false
}
