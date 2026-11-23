// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/netspace"
)

// CodeHostUnreachable is the refusal of a standalone environment whose host is not connected: nothing can be created
// or removed on it (D-12, D-21).
const CodeHostUnreachable = "ingress_host_unreachable"

// hostConnected is true when the standalone environment's one node is connected, so its engine can be read.
func hostConnected(info EnvInfo) bool {
	return len(info.Nodes) > 0 && info.Nodes[0].Connected
}

// checkHostReachable refuses a standalone environment whose one node is not connected.
func checkHostReachable(_ context.Context, _ *Service, in checkInput) ([]Finding, error) {
	if hostConnected(in.Info) {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeHostUnreachable,
		Message:  "The host is not connected to TierPlan, so the ingress proxy cannot be created or removed on it. Reconnect it and try again.",
		Severity: SeverityError,
	}}, nil
}

// load reads the host through the node its one engine answers on, once; every later call answers from the first read.
// The caller has checked that the host is connected.
func (v *hostView) load(ctx context.Context, s *Service, info EnvInfo) error {
	if v.read {
		return v.err
	}
	v.read = true
	v.err = s.d.Engine.OnNode(ctx, info.ID, info.Nodes[0].AgentID, func(c *dockerx.Client) (err error) {
		if v.published, err = c.ContainersPublishing(ctx, proxyPorts...); err != nil {
			return err
		}
		if v.networks, err = c.Networks(ctx); err != nil {
			return err
		}
		if v.project, err = c.ComposeContainers(ctx, ingress.ProjectName); err != nil {
			return err
		}
		vol, _, err := c.VolumeDetail(ctx, ingress.VolumeName)
		switch {
		case err == nil:
			v.volume = &vol
		case dockerx.IsNotFound(err):
			return nil
		}
		return err
	})
	if v.err != nil {
		v.err = fmt.Errorf("read the host: %w", v.err)
	}
	return v.err
}

// checkStandalonePorts refuses a host where any container, running, created or exited, binds host TCP 80 or 443 (D-12,
// D-31). A stopped container with a restart policy takes its port back when the engine starts, so a check of the running
// ones alone would let the proxy lose its port after a reboot. The proxy's own container never counts, so an enable that
// is run again after a failed one does not refuse itself. A UDP binding does not clash. A host that is not connected is
// refused by checkHostReachable.
func checkStandalonePorts(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !hostConnected(in.Info) {
		return nil, nil
	}
	if err := in.host.load(ctx, s, in.Info); err != nil {
		return nil, err
	}
	var items []string
	for _, p := range in.host.published {
		if ingress.OwnsProxyContainer(p.Labels) {
			continue
		}
		items = append(items, fmt.Sprintf("%s publishes %d (%s)", p.Name, p.Port, p.State))
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodePortInUse,
		Message:  "Ports 80 and 443 must be free on this host. These containers publish them, stopped ones included, because a container with a restart policy takes its port back when the host restarts:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}

// checkStandaloneNames refuses a host that holds a container of the proxy's compose project, a network named like the
// proxy's bridge or a volume named like its cache that is not TierPlan's: the object is never taken over (D-12,
// Pitfall 11). A labelled one is TierPlan's own and is reused.
func checkStandaloneNames(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !hostConnected(in.Info) {
		return nil, nil
	}
	if err := in.host.load(ctx, s, in.Info); err != nil {
		return nil, err
	}
	var items []string
	for _, c := range in.host.project {
		if !ingress.OwnsProxyContainer(c.Labels) {
			items = append(items, "container "+c.Name)
		}
	}
	for _, n := range in.host.networks {
		if n.Name == ingress.NetworkName && !ingress.OwnsNetwork(n.Name, n.Labels) {
			items = append(items, "network "+n.Name)
		}
	}
	if v := in.host.volume; v != nil && !ingress.OwnsVolume(v.Name, v.Labels) {
		items = append(items, "volume "+v.Name)
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodeNameTaken,
		Message:  "These objects have names TierPlan keeps for its ingress and are not TierPlan's. Remove or rename them, then try again:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}

// CodeNameShadowed is a standalone enable, or an ensure pass that made the ingress network again, that found a container
// stored on a network name that is the start of the new ingress network's ID (T-25-47). The engine looks such a name up
// again when the container first starts, so once no network carries it a start by the engine itself (a restart policy at
// boot) would put the container on the shared bridge under names of its own, where it could take another stack's route.
// The ID is only known once the network is made, so this is checked then, before ingress counts as on.
const CodeNameShadowed = "ingress_name_shadowed"

// shadowDetail lists refs for the person, sorted.
func shadowDetail(refs []dockerx.ShadowRef) string {
	items := make([]string, 0, len(refs))
	for _, r := range refs {
		items = append(items, r.Container+" on network name "+r.Network)
	}
	slices.Sort(items)
	return strings.Join(slices.Compact(items), "; ")
}

// checkStandaloneSubnet refuses a bridge network that could not be made: a typed subnet that is not an IPv4 block or that
// overlaps a network of the host, or no free block of StandaloneNetworkPrefix in the host's address pools (D-12). A network
// of TierPlan's that exists already is reused, so nothing is checked.
func checkStandaloneSubnet(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !hostConnected(in.Info) {
		return nil, nil
	}
	if err := in.host.load(ctx, s, in.Info); err != nil {
		return nil, err
	}
	for _, n := range in.host.networks {
		if ingress.OwnsNetwork(n.Name, n.Labels) {
			return nil, nil
		}
	}
	if in.Req.Subnet != "" {
		return checkTypedSubnet(ctx, s, in, in.host.networks, "10.20.0.0/24", "a network on the host")
	}
	if s.d.AddressSpaces == nil {
		return nil, nil
	}
	inputs, err := s.d.AddressSpaces.AllocationInputs(ctx, in.Info.ID, false)
	if err != nil {
		return nil, fmt.Errorf("read the host's address pools: %w", err)
	}
	if _, _, err := netspace.LowestFree(inputs.Pools, inputs.Occupied, inputs.Avoid, StandaloneNetworkPrefix); err != nil {
		if !errors.Is(err, netspace.ErrNoFreeBlock) {
			return nil, err
		}
		return []Finding{{
			Code:     CodeNoAddressSpace,
			Message:  fmt.Sprintf("No free /%d block is left in the host's address pools for the ingress network. Free one, or type a subnet.", StandaloneNetworkPrefix),
			Severity: SeverityError,
		}}, nil
	}
	return nil, nil
}

// checkStandaloneHTTP01 refuses when TierPlan's own certificate uses the HTTP-01 challenge and TierPlan runs on this
// host: the proxy takes port 80, so the renewal would break silently (D-04). TierPlan is on this host when the
// environment is the local socket, or an agent host whose engine ID is the local engine's. What cannot be told is a
// warning, never a silent pass: a challenge that cannot be read, a local engine that does not answer or has no ID, or a
// host whose engine ID is not known.
func checkStandaloneHTTP01(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if s.d.OwnChallenge == nil {
		return nil, nil
	}
	unknown := func() []Finding {
		return []Finding{{
			Code:     CodeHTTP01Unknown,
			Message:  "TierPlan could not tell whether its own certificate needs port 80, which the ingress takes. If it uses the HTTP-01 challenge and TierPlan runs on this host, switch it to DNS-01 first.",
			Severity: SeverityWarning,
		}}
	}
	challenge, err := s.d.OwnChallenge(ctx)
	if err != nil {
		s.log().Warn("could not read TierPlan's own challenge type for the ingress preflight", "error", err)
		return unknown(), nil
	}
	if challenge != challengeHTTP01 {
		return nil, nil
	}
	conflict := []Finding{{
		Code:     CodeHTTP01Conflict,
		Message:  "TierPlan runs on this host and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes. Switch TierPlan's own certificate to DNS-01 first. A later version lifts this limit.",
		Severity: SeverityError,
	}}
	if in.Info.Local {
		return conflict, nil
	}
	if s.d.LocalSwarm == nil || in.Info.EngineID == "" {
		return unknown(), nil
	}
	local, err := s.d.LocalSwarm(ctx)
	if err != nil {
		s.log().Warn("could not read TierPlan's own engine for the ingress preflight", "error", err)
		return unknown(), nil
	}
	switch {
	case !local.Known || local.EngineID == "":
		return unknown(), nil
	case local.EngineID == in.Info.EngineID:
		return conflict, nil
	}
	return nil, nil
}

// standaloneChecks are the checks of a standalone host, in the order their findings are listed.
var standaloneChecks = []check{
	checkPaused, checkHostReachable, checkStandalonePorts, checkStandaloneNames, checkStandaloneSubnet, checkStandaloneHTTP01, checkURL, checkRedeploy,
}

// preflightStandalone runs every check of a standalone host and returns every finding, so a person sees all that is
// wrong at once. The stacks to redeploy are listed as the swarm's preflight does.
func (s *Service) preflightStandalone(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, []StackRef, error) {
	var out []Finding
	var redeploy []StackRef
	in := checkInput{Info: info, Req: req, cl: &clusterView{}, host: &hostView{}, redeploy: &redeploy}
	for _, c := range standaloneChecks {
		fs, err := c(ctx, s, in)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fs...)
	}
	return out, redeploy, nil
}
