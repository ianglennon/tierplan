// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
)

const (
	// subnetChecksPerMinute is how many subnet checks one user may make in a minute: the picker
	// asks every 400 ms while a person types, so ordinary use is far under it (T-10-34).
	subnetChecksPerMinute = 120
	// subnetCheckBurst is how many checks may come at once before the rate applies.
	subnetCheckBurst = 20
	// subnetCheckIdle is how long a user's limiter is kept after their last check.
	subnetCheckIdle = 5 * time.Minute
)

// The codes of a subnet the check refuses.
const (
	subnetErrInvalid  = "subnet_invalid"
	subnetErrHostBits = "host_bits"
	subnetErrTooSmall = "too_small"
)

// smallestSubnetMask is the longest mask a network may have: a /29 is the smallest block with
// room for a gateway and a container, and what the create dialog's check accepts.
const smallestSubnetMask = 29

// SubnetCheckRequest is what the subnet picker asks. Every field is optional; the answer holds
// only the parts that were asked for.
type SubnetCheckRequest struct {
	Mask     *int   `json:"mask,omitempty" minimum:"16" maximum:"29" doc:"The size of the subnet to find a free block for. Left out, the recommended size is used when tasks is given, and else the mask of the first pool"`
	Tasks    *int   `json:"tasks,omitempty" minimum:"1" maximum:"10000" doc:"How many tasks are expected on the network. With it the answer carries needed, recommendedMask and recommendedUsable, or tooManyTasks"`
	Services *int   `json:"services,omitempty" minimum:"0" maximum:"1000" doc:"How many services are expected on the network; 1 when left out"`
	Nodes    *int   `json:"nodes,omitempty" minimum:"0" maximum:"1000" doc:"How many nodes the network spans; the swarm's node count when left out, 0 on a standalone host"`
	Subnet   string `json:"subnet,omitempty" maxLength:"64" doc:"A subnet a person typed, in CIDR form, to be checked as the create dialog does"`
	NodeID   string `json:"nodeId,omitempty" maxLength:"128" doc:"The node a local network (bridge, macvlan, ipvlan) is made on. Needed on a swarm for a local driver, because its pools are the node's own"`
	Driver   string `json:"driver,omitempty" enum:"bridge,overlay,macvlan,ipvlan" doc:"The driver of the network to be made: overlay (or none) on a swarm takes the swarm's pools, a local driver the pools of its node's daemon"`
	Suggest  bool   `json:"suggest,omitempty" doc:"True to find the lowest free block of the mask"`
}

// SubnetCheckInput is the path and body of the subnet check.
type SubnetCheckInput struct {
	EnvironmentPathInput
	Body SubnetCheckRequest
}

// SubnetSuggestionBody is a free block found.
type SubnetSuggestionBody struct {
	Subnet string `json:"subnet" doc:"The lowest free block of the size asked for, in CIDR form"`
	Pool   string `json:"pool" doc:"The pool the block lies in"`
}

// SubnetNoFreeBody says no block of the size is free.
type SubnetNoFreeBody struct {
	Mask int    `json:"mask" doc:"The size that was asked for"`
	Pool string `json:"pool" doc:"The pool that was searched; several pools are joined with a comma"`
}

// SubnetErrorBody is why a typed subnet is refused.
type SubnetErrorBody struct {
	Code      string `json:"code" enum:"subnet_invalid,host_bits,too_small" doc:"subnet_invalid: not in CIDR form. host_bits: host bits are set, so it is not a network address. too_small: smaller than a /29"`
	Corrected string `json:"corrected,omitempty" doc:"With host_bits, the network address the subnet is meant to be, for example 10.0.0.0/24 for 10.0.0.5/24"`
}

// SubnetFindingBody is one thing the checked subnet collides with. Only the fields its kind names
// are set.
type SubnetFindingBody struct {
	Kind        string `json:"kind" enum:"same_env,other_env,host_route,outside_pool" doc:"same_env: a network of this environment. other_env: a network of another environment the caller may read. host_route: a route of a host. outside_pool: the subnet is in none of the pools"`
	Severity    string `json:"severity" enum:"blocking,warning" doc:"blocking: creating the network is refused until createAnyway is true (same_env only), or for good when noOverride is true. warning: shown, never stops anything"`
	NoOverride  bool   `json:"noOverride,omitempty" doc:"same_env only: true when the network to be made and the network it overlaps are both swarm networks on a swarm. A swarm never allocates two of its networks with overlapping subnets, so createAnyway cannot help and the create answers 422 subnet_overlap_swarm"`
	Network     string `json:"network,omitempty" doc:"same_env, other_env: the network the subnet overlaps"`
	Subnet      string `json:"subnet,omitempty" doc:"same_env, other_env: the subnet of that network that overlaps"`
	Node        string `json:"node,omitempty" doc:"The node a local network (same_env, other_env) or a route (host_route) is on; absent for a swarm-wide network"`
	Environment string `json:"environment,omitempty" doc:"other_env: the name of the other environment"`
	Route       string `json:"route,omitempty" doc:"host_route: the route's destination in CIDR form"`
	Iface       string `json:"iface,omitempty" doc:"host_route: the interface the route is on"`
	Pool        string `json:"pool,omitempty" doc:"outside_pool: the pool the subnet should be in; several pools are joined with a comma"`
}

// RouteCheckBody says, for one node, whether its host's routes were read for the check, or why not.
type RouteCheckBody struct {
	Node   string `json:"node" doc:"The node's name"`
	NodeID string `json:"nodeId,omitempty" doc:"The node's ID; absent for a standalone host"`
	State  string `json:"state" enum:"ok,old_agent,unmounted,offline,failed" doc:"ok: the routes were read. old_agent: the agent is older than this check and cannot report routes. unmounted: the agent (or TierPlan itself, with local true) cannot read the host's route file because it is not mounted. offline: the node's agent is not connected. failed: the read errored or its answer was not a valid list of routes"`
	Local  bool   `json:"local" doc:"True for the host TierPlan itself runs on, read from the route file mounted into TierPlan's own container"`
	Fix    string `json:"fix,omitempty" doc:"With unmounted on a swarm agent, the command that adds the route mount to the agent service. Absent otherwise: the wording of the standalone and local fixes is the interface's"`
}

// SubnetCheckBody is the answer of the subnet check.
type SubnetCheckBody struct {
	Suggestion        *SubnetSuggestionBody `json:"suggestion,omitempty" doc:"The lowest free block of the size asked for, clear of every IPv4 subnet of every network on every node. It reserves nothing: two people asking at once get the same block, and the create refuses the second. Absent when suggest is off, when no pool is known (a swarm no manager answered for, or a local driver with no nodeId) and when noFreeBlock is set"`
	NoFreeBlock       *SubnetNoFreeBody     `json:"noFreeBlock,omitempty" doc:"Present when no block of the size is free in the pools; never an error"`
	Needed            *int                  `json:"needed,omitempty" minimum:"0" doc:"Addresses the expected tasks need: ceil((2 x tasks + services + nodes) x 1.25) + 3. Present when tasks was given"`
	RecommendedMask   *int                  `json:"recommendedMask,omitempty" minimum:"24" maximum:"28" doc:"The smallest of /28 to /24 whose usable addresses hold needed. Absent with tooManyTasks"`
	RecommendedUsable *int                  `json:"recommendedUsable,omitempty" minimum:"0" doc:"The usable addresses of recommendedMask: the size less the network, broadcast and gateway addresses"`
	TooManyTasks      bool                  `json:"tooManyTasks" doc:"True when not even a /24 holds needed"`
	SubnetError       *SubnetErrorBody      `json:"subnetError,omitempty" doc:"Why the typed subnet is refused; absent when it is fine or none was sent"`
	IPv6NotChecked    bool                  `json:"ipv6NotChecked" doc:"True when the typed subnet is IPv6, which is not checked"`
	RouteChecks       []RouteCheckBody      `json:"routeChecks" doc:"With a valid IPv4 subnet or suggest, one entry per node of the environment saying whether its host's routes were read for the check. Never null; empty when neither was asked for"`
	Findings          []SubnetFindingBody   `json:"findings" doc:"What the typed subnet collides with, never null: this environment's networks on every node (blocking, or blocking for good with noOverride), other environments' networks the caller may read, host routes and the pools it lies outside of (warnings). In that order, and within a kind by name or address. Empty when the subnet is fine, absent subnet, invalid or IPv6"`
}

// SubnetCheckOutput wraps SubnetCheckBody for huma.
type SubnetCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SubnetCheckBody
}

// subnetLimiter is the per-user limit on subnet checks. It forgets a user who has been idle.
type subnetLimiter struct {
	mu    sync.Mutex
	by    map[string]*subnetLimit
	swept time.Time
}

type subnetLimit struct {
	lim  *rate.Limiter
	seen time.Time
}

func (l *subnetLimiter) allow(user string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.by == nil {
		l.by = map[string]*subnetLimit{}
	}
	if now.Sub(l.swept) > time.Minute {
		for id, e := range l.by {
			if now.Sub(e.seen) > subnetCheckIdle {
				delete(l.by, id)
			}
		}
		l.swept = now
	}
	e, ok := l.by[user]
	if !ok {
		e = &subnetLimit{lim: rate.NewLimiter(rate.Every(time.Minute/subnetChecksPerMinute), subnetCheckBurst)}
		l.by[user] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

// registerSubnetCheck registers the route the subnet picker asks for a size recommendation, a
// free block and a check of a typed subnet.
func registerSubnetCheck(r *httpx.Router, d AddressSpaceDeps) {
	limiter := &subnetLimiter{}
	httpx.Register(r, huma.Operation{
		OperationID: "checkSubnet", Method: http.MethodPost, Path: "/api/env/{envId}/address-space/subnet-check",
		Summary: "Suggest a free subnet, recommend a size and check a typed subnet",
		Description: "With suggest true, answers suggestion: the lowest free block of mask (or of the recommended size) inside the pools of the network to be made, clear of every IPv4 subnet of every network on every node. " +
			"An overlay (or no driver) on a swarm takes the swarm's pools; a local driver on a swarm takes the daemon pools of the node nodeId (or Docker's defaults when it sets none); a standalone host takes its pools as the address space reads them. " +
			"When no block of the size is free the answer is noFreeBlock, never an error. The suggestion reserves nothing. " +
			"With tasks, answers needed, recommendedMask and recommendedUsable, or tooManyTasks when not even a /24 holds them. " +
			"With subnet, answers subnetError (subnet_invalid, host_bits with corrected, too_small) as the create dialog's server check does; an IPv6 subnet has no error and ipv6NotChecked is true. " +
			"With a valid IPv4 subnet, answers findings: the networks of this environment it overlaps (blocking: the create refuses it without createAnyway), networks of other environments the caller may read from their last read, and the pools it lies outside of (warnings). The overlap rule is the subnet map's: an overlay against every network, a local network of a node against that node's local networks and every swarm network. " +
			"IPv4 only. Reads only the cached view of the environment. At most 120 checks a minute per user: 429 rate_limited. Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SubnetCheckInput) (*SubnetCheckOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.AddressSpaces == nil || d.Aggregator == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			now := time.Now
			if d.Now != nil {
				now = d.Now
			}
			if !limiter.allow(p.ID, now()) {
				return nil, httpx.Problem(http.StatusTooManyRequests, problemRateLimited, "Too many checks. Wait a minute and try again.")
			}
			body, err := checkSubnet(ctx, d, env.ID, in.Body)
			if err != nil {
				return nil, err
			}
			return &SubnetCheckOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// checkSubnet answers one subnet check. The size recommendation and the check of the typed
// subnet's form need no read at all; a suggestion or the findings of a valid subnet read the
// (cached) address space.
func checkSubnet(ctx context.Context, d AddressSpaceDeps, envID string, req SubnetCheckRequest) (SubnetCheckBody, error) {
	out := SubnetCheckBody{Findings: []SubnetFindingBody{}, RouteChecks: []RouteCheckBody{}}
	if req.Subnet != "" {
		out.SubnetError, out.IPv6NotChecked = validateTypedSubnet(req.Subnet)
	}
	var candidate netip.Prefix
	if req.Subnet != "" && out.SubnetError == nil && !out.IPv6NotChecked {
		candidate, _ = netip.ParsePrefix(req.Subnet)
		candidate = candidate.Masked()
	}
	space, agg, err := readForSubnetCheck(ctx, d, envID, req, candidate.IsValid())
	if err != nil {
		return out, err
	}

	if req.Tasks != nil {
		services := 1
		if req.Services != nil {
			services = *req.Services
		}
		nodes := 0
		switch {
		case req.Nodes != nil:
			nodes = *req.Nodes
		case agg.Counts.Nodes != nil:
			// Only a swarm manager reports a node count; a standalone host spans none.
			nodes = *agg.Counts.Nodes
		}
		needed := netspace.Needed(*req.Tasks, services, nodes)
		out.Needed = &needed
		if mask, ok := netspace.RecommendMask(needed); ok {
			usable := netspace.Usable(mask)
			out.RecommendedMask, out.RecommendedUsable = &mask, &usable
		} else {
			out.TooManyTasks = true
		}
	}

	var pools []netspace.Pool
	if req.Suggest || candidate.IsValid() {
		var perr error
		if pools, perr = poolsForSubnet(ctx, d, envID, req, space); perr != nil {
			// A suggestion needs the pools. A check of a typed subnet only loses its
			// outside-the-pool note.
			if req.Suggest {
				return out, perr
			}
			pools = nil
		}
	}
	var routes []netspace.NodeRoute
	if req.Suggest || candidate.IsValid() {
		out.RouteChecks, routes = readHostRoutes(ctx, d, envID, space, agg)
	}
	all, node := subnetScope(space, req, agg)
	candidateSwarm := space.Swarm && (req.Driver == "" || req.Driver == dockerx.DriverOverlay)
	if req.Suggest {
		if err := suggestSubnet(pools, scopedRoutes(routes, all, node), req, agg, &out); err != nil {
			return out, err
		}
	}
	if candidate.IsValid() {
		out.Findings = subnetFindings(ctx, d, envID, candidate, agg, pools, routes, all, node, candidateSwarm)
	}
	return out, nil
}

// readForSubnetCheck reads the cached address space (for a suggestion or the findings of a
// typed subnet) and the cached aggregate (for those, or for the swarm's node count of a size
// recommendation), and nothing otherwise.
func readForSubnetCheck(ctx context.Context, d AddressSpaceDeps, envID string, req SubnetCheckRequest, checked bool) (envs.AddressSpace, envs.Aggregate, error) {
	var space envs.AddressSpace
	var agg envs.Aggregate
	if req.Suggest || checked {
		var err error
		if space, err = d.AddressSpaces.Read(ctx, envID); err != nil {
			return space, agg, snapshotProblem(d.Log, envID, err)
		}
	}
	if req.Suggest || checked || (req.Tasks != nil && req.Nodes == nil) {
		var err error
		if agg, _, err = d.Aggregator.Snapshot(ctx, envID); err != nil {
			return space, agg, snapshotProblem(d.Log, envID, err)
		}
	}
	return space, agg, nil
}

// subnetScope says which networks a network of the request is compared with: all is true for an
// overlay (or no driver), for a host that is not a swarm and when the node of a local network is
// not known; else node is the name of the node it is local to.
func subnetScope(space envs.AddressSpace, req SubnetCheckRequest, agg envs.Aggregate) (all bool, node string) {
	local := req.Driver != "" && req.Driver != "overlay"
	if !space.Swarm || !local || req.NodeID == "" {
		return true, ""
	}
	if name, ok := nodeName(agg, req.NodeID); ok {
		return false, name
	}
	return true, ""
}

// sameEnvOverlaps are the networks of this environment whose subnets overlap the subnet of a
// create, by the scope rule of the subnet map: a cluster network or a host's only network is
// compared with every network, a local network of a node with that node's local networks and
// every swarm network. An IPv6 subnet is not checked. The network dialog's create and the
// networks a service apply or create makes (stagedOverlapGate) hold to this one rule.
func sameEnvOverlaps(subnet string, agg envs.Aggregate, localOnSwarm bool, nodeID string) []netspace.Finding {
	candidate, err := netip.ParsePrefix(subnet)
	if err != nil {
		return nil
	}
	in := netspace.OverlapInput{Swarm: true, Same: envs.NetworkItems(agg.Networks)}
	if localOnSwarm {
		if name, ok := nodeName(agg, nodeID); ok {
			in.Swarm, in.Node = false, name
		}
	}
	return netspace.FindOverlaps(candidate, in)
}

// overrideAllowed says whether createAnyway can let the overlap f through. It cannot when the
// network to be made is swarm-scoped on a swarm (candidateSwarm) and the network it overlaps is
// a swarm network too: the managers' allocator refuses such a subnet after the create has
// already answered, and leaves a stub network that tasks hang on (UAT G-10-8). Every other
// overlap is the engine's to accept or refuse when the create is sent: a node-local network
// lives in the node's own address space, and the swarm allocator does not see it. A finding
// of another kind is not an overlap that blocks, so there is nothing to override.
func overrideAllowed(candidateSwarm bool, f netspace.Finding) bool {
	return !candidateSwarm || f.Kind != netspace.KindSameEnv || !f.Swarm
}

// subnetFindings is what the typed subnet collides with.
func subnetFindings(ctx context.Context, d AddressSpaceDeps, envID string, candidate netip.Prefix, agg envs.Aggregate, pools []netspace.Pool, routes []netspace.NodeRoute, all bool, node string, candidateSwarm bool) []SubnetFindingBody {
	in := netspace.OverlapInput{
		Swarm: all, Node: node,
		Same:   envs.NetworkItems(agg.Networks),
		Other:  otherEnvironments(ctx, d, envID),
		Routes: routes,
		Pools:  pools,
	}
	found := netspace.FindOverlaps(candidate, in)
	out := make([]SubnetFindingBody, 0, len(found))
	for _, f := range found {
		out = append(out, SubnetFindingBody{
			Kind: string(f.Kind), Severity: string(f.Severity), Network: f.Network, Subnet: f.Subnet, Node: f.Node,
			Environment: f.Environment, Route: f.Route, Iface: f.Iface, Pool: f.Pool,
			NoOverride: !overrideAllowed(candidateSwarm, f),
		})
	}
	return out
}

// dockerIface reports whether name is one of Docker's own bridges, whose routes are the networks
// Docker made and are not a LAN or VPN of the host.
func dockerIface(name string) bool {
	return name == "docker0" || name == "docker_gwbridge" || strings.HasPrefix(name, "br-")
}

// readHostRoutes reads the routes of every node's host (cached by the route reader) and answers a
// route check per node and the usable routes of the nodes that were read: not a route that is the
// subnet of a Docker network of the environment, and not one on a Docker bridge. With no route
// reader every node is reported as failed and no stream is opened.
func readHostRoutes(ctx context.Context, d AddressSpaceDeps, envID string, space envs.AddressSpace, agg envs.Aggregate) ([]RouteCheckBody, []netspace.NodeRoute) {
	var nodes []envs.NodeRoutes
	if d.HostRoutes != nil {
		var err error
		if nodes, err = d.HostRoutes.Get(ctx, envID); err != nil {
			d.Log.Debug("the host routes of an environment could not be read", "environment", envID, "error", err)
			nodes = nil
		}
	}
	if nodes == nil {
		for _, n := range agg.Nodes {
			nodes = append(nodes, envs.NodeRoutes{Node: n.Name, NodeID: n.ID, State: envs.RouteCheckFailed})
		}
	}
	docker := map[netip.Prefix]bool{}
	for _, n := range agg.Networks {
		for _, raw := range n.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() {
				docker[p.Masked()] = true
			}
		}
	}
	checks := make([]RouteCheckBody, 0, len(nodes))
	var routes []netspace.NodeRoute
	for _, n := range nodes {
		c := RouteCheckBody{Node: n.Node, NodeID: n.NodeID, State: n.State, Local: n.Local}
		if n.State == envs.RouteCheckUnmounted && !n.Local && space.Swarm {
			c.Fix = agents.HostRouteMountCommand("swarm", agents.InstallParams{})
		}
		checks = append(checks, c)
		if n.State != envs.RouteCheckOK {
			continue
		}
		for _, r := range n.Routes {
			if dockerIface(r.Iface) || docker[r.Dst.Masked()] {
				continue
			}
			routes = append(routes, netspace.NodeRoute{Node: n.Node, Iface: r.Iface, Dst: r.Dst})
		}
	}
	return checks, routes
}

// scopedRoutes are the destinations of the routes that concern a network of the scope: every
// node's for a swarm-wide network, one node's for a local one.
func scopedRoutes(routes []netspace.NodeRoute, all bool, node string) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range routes {
		if all || r.Node == node {
			out = append(out, r.Dst)
		}
	}
	return out
}

// canReadNetworks reports whether the grants listed for each environment include reading its
// networks.
func canReadNetworks(perEnv map[string][]rbac.Perm, id string) bool {
	return slices.Contains(perEnv[id], rbac.PermNetworksRead) ||
		slices.Contains(perEnv[rbac.AllEnvironments], rbac.PermNetworksRead)
}

// otherEnvironments are the networks of the other environments the caller may read networks of,
// from the last read of each: nothing is read from an engine, and an environment never read
// contributes nothing (T-10-39).
func otherEnvironments(ctx context.Context, d AddressSpaceDeps, envID string) []netspace.OtherEnv {
	p := httpx.PrincipalFrom(ctx)
	if p == nil || d.Checker == nil || d.Store == nil {
		return nil
	}
	cached := d.AddressSpaces.Environments()
	if !slices.ContainsFunc(cached, func(id string) bool { return id != envID }) {
		return nil
	}
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		d.Log.Warn("the permissions of a caller could not be read for a subnet check", "error", err)
		return nil
	}
	rows, err := d.Store.RQ.ListEnvironments(ctx)
	if err != nil {
		d.Log.Warn("the environments could not be listed for a subnet check", "error", err)
		return nil
	}
	names := make(map[string]string, len(rows))
	for _, r := range rows {
		names[r.ID] = r.Name
	}
	var out []netspace.OtherEnv
	for _, id := range cached {
		name, known := names[id]
		if id == envID || !known || !canReadNetworks(perEnv, id) {
			continue
		}
		if nets, _, ok := d.AddressSpaces.CachedNetworks(id); ok {
			out = append(out, netspace.OtherEnv{Name: name, Networks: nets})
		}
	}
	return out
}

// suggestSubnet fills the suggestion, or noFreeBlock, of out from the pools to search, clear of
// every subnet of the environment and of avoid, the host routes in scope.
func suggestSubnet(pools []netspace.Pool, avoid []netip.Prefix, req SubnetCheckRequest, agg envs.Aggregate, out *SubnetCheckBody) error {
	if len(pools) == 0 {
		return nil
	}
	mask := netspace.DefaultSwarmSubnetSize
	switch {
	case req.Mask != nil:
		mask = *req.Mask
	case out.RecommendedMask != nil:
		mask = *out.RecommendedMask
	case pools[0].Size > 0 && pools[0].Size <= smallestSubnetMask:
		mask = pools[0].Size
	}

	var occupied []netip.Prefix
	for _, n := range agg.Networks {
		for _, raw := range n.Subnets {
			if sp, err := netip.ParsePrefix(raw); err == nil && sp.Addr().Is4() {
				occupied = append(occupied, sp.Masked())
			}
		}
	}
	block, pool, err := netspace.LowestFree(pools, occupied, avoid, mask)
	if errors.Is(err, netspace.ErrNoFreeBlock) {
		names := make([]string, 0, len(pools))
		for _, p := range pools {
			names = append(names, p.Base.Masked().String())
		}
		out.NoFreeBlock = &SubnetNoFreeBody{Mask: mask, Pool: strings.Join(names, ", ")}
		return nil
	}
	if err != nil {
		return err
	}
	out.Suggestion = &SubnetSuggestionBody{Subnet: block.String(), Pool: pool.Base.Masked().String()}
	return nil
}

// poolsForSubnet is the pools a network of the request's driver is made from. An empty list
// means no pool is known to search.
func poolsForSubnet(ctx context.Context, d AddressSpaceDeps, envID string, req SubnetCheckRequest, space envs.AddressSpace) ([]netspace.Pool, error) {
	local := req.Driver != "" && req.Driver != "overlay"
	if !space.Swarm || !local {
		pools := make([]netspace.Pool, 0, len(space.Pools))
		for _, p := range space.Pools {
			pools = append(pools, p.Pool)
		}
		return pools, nil
	}
	// A local network of a swarm is allocated by the daemon of its node.
	if req.NodeID == "" {
		return nil, nil
	}
	var reported dockerx.AddressPools
	err := d.Aggregator.OnNode(ctx, envID, req.NodeID, func(c *dockerx.Client) (err error) {
		reported, err = c.AddressPools(ctx)
		return err
	})
	if err != nil {
		_, problem := networkWriteProblem(d.Log, envID, err)
		return nil, problem
	}
	var pools []netspace.Pool
	for _, p := range reported.DaemonPools {
		pools = append(pools, netspace.Pool{Base: p.Base, Size: p.Size})
	}
	if len(pools) == 0 {
		pools = netspace.DockerDefaultPools
	}
	return pools, nil
}

// validateTypedSubnet judges a subnet a person typed as the create dialog's server check does.
// An IPv6 subnet has no error and is reported unchecked.
func validateTypedSubnet(text string) (*SubnetErrorBody, bool) {
	sp, err := netip.ParsePrefix(text)
	if err != nil {
		return &SubnetErrorBody{Code: subnetErrInvalid}, false
	}
	if !sp.Addr().Is4() {
		return nil, true
	}
	if sp.Bits() > smallestSubnetMask {
		return &SubnetErrorBody{Code: subnetErrTooSmall}, false
	}
	if masked := sp.Masked(); masked != sp {
		return &SubnetErrorBody{Code: subnetErrHostBits, Corrected: masked.String()}, false
	}
	return nil, false
}
