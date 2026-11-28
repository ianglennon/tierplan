// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"fmt"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// SelfBackendAlias is the name TierPlan's own container answers to on a standalone host's self network (D-04): the ensure
// loop connects the container under it, and the proxy reaches TierPlan by it. It is the observation's BackendHost there.
const SelfBackendAlias = "tierplan"

// SelfBridgeSpec is the self network on a standalone host (D-02): a user-defined bridge carrying SelfNetworkLabels. An
// empty subnet leaves the choice to the engine.
func SelfBridgeSpec(subnet string) dockerx.NetworkCreateSpec {
	return dockerx.NetworkCreateSpec{
		Name:   ingress.SelfNetworkName,
		Driver: dockerx.DriverBridge,
		Labels: ingress.SelfNetworkLabels(),
		Subnet: subnet,
	}
}

// standaloneAttach is whether the proxy is to be on the self network as the host was read in l: the network is not foreign
// and is wanted. When wanted could not be read, the proxy keeps the attachment it has.
func (sp *selfPass) standaloneAttach(l standaloneLive) bool {
	if l.selfForeign {
		return false
	}
	if !sp.known {
		return l.self != nil && l.proxyOnSelf
	}
	return sp.wanted
}

// selfConnectable is whether TierPlan can connect its own container on this host: it runs here as a standalone container
// and the server reaches its local engine (Deps.SelfConnect).
func (s *Service) selfConnectable(sp *selfPass) bool {
	return s.d.SelfConnect != nil && sp.place.Here == SelfHereYes && sp.place.Kind == SelfKindStandalone && sp.place.ContainerID != ""
}

// selfDisconnectable is whether TierPlan can take its own container off the self network on this host.
func (s *Service) selfDisconnectable(sp *selfPass) bool {
	return s.d.SelfDisconnect != nil && sp.place.Here == SelfHereYes && sp.place.Kind == SelfKindStandalone
}

// standaloneSelfWork is whether the self network of a standalone host has a write waiting that the proxy's plan does not
// already bring: it is wanted and missing, or wanted and TierPlan's container (a recreated one, after an upgrade) is not on
// it while TierPlan can connect itself; or TierPlan's own bridge is there and not wanted, and TierPlan is not stuck on it
// (on it with no way to disconnect itself), so it is to be taken down.
func (s *Service) standaloneSelfWork(sp *selfPass, l standaloneLive) bool {
	if !sp.known || l.selfForeign {
		return false
	}
	if sp.wanted {
		return l.self == nil || (s.selfConnectable(sp) && !sp.place.Attached)
	}
	return l.self != nil && (!sp.place.Attached || s.selfDisconnectable(sp))
}

// ensureStandaloneSelfNetwork is the self network step of a standalone pass (D-04), under the operation lock and before the
// proxy's project is built. TierPlan's own bridge (the name and the label pair) is adopted whatever its subnet; one of the
// name that is not TierPlan's is foreign and never attached. When wanted and absent, the bridge is made, its /24 from the
// host's pools under the address-space lock when TierPlan plans them, else the engine's choice. Then, when wanted, TierPlan's
// own container is connected to it under SelfBackendAlias unless it is on it already, and each connect is audited as
// ingress.self_network_attached (D-18). A connect that fails is logged and tried again on the next pass: it changes nothing.
// When not wanted, TierPlan's container is taken off it through SelfDisconnect; removeStandaloneSelf removes the bridge once
// the proxy is off it too. c is the host's client, inside one write.
func (s *Service) ensureStandaloneSelfNetwork(ctx context.Context, c *dockerx.Client, sp *selfPass, l standaloneLive) (selfNet, error) {
	if l.selfForeign {
		return selfNet{foreign: true}, nil
	}
	attach := sp.standaloneAttach(l)
	net := selfNet{attach: attach}
	if l.self != nil {
		net.id = l.self.ID
	}
	if !sp.known {
		return net, nil
	}
	if net.id == "" && sp.wanted {
		id, err := s.createSelfBridge(ctx, c, sp.info)
		if err != nil {
			return selfNet{}, err
		}
		net.id = id
	}
	switch {
	case sp.wanted && net.id != "":
		s.connectSelf(ctx, sp, net.id)
	case !sp.wanted && net.id != "" && sp.place.Attached:
		net.keptInUse = !s.disconnectSelf(ctx, sp, net.id)
	}
	return net, nil
}

// disconnectSelf takes TierPlan's own container off the self network, which is no longer wanted (D-02), and says whether it
// is off. A host on which TierPlan cannot disconnect itself, or an engine that refuses, keeps the network in use; the next
// pass tries again.
func (s *Service) disconnectSelf(ctx context.Context, sp *selfPass, networkID string) bool {
	if !s.selfDisconnectable(sp) {
		return false
	}
	if err := s.d.SelfDisconnect(ctx, networkID); err != nil {
		s.log().Warn("the ingress ensure loop could not take TierPlan's own container off "+ingress.SelfNetworkName+"; the next pass tries again",
			"environment", sp.info.ID, "error", err)
		return false
	}
	sp.place.Attached = false
	s.log().Info("the ingress ensure loop took TierPlan's own container off "+ingress.SelfNetworkName+": it is no longer needed", "environment", sp.info.ID)
	return true
}

// removeStandaloneSelf removes TierPlan's own bridge from a standalone host once it is not wanted, TierPlan's container is
// off it and the proxy is too (proxyOff: the proxy as read is not on it, or this pass brought it up without it), so nothing
// of TierPlan's is left behind (D-02). An engine that answers the network is still in use keeps it (KeptInUse) and is never
// a failed pass; the next pass tries again. The caller holds the operation lock.
func (s *Service) removeStandaloneSelf(ctx context.Context, sp *selfPass, proxyOff bool) error {
	if !sp.known || sp.wanted || sp.net.id == "" || sp.net.foreign || sp.net.keptInUse {
		return nil
	}
	if sp.place.Attached || !proxyOff {
		sp.net.keptInUse = sp.place.Attached
		return nil
	}
	info := sp.info
	err := s.d.Engine.WriteOnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) error {
		return c.NetworkRemove(ctx, sp.net.id)
	})
	switch {
	case err == nil, dockerx.IsNotFound(err):
		sp.net.id, sp.net.removed = "", true
		return nil
	case dockerx.IsNetworkInUse(err):
		sp.net.keptInUse = true
		return nil
	}
	return fmt.Errorf("remove the self network: %w", err)
}

// createSelfBridge makes the self bridge on a standalone host and returns its ID.
func (s *Service) createSelfBridge(ctx context.Context, c *dockerx.Client, info EnvInfo) (string, error) {
	subnet := ""
	if s.d.AddressSpaces != nil {
		defer s.d.AddressSpaces.LockAllocation(info.ID)()
		var err error
		if subnet, err = s.chooseSubnetSized(ctx, info.ID, true, SelfNetworkPrefix); err != nil {
			return "", fmt.Errorf("choose the self network's subnet: %w", err)
		}
	}
	id, _, err := c.NetworkCreate(ctx, SelfBridgeSpec(subnet))
	if err != nil {
		return "", fmt.Errorf("create the self network: %w", err)
	}
	return id, nil
}

// connectSelf connects TierPlan's own container to the self network when it is not on it and TierPlan can. The place is
// updated, so the observation of the pass says TierPlan is attached.
func (s *Service) connectSelf(ctx context.Context, sp *selfPass, networkID string) {
	if sp.place.Attached || !s.selfConnectable(sp) {
		return
	}
	if err := s.d.SelfConnect(ctx, networkID, SelfBackendAlias); err != nil {
		s.log().Warn("the ingress ensure loop could not connect TierPlan's own container to "+ingress.SelfNetworkName+"; the next pass tries again",
			"environment", sp.info.ID, "error", err)
		return
	}
	sp.place.Attached = true
	s.log().Info("the ingress ensure loop connected TierPlan's own container to "+ingress.SelfNetworkName, "environment", sp.info.ID)
	if s.d.Audit == nil {
		return
	}
	if err := s.d.Audit.Record(ctx, audit.Entry{
		Actor: audit.SystemIngress, Action: audit.ActionIngressSelfNetworkAttached, TargetType: "environment", TargetID: sp.info.ID,
		EnvID: sp.info.ID, Result: audit.ResultSuccess,
		Details: map[string]string{"network": ingress.SelfNetworkName, "container": shortContainerID(sp.place.ContainerID)},
	}); err != nil {
		s.log().Warn("the ingress ensure loop could not audit TierPlan's connect to its own network", "environment", sp.info.ID, "error", err)
	}
}

// shortContainerID is the 12 characters of a container ID the docker CLI shows.
func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// observeStandaloneSelf reads the self network, the ingress network and the host's bridge on the standalone host's engine
// and publishes what it found, as observeSelf does on a swarm. A host on which the self network is wanted, while TierPlan
// cannot connect its own container (no local engine), is logged when that is first seen.
func (s *Service) observeStandaloneSelf(ctx context.Context, sp *selfPass) {
	info := sp.info
	prev, had := s.SelfObservationOf(info.ID)
	o, ok := s.observeSelfOn(ctx, sp, func(fn func(*dockerx.Client) error) error {
		return s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, fn)
	})
	if !ok {
		return
	}
	stuck := o.Wanted && o.Exists && !o.Attached && s.d.SelfConnect == nil
	if stuck && (!had || !prev.Wanted || !prev.Exists || prev.Attached) {
		s.log().Warn("TierPlan cannot connect its own container to "+ingress.SelfNetworkName+": it does not reach the host's engine through its "+
			"local socket. Connect it by hand with the alias "+SelfBackendAlias+", or run TierPlan with the Docker socket mounted",
			"environment", info.ID)
	}
}
