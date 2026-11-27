// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/netspace"
)

// The codes of the preflight refusals this file owns. Every other check of the list adds its own beside
// its function.
const (
	// CodeNoManager is the refusal of an environment no manager agent is connected to (D-07).
	CodeNoManager = "ingress_no_manager"
	// CodePortInUse is the refusal of an environment where something already publishes TCP port 80 or 443 (D-07, D-43).
	CodePortInUse = "ingress_port_in_use"
	// CodeNodesNotChecked is the warning that lists the nodes whose ports could not be read. It never refuses
	// (PROH-24-06).
	CodeNodesNotChecked = "ingress_nodes_not_checked"
	// CodeNoAddressSpace is the refusal of a swarm whose default address pool has no free block for the ingress network.
	CodeNoAddressSpace = "ingress_no_address_space"
	// CodeSubnetInvalid is the refusal of a typed subnet that is not an IPv4 CIDR block.
	CodeSubnetInvalid = "ingress_subnet_invalid"
	// CodeSubnetOverlaps is the refusal of a typed subnet that overlaps an existing network.
	CodeSubnetOverlaps = "ingress_subnet_overlaps"
	// CodeHTTP01Conflict is the refusal of an environment that carries TierPlan itself, whose own certificate uses the
	// HTTP-01 challenge on port 80, where the carve-out cannot reach TierPlan (D-09, D-12).
	CodeHTTP01Conflict = "ingress_http01_conflict"
	// CodeHTTP01Unknown is the warning that TierPlan could not tell whether the conflict applies.
	CodeHTTP01Unknown = "ingress_http01_unknown"
	// CodeHTTP01CarveOut is the notice that TierPlan's own certificate uses HTTP-01 and keeps renewing through the ingress's
	// challenge carve-out over the self network (D-12).
	CodeHTTP01CarveOut = "ingress_http01_carve_out"
	// CodeNameTaken is the refusal of a swarm that holds an object under a name the ingress reserves that is not
	// TierPlan's (Pitfall 11).
	CodeNameTaken = "ingress_name_taken"
	// CodeURLUnset is the refusal of an install that does not know its own address, which the proxies dial.
	CodeURLUnset = "ingress_url_unset"
	// CodeURLRouted is the refusal of an address whose host the ingress routes, so the proxies would dial TierPlan
	// through themselves.
	CodeURLRouted = "ingress_url_routed"
	// CodeRedeployNeeded is the warning that lists the stacks to redeploy so they join the ingress network (D-14).
	CodeRedeployNeeded = "ingress_redeploy_needed"
	// CodePaused is the refusal of an environment whose ingress a restore paused (D-42).
	CodePaused = "ingress_paused"
)

// The host ports the swarm proxy takes on every node: HTTP, HTTPS and the readiness endpoint (D-13).
var swarmProxyPorts = []uint32{HTTPPublished, HTTPSPublished, ReadyPublished}

// The host ports the standalone proxy takes: HTTP and HTTPS only. The readiness endpoint is a swarm-only
// concern (A-06), so a standalone host may use that port.
var standaloneProxyPorts = []uint32{HTTPPublished, HTTPSPublished}

// portsSentence writes the ports as the preflight messages name them: "80 and 443", "80, 443 and 7483".
func portsSentence(ports []uint32) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.FormatUint(uint64(p), 10)
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// The severities of a Finding.
const (
	// SeverityError refuses the operation.
	SeverityError = "error"
	// SeverityWarning tells the person something and lets the operation go on.
	SeverityWarning = "warning"
	// SeverityNotice is a neutral note: nothing is wrong, the person is told how something will work.
	SeverityNotice = "notice"
)

// Finding is one thing the preflight found: a refusal, or a warning.
type Finding struct {
	// Code is the stable name of the finding.
	Code string
	// Message is the sentence the person reads.
	Message string
	// Items name what the finding is about (a port, a stack, a hostname), empty for none.
	Items    []string
	Severity string
}

// PreflightError is an enable the preflight refused: at least one finding of severity error. Nothing was
// written.
type PreflightError struct {
	Findings []Finding
}

// Error lists the codes of the refusals.
func (e *PreflightError) Error() string {
	var codes []string
	for _, f := range e.Findings {
		if f.Severity == SeverityError {
			codes = append(codes, f.Code)
		}
	}
	return "ingress was not enabled: " + strings.Join(codes, ", ")
}

// hasErrors reports whether any finding refuses.
func hasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// checkInput is what a preflight check reads.
type checkInput struct {
	Info EnvInfo
	Req  EnableRequest
	// cl is read once and shared by the checks that look at the swarm.
	cl *clusterView
	// redeploy collects the stacks to redeploy, for the preview.
	redeploy *[]StackRef
	// host is read once and shared by the checks that look at a standalone host, nil for a swarm.
	host *hostView
}

// clusterView is what one manager says of the swarm, read the first time a check asks for it.
type clusterView struct {
	read     bool
	err      error
	svcs     []dockerx.ServiceRow
	networks []dockerx.NetworkRow
	secrets  []dockerx.SecretRow
}

// load reads the swarm through the engine once; every later call answers from the first read.
func (v *clusterView) load(ctx context.Context, s *Service, envID string) error {
	if v.read {
		return v.err
	}
	v.read = true
	v.err = s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		cr, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		v.svcs = cr.Services
		if v.networks, err = c.Networks(ctx); err != nil {
			return err
		}
		v.secrets, err = c.SecretList(ctx)
		return err
	})
	if v.err != nil {
		v.err = fmt.Errorf("read the swarm: %w", v.err)
	}
	return v.err
}

// hostView is what the standalone host's one engine says of itself, read the first time a check asks for it.
type hostView struct {
	read bool
	err  error
	// published are the containers, of every state, that bind host TCP 80 or 443.
	published []dockerx.PublishedPort
	// networks are every network of the host.
	networks []dockerx.NetworkRow
	// project are the containers of the proxy's compose project, of every state.
	project []dockerx.ComposeContainer
	// volume is the volume the proxy's cache would use, nil when the host has none by that name.
	volume *dockerx.VolumeDetail
}

// check is one step of the preflight. It returns what it found, or an error when it could not look.
type check func(ctx context.Context, s *Service, in checkInput) ([]Finding, error)

// preflightChecks are the checks, in the order their findings are listed. A later plan adds to the end.
var preflightChecks = []check{
	checkManager, checkPaused, checkServicePorts, checkContainerPorts, checkNames, checkAddressPool, checkHTTP01, checkURL, checkRedeploy,
}

// preflight runs every check and returns every finding, so a person sees all that is wrong at once.
func (s *Service) preflight(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, error) {
	out, _, err := s.preflightRun(ctx, info, req)
	return out, err
}

// preflightRun is preflight and also returns the stacks the preview lists to redeploy, ordered by name.
func (s *Service) preflightRun(ctx context.Context, info EnvInfo, req EnableRequest) ([]Finding, []StackRef, error) {
	var out []Finding
	var redeploy []StackRef
	in := checkInput{Info: info, Req: req, cl: &clusterView{}, redeploy: &redeploy}
	for _, c := range preflightChecks {
		fs, err := c(ctx, s, in)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, fs...)
	}
	return out, redeploy, nil
}

// checkManager refuses an environment no manager agent is connected to: the cluster cannot be written to
// (D-07).
func checkManager(_ context.Context, _ *Service, in checkInput) ([]Finding, error) {
	if in.Info.ManagerConnected {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeNoManager,
		Message:  "No manager node of this swarm is connected, so TierPlan cannot create the ingress objects. Reconnect a manager node's agent and try again.",
		Severity: SeverityError,
	}}, nil
}

// checkServicePorts refuses a swarm where a service, of a stack or of none, publishes TCP port 80, 443 or 7483 in either
// mode: the proxy publishes all three on every node (D-07, D-13). The proxy's own service never counts, and a UDP port does not
// clash with it (D-43).
func checkServicePorts(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !in.Info.ManagerConnected {
		return nil, nil
	}
	if err := in.cl.load(ctx, s, in.Info.ID); err != nil {
		return nil, err
	}
	var items []string
	for _, row := range in.cl.svcs {
		if row.Ingress {
			continue
		}
		for _, p := range row.Ports {
			if p.Protocol == "tcp" && slices.Contains(swarmProxyPorts, p.Published) {
				items = append(items, fmt.Sprintf("%s publishes %d (%s mode)", row.Name, p.Published, p.Mode))
			}
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodePortInUse,
		Message:  "Ports " + portsSentence(swarmProxyPorts) + " must be free on every node. These publish them:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}

// checkPaused refuses while a restore has paused the environment's ingress, so a restored install cannot switch a
// swarm's ingress on by accident (D-42).
func checkPaused(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if s.d.Store == nil {
		return nil, nil
	}
	st, err := s.d.Store.DeployState(ctx, in.Info.ID)
	if err != nil {
		return nil, err
	}
	if !st.Paused {
		return nil, nil
	}
	return []Finding{{
		Code:     CodePaused,
		Message:  "Ingress on this environment was paused by a restore. Resume it first.",
		Severity: SeverityError,
	}}, nil
}

// tcpHostPorts are the TCP ports of the proxy that a container row publishes on its host, from the rows' "ip:port->
// port/proto" form.
func tcpHostPorts(row dockerx.ContainerRow) []uint32 {
	var out []uint32
	for _, p := range row.Ports {
		left, right, ok := strings.Cut(p, "->")
		if !ok || !strings.HasSuffix(right, "/tcp") {
			continue
		}
		i := strings.LastIndex(left, ":")
		if i < 0 {
			continue
		}
		n, err := strconv.ParseUint(left[i+1:], 10, 32)
		if err == nil && slices.Contains(swarmProxyPorts, uint32(n)) {
			out = append(out, uint32(n))
		}
	}
	return out
}

// checkContainerPorts refuses a node where a plain container publishes TCP 80, 443 or the readiness port on the host (D-07, D-43). The proxy's
// own tasks and the tasks of other swarm services never count here: the service check names those. A node whose agent
// cannot be reached is a warning and never a refusal (PROH-24-06).
func checkContainerPorts(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !in.Info.ManagerConnected {
		return nil, nil
	}
	var items, unchecked []string
	for _, n := range in.Info.Nodes {
		if !n.Connected || n.AgentID == "" {
			unchecked = append(unchecked, n.Name)
			continue
		}
		var rows []dockerx.ContainerRow
		err := s.d.Engine.OnNode(ctx, in.Info.ID, n.AgentID, func(c *dockerx.Client) (err error) {
			rows, err = c.RunningContainers(ctx)
			return err
		})
		if err != nil {
			s.log().Warn("could not read the containers of a node for the ingress preflight", "environment", in.Info.ID, "node", n.Name, "error", err)
			unchecked = append(unchecked, n.Name)
			continue
		}
		for _, row := range rows {
			if ingress.HasComponentLabel(row.Labels) || row.Labels[swarmServiceLabel] != "" {
				continue
			}
			for _, port := range tcpHostPorts(row) {
				items = append(items, fmt.Sprintf("%s: %s publishes %d", n.Name, row.Name, port))
			}
		}
	}
	var out []Finding
	if len(items) > 0 {
		slices.Sort(items)
		out = append(out, Finding{
			Code:     CodePortInUse,
			Message:  "Ports " + portsSentence(swarmProxyPorts) + " must be free on every node. These containers publish them:",
			Items:    slices.Compact(items),
			Severity: SeverityError,
		})
	}
	if len(unchecked) > 0 {
		slices.Sort(unchecked)
		out = append(out, Finding{
			Code:     CodeNodesNotChecked,
			Message:  "These nodes could not be checked for something on ports " + portsSentence(swarmProxyPorts) + ". Enabling ingress goes on without them; if one of them already uses a port, its proxy will not start:",
			Items:    unchecked,
			Severity: SeverityWarning,
		})
	}
	return out, nil
}

// swarmServiceLabel is the label the engine puts on every container that runs a swarm service's task.
const swarmServiceLabel = "com.docker.swarm.service.name"

// checkNames refuses a swarm that holds a service, a network or a join token secret under a name the ingress reserves
// without the component label: the object is not TierPlan's and is never taken over (Pitfall 11). A labelled one is
// TierPlan's own and is reused.
func checkNames(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !in.Info.ManagerConnected {
		return nil, nil
	}
	if err := in.cl.load(ctx, s, in.Info.ID); err != nil {
		return nil, err
	}
	var items []string
	for _, row := range in.cl.svcs {
		if row.Name == ingress.ServiceName && !ingress.OwnsService(row.Name, row.Labels) {
			items = append(items, "service "+row.Name)
		}
	}
	for _, row := range in.cl.networks {
		if row.Name == ingress.NetworkName && !ingress.OwnsNetwork(row.Name, row.Labels) {
			items = append(items, "network "+row.Name)
		}
	}
	for _, row := range in.cl.secrets {
		if strings.HasPrefix(row.Name, ingress.SecretPrefix) && !ingress.OwnsSecret(row.Name, row.Labels) {
			items = append(items, "secret "+row.Name)
		}
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

// checkAddressPool refuses an ingress network that could not be made: a typed subnet that is not an IPv4 block or that
// overlaps a network, or no free block of NetworkPrefix in the swarm's default pool (D-02, D-07). A network of TierPlan's
// that exists already is reused, so nothing is checked.
func checkAddressPool(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if !in.Info.ManagerConnected {
		return nil, nil
	}
	if err := in.cl.load(ctx, s, in.Info.ID); err != nil {
		return nil, err
	}
	for _, n := range in.cl.networks {
		if ingress.OwnsNetwork(n.Name, n.Labels) {
			return nil, nil
		}
	}
	if in.Req.Subnet != "" {
		return checkTypedSubnet(ctx, s, in, in.cl.networks, "10.20.0.0/22", "a network on one of the nodes")
	}
	if s.d.AddressSpaces == nil {
		return nil, nil
	}
	inputs, err := s.d.AddressSpaces.AllocationInputs(ctx, in.Info.ID, false)
	if err != nil {
		return nil, fmt.Errorf("read the swarm's address pools: %w", err)
	}
	if _, _, err := netspace.LowestFree(inputs.Pools, inputs.Occupied, inputs.Avoid, NetworkPrefix); err != nil {
		if !errors.Is(err, netspace.ErrNoFreeBlock) {
			return nil, err
		}
		return []Finding{{
			Code:     CodeNoAddressSpace,
			Message:  fmt.Sprintf("No free /%d block is left in the swarm's default address pool for the ingress network. Free one, or type a subnet.", NetworkPrefix),
			Severity: SeverityError,
		}}, nil
	}
	return nil, nil
}

// checkTypedSubnet judges a subnet the person typed: it must be an IPv4 CIDR block and must not overlap one of the
// networks given, checked the way the stack editor checks an explicit subnet. example is the block the refusal writes as
// an example of the form, and elsewhere names a block that is taken by a network the read does not list.
func checkTypedSubnet(ctx context.Context, s *Service, in checkInput, networks []dockerx.NetworkRow, example, elsewhere string) ([]Finding, error) {
	candidate, err := netip.ParsePrefix(in.Req.Subnet)
	if err != nil || !candidate.Addr().Is4() {
		return []Finding{{
			Code:     CodeSubnetInvalid,
			Message:  "The subnet must be an IPv4 block written like " + example + ".",
			Severity: SeverityError,
		}}, nil
	}
	var same []netspace.MapItemNamed
	for _, n := range networks {
		var ps []netip.Prefix
		for _, raw := range n.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() {
				ps = append(ps, p.Masked())
			}
		}
		if len(ps) > 0 {
			same = append(same, netspace.MapItemNamed{MapItem: netspace.MapItem{Swarm: n.Scope == dockerx.ScopeSwarm, Prefixes: ps}, Name: n.Name})
		}
	}
	var items []string
	for _, f := range netspace.FindOverlaps(candidate, netspace.OverlapInput{Swarm: true, Same: same}) {
		if f.Kind == netspace.KindSameEnv {
			items = append(items, fmt.Sprintf("%s (%s)", f.Network, f.Subnet))
		}
	}
	if len(items) == 0 && s.d.AddressSpaces != nil {
		// A network of another node that the cluster read does not list has no name, but its block is taken.
		if inputs, err := s.d.AddressSpaces.AllocationInputs(ctx, in.Info.ID, false); err == nil {
			if sub, ok := netspace.FirstOverlap([]netip.Prefix{candidate.Masked()}, inputs.Occupied); ok {
				items = append(items, fmt.Sprintf("%s (%s)", elsewhere, sub))
			}
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	slices.Sort(items)
	return []Finding{{
		Code:     CodeSubnetOverlaps,
		Message:  "The subnet overlaps these networks. Choose another block:",
		Items:    slices.Compact(items),
		Severity: SeverityError,
	}}, nil
}

// The sentences of the HTTP-01 checks (UI-SPEC S4). The swarm refusal is the one where TierPlan runs on this swarm as a
// service that is not on the self network, or its own container could not be read; the plain-container refusal drops the
// self network, which a plain container cannot be given; the notice names the hosts the carve-out serves.
const (
	http01SwarmConflict     = "TierPlan runs on this swarm and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes on every node. Add the tierplan-self network to TierPlan's service, or switch TierPlan's own certificate to DNS-01. Then turn ingress on again."
	http01PlainContainer    = "TierPlan runs on this swarm and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes on every node. Switch TierPlan's own certificate to DNS-01. Then turn ingress on again."
	http01SwarmUnknown      = "TierPlan could not tell whether its own certificate needs port 80, which the ingress takes. If it uses the HTTP-01 challenge on this swarm, switch it to DNS-01 first."
	http01CarveOutNotice    = "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for %s to TierPlan, over the tierplan-self network, so the certificate keeps renewing."
	http01CarveOutNoHostsAs = "TierPlan's own hostnames"
)

// http01Place is what the HTTP-01 checks need to tell the person once SelfLocation has said where TierPlan runs and its own
// certificate is known to renew over HTTP-01 (D-12). Elsewhere is nothing. Where the carve-out reaches TierPlan, a swarm service
// on the self network or a standalone container TierPlan connects itself (D-04), it is a notice. Where TierPlan runs here and the
// carve-out cannot reach it, it is a refusal with the fix, so the ingress never takes port 80 from the renewal silently
// (SELF-04). What cannot be told is a warning.
func (s *Service) http01Place(place SelfPlace, conflict, unknown string) []Finding {
	refuse := func(msg string) []Finding {
		return []Finding{{Code: CodeHTTP01Conflict, Message: msg, Severity: SeverityError}}
	}
	switch {
	case place.Here == SelfHereNo:
		return nil
	case place.Here == SelfHereYes && (place.Kind == SelfKindStandalone || (place.Kind == SelfKindSwarmService && place.Attached)):
		return []Finding{s.http01CarveOut()}
	case place.Here == SelfHereYes && place.Kind == SelfKindSwarmContainer:
		return refuse(http01PlainContainer)
	case place.Here == SelfHereYes || place.EnvHere:
		// A swarm service not on the self network, or an environment TierPlan runs in whose own container could not be read.
		return refuse(conflict)
	}
	return []Finding{{Code: CodeHTTP01Unknown, Message: unknown, Severity: SeverityWarning}}
}

// http01CarveOut is the notice that the carve-out keeps TierPlan's own renewal working, naming the hosts of the public URL and
// of the agent URL, deduplicated and sorted; they are the finding's items too.
func (s *Service) http01CarveOut() Finding {
	hosts := s.ownHosts()
	named := http01CarveOutNoHostsAs
	if len(hosts) > 0 {
		named = joinAnd(hosts)
	}
	return Finding{Code: CodeHTTP01CarveOut, Message: fmt.Sprintf(http01CarveOutNotice, named), Items: hosts, Severity: SeverityNotice}
}

// ownHosts are the hosts of the instance's public URL and agent URL, deduplicated and sorted, empty when neither is set.
func (s *Service) ownHosts() []string {
	if s.d.SelfURLs == nil {
		return nil
	}
	urls := s.d.SelfURLs()
	var hosts []string
	for _, raw := range []string{urls.PublicURL, urls.AgentURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if h := ingress.NormalizeHost(u.Hostname()); h != "" {
			hosts = append(hosts, h)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// joinAnd writes names as a sentence does: "a", "a and b", "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// checkHTTP01 tells the person what the ingress taking port 80 on every node does to TierPlan's own certificate, when that
// certificate really renews over HTTP-01 (A-04, D-12). SelfLocation says where TierPlan runs; http01Place turns that into
// nothing, the carve-out notice, a refusal with the fix, or a warning when it cannot be told.
func checkHTTP01(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if s.d.OwnHTTP01InUse == nil {
		return nil, nil
	}
	inUse, err := s.d.OwnHTTP01InUse(ctx)
	if err != nil {
		s.log().Warn("could not read whether TierPlan's own certificate uses HTTP-01 for the ingress preflight", "error", err)
		return []Finding{{Code: CodeHTTP01Unknown, Message: http01SwarmUnknown, Severity: SeverityWarning}}, nil
	}
	if !inUse {
		// A self-signed install, certificate files, or Let's Encrypt on DNS-01: nothing of TierPlan's needs port 80 (A-04).
		return nil, nil
	}
	place, _, err := s.SelfLocation(ctx, in.Info)
	if err != nil {
		return nil, err
	}
	return s.http01Place(place, http01SwarmConflict, http01SwarmUnknown), nil
}

// checkURL refuses an install that does not know its own address, and an address whose host the ingress routes: the
// proxies dial TierPlan at it, so a route to it would send them through themselves.
func checkURL(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	raw := s.url()
	if raw == "" {
		return []Finding{{
			Code:     CodeURLUnset,
			Message:  "TierPlan does not know its own address, which the ingress proxies dial. Set the agent URL or the public URL in the settings, then try again.",
			Severity: SeverityError,
		}}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || s.d.Store == nil {
		return nil, nil
	}
	host := ingress.NormalizeHost(u.Hostname())
	world, err := s.d.Store.World(ctx, in.Info.ID, nil)
	if err != nil {
		return nil, err
	}
	if _, claimed := world.Claims[host]; !claimed && !world.Reserved[host] {
		return nil, nil
	}
	return []Finding{{
		Code:     CodeURLRouted,
		Message:  "TierPlan's own address is a hostname the ingress serves, so the proxies would reach TierPlan through themselves. Use an address that no route or reservation holds:",
		Items:    []string{host},
		Severity: SeverityError,
	}}, nil
}

// checkRedeploy warns about the stacks that hold routes and have to be redeployed to join the ingress network. The enable
// never changes a stack (D-14). A deployer with no Stacks lists none.
func checkRedeploy(ctx context.Context, s *Service, in checkInput) ([]Finding, error) {
	if s.d.Stacks == nil {
		return nil, nil
	}
	refs, err := s.d.Stacks.WithRoutes(ctx, in.Info.ID)
	if err != nil {
		return nil, fmt.Errorf("list the stacks that hold routes: %w", err)
	}
	var todo []StackRef
	for _, r := range refs {
		if !r.Attached {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return nil, nil
	}
	slices.SortFunc(todo, func(a, b StackRef) int { return strings.Compare(a.Name, b.Name) })
	*in.redeploy = todo
	items := make([]string, 0, len(todo))
	for _, r := range todo {
		items = append(items, r.Name)
	}
	return []Finding{{
		Code:     CodeRedeployNeeded,
		Message:  "These stacks hold routes and must be redeployed to join the ingress network. Enabling ingress does not change them:",
		Items:    items,
		Severity: SeverityWarning,
	}}, nil
}
