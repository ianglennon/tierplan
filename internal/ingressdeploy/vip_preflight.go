// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// The codes of the refusals only the virtual IP enable raises. The address, router ID and manager codes are those of
// vip_checks.go and vip_gate.go, and the paused code is the ingress one (CONTEXT D-16, D-19; UI-SPEC S5).
const (
	// CodeVIPSwarmOnly is the refusal of a virtual IP on a standalone environment: its own address already reaches its proxy.
	CodeVIPSwarmOnly = "vip_swarm_only"
	// CodeVIPIngressOff is the refusal of a virtual IP while the environment's ingress is off: the address points at the proxies.
	CodeVIPIngressOff = "vip_ingress_off"
	// CodeVIPAlreadyEnabled is the refusal of an enable on an environment that has a virtual IP row already. A change or a
	// switch-off is the way on from there.
	CodeVIPAlreadyEnabled = "vip_already_enabled"
	// CodeVIPNameTaken is the refusal of a swarm that holds a service named tierplan-keepalived, or a secret under the key
	// secrets' prefix, that is not TierPlan's. It is never taken over (A-08).
	CodeVIPNameTaken = "vip_name_taken"
)

// The sentences of those codes. The first two are UI-SPEC S5's; none is built from a value.
const (
	vipSwarmOnlyText      = "A virtual IP is for swarms. A single host's own address already reaches its proxy."
	vipIngressOffText     = "Switch ingress on for this environment first. The virtual IP points at the proxies."
	vipNameTakenText      = "The swarm already has an object under a name the virtual IP reserves, and it isn't TierPlan's. Remove or rename it, then try again."
	vipAlreadyEnabledText = "This environment already has a virtual IP. Change it or switch it off instead of enabling it again."
	// vipManagersUnreadText is added to the gate's sentence when the managers' opt-in could not be read at all.
	vipManagersUnreadText = " The managers could not be read just now, so none can be counted as allowing."
)

// VIPRequest is what a person asks for when they enable the virtual IP.
type VIPRequest struct {
	// Address is the virtual IP with its prefix length, for example 192.168.1.50/24.
	Address string
	// RouterID is the VRRP router ID the Admin chose; nil lets TierPlan derive one from the swarm's cluster ID (D-05).
	RouterID *int
}

// VIPPreview is what PreviewVIP tells a person before they enable (D-16). It holds no key: none exists until the job
// makes it.
type VIPPreview struct {
	// Findings are the refusals and the warnings, in the order the checks ran.
	Findings []Finding
	// Address is the address as parsed, with its prefix length; the zero Prefix when it was refused as typed.
	Address netip.Prefix
	// RouterID is the router ID the enable would use and RouterDerived says it comes from the cluster ID; DerivedRouterID is
	// the value the cluster ID gives, so a form can show it beside an override.
	RouterID, DerivedRouterID int
	RouterDerived             bool
	// Managers is the privileged-services opt-in of every manager (D-19); empty when it could not be read.
	Managers []envs.ManagerPolicyRow
	// Warning is the fixed text of D-20, always present.
	Warning string
	// Items are the steps the enable job would run, as their labels.
	Items []string
}

// vipView is what the checks read of the swarm, through one manager.
type vipView struct {
	nodes   []dockerx.NodeRow
	nets    []dockerx.NetworkRow
	secrets []dockerx.SecretRow
	// service is the keepalived service by its reserved name, nil when the swarm has none.
	service *dockerx.ServiceDetail
}

// vipPreflight runs every check of the virtual IP enable and writes nothing. The preview shows its findings and the enable
// re-runs it under the environment's operation lock, so a refusal is the same on both. It stops at the first of the four
// refusals that make the rest meaningless (a standalone environment, ingress off or paused, a virtual IP already there, no
// manager to read) and otherwise gathers every finding, so a person fixes them in one pass. An error is a read that failed,
// not a refusal.
func (s *Service) vipPreflight(ctx context.Context, info EnvInfo, req VIPRequest) (VIPPreview, []Finding, error) {
	return s.vipChecks(ctx, info, req, false)
}

// vipChecks is vipPreflight for an enable (change false) and for a change of the virtual IP the environment has (change true).
// A change runs every check of the enable on the new request, with two differences: the environment must have a virtual IP
// already (ErrVIPNotEnabled when it has none) and having one is no refusal, and the preview lists the steps of the change.
func (s *Service) vipChecks(ctx context.Context, info EnvInfo, req VIPRequest, change bool) (VIPPreview, []Finding, error) {
	pv := VIPPreview{Warning: VIPPrivilegeWarning, Items: vipItemLabelList(), DerivedRouterID: DerivedRouterID(info.ClusterID)}
	refuse := func(code, msg string) (VIPPreview, []Finding, error) {
		return pv, []Finding{{Code: code, Message: msg, Severity: SeverityError}}, nil
	}
	if !info.Swarm {
		return refuse(CodeVIPSwarmOnly, vipSwarmOnlyText)
	}
	state, err := s.d.Store.DeployState(ctx, info.ID)
	if err != nil {
		return pv, nil, err
	}
	if !state.Enabled {
		return refuse(CodeVIPIngressOff, vipIngressOffText)
	}
	if paused, err := checkPaused(ctx, s, checkInput{Info: info}); err != nil || len(paused) > 0 {
		return pv, paused, err
	}
	vips, err := s.d.Store.ListVIPs(ctx)
	if err != nil {
		return pv, nil, err
	}
	has := slices.ContainsFunc(vips, func(v ingressstore.VIPConfig) bool { return v.EnvID == info.ID })
	switch {
	case change && !has:
		return pv, nil, ErrVIPNotEnabled
	case !change && has:
		return refuse(CodeVIPAlreadyEnabled, vipAlreadyEnabledText)
	}
	if !info.ManagerConnected {
		f, err := checkManager(ctx, s, checkInput{Info: info})
		return pv, f, err
	}
	view, err := s.readVIPView(ctx, info.ID)
	if err != nil {
		return pv, nil, err
	}
	others := s.vipOthers(ctx, info.ID, vips)

	var findings []Finding
	prefix, f := ParseVIPAddress(req.Address)
	if f != nil {
		findings = append(findings, *f)
	} else {
		pv.Address = prefix
		allocated, pools, err := s.vipAllocation(ctx, info.ID, view)
		if err != nil {
			return pv, nil, err
		}
		findings = append(findings, CheckVIPAddress(VIPAddressInput{
			Prefix: prefix, Nodes: vipNodes(view.nodes), Others: others, Allocated: allocated, Pools: pools,
		})...)
	}

	id, derived, f := ResolveRouterID(req.RouterID, info.ClusterID)
	if f != nil {
		findings = append(findings, *f)
	} else {
		pv.RouterID, pv.RouterDerived = id, derived
		if cf := CheckRouterIDCollision(info.ID, id, prefix, others); cf != nil {
			findings = append(findings, *cf)
		}
	}

	findings = append(findings, checkVIPNames(view)...)

	rows, err := s.vipManagerRows(ctx, info.ID)
	pv.Managers = rows
	if ok, gf := EvaluateManagerGate(rows); !ok {
		if err != nil {
			gf.Message += vipManagersUnreadText
		}
		findings = append(findings, *gf)
	}
	return pv, findings, nil
}

// vipManagerRows reads the managers' opt-in. A dependency that is not wired, or a read that fails, gives no rows and an
// error, which the gate treats as every manager not allowing; the read error is logged and never shown, because it can carry
// an engine's words.
func (s *Service) vipManagerRows(ctx context.Context, envID string) ([]envs.ManagerPolicyRow, error) {
	if s.d.Managers == nil {
		return nil, errVIPManagersUnread
	}
	rows, err := s.d.Managers.ManagerPolicies(ctx, envID)
	if err != nil {
		s.log().Warn("could not read the managers' privileged-services setting for a virtual IP check", "environment", envID, "error", err)
		return nil, err
	}
	return rows, nil
}

// readVIPView reads the nodes, the networks, the secrets and the keepalived service of the swarm through one manager.
func (s *Service) readVIPView(ctx context.Context, envID string) (vipView, error) {
	var v vipView
	err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		if v.nodes, err = c.SwarmNodes(ctx); err != nil {
			return err
		}
		if v.nets, err = c.Networks(ctx); err != nil {
			return err
		}
		if v.secrets, err = c.SecretList(ctx); err != nil {
			return err
		}
		svc, _, err := c.ServiceInspect(ctx, ingress.ServiceNameKeepalived)
		switch {
		case err == nil:
			v.service = &svc
		case errors.Is(err, dockerx.ErrServiceNotFound):
		default:
			return err
		}
		return nil
	})
	return v, err
}

// vipNodes is the node list as the address checks read it. An address the swarm gives that does not parse is left out of
// the comparison, as one it did not give is.
func vipNodes(rows []dockerx.NodeRow) []VIPNode {
	out := make([]VIPNode, 0, len(rows))
	for _, n := range rows {
		node := VIPNode{ID: n.ID, Hostname: n.Hostname}
		if a, err := netip.ParseAddr(n.Address); err == nil {
			node.Address = a
		}
		out = append(out, node)
	}
	return out
}

// vipOthers are the other environments' virtual IPs with the names of their environments. An environment whose name cannot
// be read is quoted by its ID.
func (s *Service) vipOthers(ctx context.Context, self string, vips []ingressstore.VIPConfig) []VIPOther {
	var out []VIPOther
	for _, v := range vips {
		if v.EnvID == self {
			continue
		}
		name := v.EnvID
		if e, err := s.d.Envs.Info(ctx, v.EnvID); err == nil && e.Name != "" {
			name = e.Name
		}
		out = append(out, VIPOther{EnvID: v.EnvID, EnvName: name, Prefix: v.Prefix(), RouterID: v.RouterID})
	}
	return out
}

// vipAllocation is what the address is compared with besides the nodes: the subnets of the networks that exist, which refuse
// an overlap, and the swarm's address pools and the nodes' host routes, which only warn (A-11). A network is quoted by its
// name when the manager lists it, and otherwise by its subnet. Without address spaces nothing is compared.
func (s *Service) vipAllocation(ctx context.Context, envID string, view vipView) ([]VIPNetwork, []netip.Prefix, error) {
	if s.d.AddressSpaces == nil {
		return nil, nil, nil
	}
	in, err := s.d.AddressSpaces.AllocationInputs(ctx, envID, true)
	if err != nil {
		return nil, nil, err
	}
	names := map[netip.Prefix]string{}
	for _, n := range view.nets {
		for _, sub := range n.Subnets {
			if p, err := netip.ParsePrefix(sub); err == nil {
				if _, ok := names[p.Masked()]; !ok {
					names[p.Masked()] = n.Name
				}
			}
		}
	}
	allocated := make([]VIPNetwork, 0, len(in.Occupied))
	for _, p := range in.Occupied {
		name, ok := names[p.Masked()]
		if !ok {
			name = p.String()
		}
		allocated = append(allocated, VIPNetwork{Name: name, Subnet: p})
	}
	pools := make([]netip.Prefix, 0, len(in.Pools)+len(in.Avoid))
	for _, p := range in.Pools {
		pools = append(pools, p.Base)
	}
	return allocated, append(pools, in.Avoid...), nil
}

// checkVIPNames refuses a swarm that holds a service named tierplan-keepalived, or a secret under the key secrets' prefix,
// without the keepalived label: the object is not TierPlan's and is never taken over (A-08). A labelled one is TierPlan's own,
// the leftover of an enable that failed, and the job reuses it. The names are cut for the finding, which is never built from
// anything else the engine said.
func checkVIPNames(view vipView) []Finding {
	var items []string
	if view.service != nil && !ingress.OwnsKeepalivedService(view.service.Name, view.service.Labels) {
		items = append(items, "service "+vipName(view.service.Name))
	}
	for _, row := range view.secrets {
		if strings.HasPrefix(row.Name, ingress.SecretPrefixKeepalived) && !ingress.OwnsKeepalivedSecret(row.Name, row.Labels) {
			items = append(items, "secret "+vipName(row.Name))
		}
	}
	if len(items) == 0 {
		return nil
	}
	slices.Sort(items)
	return []Finding{{Code: CodeVIPNameTaken, Message: vipNameTakenText, Items: slices.Compact(items), Severity: SeverityError}}
}
