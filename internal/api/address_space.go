// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/store"
)

// AddressSpaceDeps are the dependencies of the address-space routes of the networks phase.
// Every dependency the phase's routes use is here, so a later route is added without new
// wiring.
type AddressSpaceDeps struct {
	// AddressSpaces reads the pools of an environment and how full each is.
	AddressSpaces *envs.AddressSpaces
	// Aggregator and Clusters reach the engines of an environment.
	Aggregator *envs.Aggregator
	Clusters   *envs.Clusters
	// Store holds what the phase persists beside Docker's own state.
	Store *store.Store
	Audit *audit.Writer
	// Checker says what the caller may do beyond the permission of a route.
	Checker *rbac.Checker
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// AddressSpaceOutput wraps AddressSpaceBody for huma.
type AddressSpaceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AddressSpaceBody
}

// AddressSpaceBody is the answer of GET /api/env/{envId}/address-space.
type AddressSpaceBody struct {
	EnvType      string             `json:"envType" enum:"swarm,standalone" doc:"What the environment is"`
	Measured     bool               `json:"measured" doc:"True when the numbers were read from the swarm's own pools through a manager. False for a standalone host, whose rows are all estimated, and when notMeasured is set"`
	NotMeasured  string             `json:"notMeasured,omitempty" enum:"no_manager" doc:"Why a swarm could not be measured: no_manager means no manager answered. The pools last read are returned with state unknown and nothing counted. Absent when the swarm was measured or the environment is standalone"`
	Pools        []AddressPoolBody  `json:"pools" doc:"One row per address pool, in the order the swarm or the daemon lists them. Empty when a swarm was never measured"`
	Overlays     []OverlayUsageBody `json:"overlays" doc:"Every swarm overlay network, ingress included, with how many of its addresses are handed out against what it can still hand out, the fullest first and then by name. Empty for a standalone host. A network whose count could not be read has measured false (Not measured: an engine before 29 on the manager, or no swarm count), never a zero"`
	Ingress      *OverlayUsageBody  `json:"ingress,omitempty" doc:"The ingress network again (it is also in overlays), with outsidePool saying whether it lies outside every pool of the swarm. Absent when the swarm lists none"`
	NotReporting []string           `json:"notReporting" doc:"The nodes that did not answer, whose networks are left out of the counts and of the map. Empty when every node answered"`

	Networks          []AddressMapRowBody `json:"networks" doc:"The subnet map: every network of every reporting node, one row for a swarm-wide network and one per node for a local one, ordered by the numeric start address of the first IPv4 subnet, then its mask, name and node; a network with no IPv4 subnet is last. At most 5000 rows (the page shows 500 and searches the rest); networksTotal is the full count. The overlaps are decided on the server over every row, before the cap"`
	NetworksTotal     int                 `json:"networksTotal" minimum:"0" doc:"How many networks the map holds, whatever the cap"`
	NetworksTruncated bool                `json:"networksTruncated" doc:"True when networksTotal is more than the rows returned"`

	Orphans         []OrphanRowBody      `json:"orphans" doc:"The networks nothing uses any more, decided on the server (never in the browser): orphaned (it carries a stack or compose-project label and that stack is gone), unused (no stack ever owned it) and cant_tell (Coaming cannot be sure, see orphansWithheld). A network is listed only after every node, every service spec and every live task were checked and it has been idle for 24 hours, counted from the later of its creation and the last time Coaming saw it in use; at exactly 24 hours it is listed. The engine's own networks, networks in use, kept networks, networks a Coaming stack declares external and networks of a stack with an operation running are never listed. A local network is judged per node, one row per node. Orphaned first, then unused, then cant_tell, each by name"`
	OrphansWithheld *OrphansWithheldBody `json:"orphansWithheld,omitempty" doc:"Present when the check could not judge every network: which nodes did not report, whether no manager answered, and which stacks did not load. While it is present no network that depends on the missing part is called orphaned or unused. Absent when nothing was withheld"`
}

// OrphanRowBody is one network the orphan check lists.
type OrphanRowBody struct {
	NetworkID       string   `json:"networkId" doc:"The network's full ID"`
	Name            string   `json:"name"`
	Node            string   `json:"node" doc:"The node a local network is on, or Cluster for a swarm-wide network"`
	NodeID          string   `json:"nodeId" doc:"The ID of the node the network was read from; for a swarm-wide network, the manager in use"`
	Scope           string   `json:"scope" enum:"swarm,local"`
	Subnets         []string `json:"subnets" doc:"The IPAM subnets in CIDR notation; empty when there is none"`
	Class           string   `json:"class" enum:"orphaned,unused,cant_tell" doc:"orphaned: its stack is gone. unused: no stack ever owned it. cant_tell: Coaming cannot be sure, because a node did not report, no manager answered or a stack did not load"`
	FormerOwner     string   `json:"formerOwner" doc:"The stack or compose project the network's labels name; empty for a network with no owner label"`
	FormerOwnerKind string   `json:"formerOwnerKind" enum:"stack,project," doc:"What formerOwner is, or empty when the network has no owner label"`
	IdleSince       string   `json:"idleSince" format:"date-time" doc:"When the network's idle time started: the later of its creation and the last time Coaming saw it in use"`
	IdleSinceSource string   `json:"idleSinceSource" enum:"seen,created" doc:"seen when idleSince is the last time Coaming saw the network in use, created when it is the network's creation"`
}

// OrphansWithheldBody says what the orphan check could not see.
type OrphansWithheldBody struct {
	Nodes          []string `json:"nodes" doc:"The nodes that did not report, because of which a network was not judged"`
	Stacks         []string `json:"stacks" doc:"The Coaming stacks that did not load, whose external networks are therefore not known"`
	ManagerMissing bool     `json:"managerMissing" doc:"True when a swarm's managers did not answer, so its services and tasks were not read"`
}

// maxMapRows is the most rows of the subnet map one answer carries (T-10-15). The page renders
// the first 500 and searches the rest.
const maxMapRows = 5000

// AddressMapRowBody is one network on the subnet map.
type AddressMapRowBody struct {
	ID                 string            `json:"id" doc:"The network's full ID"`
	Name               string            `json:"name"`
	Node               string            `json:"node" doc:"The node a local network is on, or Cluster for a swarm-wide network"`
	NodeID             string            `json:"nodeId" doc:"The ID of the node the network was read from; for a swarm-wide network, the manager in use"`
	Scope              string            `json:"scope" enum:"swarm,local" doc:"swarm for a cluster-wide network, local for every other"`
	Driver             string            `json:"driver"`
	Subnets            []string          `json:"subnets" doc:"The IPAM subnets in CIDR notation; empty for host and none and any network without one"`
	Owner              string            `json:"owner" doc:"The stack (com.docker.stack.namespace) or compose project (com.docker.compose.project) label value; for a swarm network with neither, the first service by name whose spec names it; empty otherwise"`
	OwnerKind          string            `json:"ownerKind" enum:"stack,project,service,none" doc:"What owner is: a stack, a compose project, a service, or none"`
	OwnerStackID       string            `json:"ownerStackId" doc:"The ID of the Coaming stack of this environment the network belongs to, so the page can link to it. Empty when no Coaming stack of the environment owns it"`
	AttachedContainers int               `json:"attachedContainers" doc:"Containers, running or not, on any reporting node that are attached to the network. A local network counts the containers of its own node"`
	AttachedServices   int               `json:"attachedServices" doc:"Swarm services whose spec names the network, even with no running task, as of the last cluster read. Always 0 off a swarm and for a local network"`
	Created            string            `json:"created,omitempty" format:"date-time"`
	BuiltIn            bool              `json:"builtIn" doc:"True for bridge, host, none, ingress, docker_gwbridge and any ingress network: the engine's own"`
	Ingress            bool              `json:"ingress" doc:"True for the swarm's ingress (routing mesh) network"`
	Usage              *NetworkUsageBody `json:"usage,omitempty" doc:"Overlay networks only: addresses used against what the subnet can still hand out. Absent for every other network and when the count could not be read, never a zero"`
	Overlaps           []MapOverlapBody  `json:"overlaps" doc:"The other networks whose IPv4 subnets overlap this one's, within scope: a swarm network against every swarm network and every local network, a local network against the local networks of its own node and every swarm network. Empty when none. At most 50 are named"`
	Order              int               `json:"order" minimum:"0" doc:"This row's position in the address order (its index in networks)"`
}

// MapOverlapBody names the other network of an overlap.
type MapOverlapBody struct {
	Name   string `json:"name" doc:"The other network's name"`
	Subnet string `json:"subnet" doc:"The first subnet of the other network that overlaps this row's"`
	Node   string `json:"node" doc:"The node of the other network when it is local; empty for a swarm-wide network"`
}

// OverlayUsageBody is one swarm overlay network and how full its subnet is.
type OverlayUsageBody struct {
	ID          string   `json:"id" doc:"The network's full ID"`
	Name        string   `json:"name" doc:"The network's name"`
	Subnets     []string `json:"subnets" doc:"The network's IPv4 subnets in CIDR form"`
	Used        int64    `json:"used" minimum:"0" doc:"Addresses the swarm has handed out on the network's IPv4 subnets, the network and broadcast addresses, gateway, virtual IPs and load-balancer endpoints included. 0 with measured false"`
	Capacity    int64    `json:"capacity" minimum:"0" doc:"Addresses handed out plus those the swarm can still hand out, so a /24 reads 256 and an ip-range /25 inside a /24 reads 129. 0 with measured false"`
	Percent     int      `json:"percent" minimum:"0" maximum:"100" doc:"used over capacity as a whole percent, rounded down. 0 with measured false"`
	State       string   `json:"state" enum:"normal,getting_full,almost_full,unknown" doc:"normal under 70%, getting_full from 70% and almost_full from 90%, decided on the exact ratio. unknown when measured is false"`
	Measured    bool     `json:"measured" doc:"False when the count could not be read: the engine on the manager is older than 29 (no address status), the manager reports no swarm count, or the read failed. The numbers are then 0 and mean nothing"`
	Ingress     bool     `json:"ingress" doc:"True for the swarm's ingress (routing mesh) network"`
	OutsidePool bool     `json:"outsidePool" doc:"Only ever true for the ingress network: none of its IPv4 subnets lies inside a pool of the swarm, so no pool row counts it. Ingress inside a pool is counted in that pool once"`
}

// overlayBody is the wire form of an overlay.
func overlayBody(o envs.OverlayUsage) OverlayUsageBody {
	return OverlayUsageBody{
		ID: o.ID, Name: o.Name, Subnets: append([]string{}, o.Subnets...),
		Used: addresses(o.Used), Capacity: addresses(o.Capacity), Percent: o.Percent, State: string(o.State),
		Measured: o.Measured, Ingress: o.Ingress, OutsidePool: o.OutsidePool,
	}
}

// addresses is a count of addresses as the wire number. A measured count is at most a few times
// 2^32 (the reader refuses more), far under the int64 range; the clamp only guards the cast.
func addresses(n uint64) int64 { return int64(min(n, math.MaxInt64)) } //nolint:gosec // G115: held to MaxInt64 on this line

// AddressPoolBody is one address pool and how much of it is in use.
type AddressPoolBody struct {
	CIDR               string `json:"cidr" doc:"The pool's range, for example 10.0.0.0/8"`
	Mask               int    `json:"mask" minimum:"1" maximum:"30" doc:"The mask of each network allocated from the pool (the swarm's SubnetSize, or the daemon's pool size)"`
	Used               int64  `json:"used" minimum:"0" doc:"How many blocks of the pool, each of the size the mask gives, any subnet overlaps. A /28 inside one /26 block uses 1; a /16 in a /24-size pool uses 256. IPv6 subnets are not counted. At most 2^30, so it always fits"`
	Capacity           int64  `json:"capacity" minimum:"0" doc:"How many blocks the pool holds in all (at most 2^30). Zero cannot happen for a listed pool"`
	Percent            int    `json:"percent" minimum:"0" maximum:"100" doc:"used over capacity as a whole percent, rounded down, so 69.99% is 69"`
	State              string `json:"state" enum:"normal,getting_full,almost_full,unknown" doc:"normal under 70%, getting_full from 70% and almost_full from 90%, decided on the exact ratio and never on the rounded percent. unknown when the pool could not be measured"`
	LargestFree        string `json:"largestFree" doc:"The largest free block made of whole blocks of the pool, aligned to the pool, in CIDR form. Empty when no block is free or the pool was not measured"`
	LargestFreeSubnets int64  `json:"largestFreeSubnets" minimum:"0" doc:"How many blocks of the pool's mask largestFree holds. 0 with an empty largestFree"`
	Source             string `json:"source" enum:"swarm,daemon,defaults,entered" doc:"Where the pool came from: swarm (the swarm's own, or its built-in 10.0.0.0/8), daemon (the host's default-address-pools setting), defaults (Docker's built-in pools, because the host sets none) or entered (a pool a person entered)"`
	Estimated          bool   `json:"estimated" doc:"True when the numbers cannot be taken as exact. Every standalone row is: the daemon skips pool slots that overlap routes of the host, and engines differ"`
}

// RegisterAddressSpace registers the address-space routes. Nothing here is cached beyond the
// server's short read of the engines: the numbers are read live (the browser is told not to
// keep them).
func RegisterAddressSpace(r *httpx.Router, d AddressSpaceDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	httpx.Register(r, huma.Operation{
		OperationID: "getAddressSpace", Method: http.MethodGet, Path: "/api/env/{envId}/address-space",
		Summary: "The address pools of an environment and how full each is",
		Description: "One row per address pool. A swarm answers the pools its manager reports (Swarm.Cluster.DefaultAddrPool of the engine info; none reported means the swarm's built-in 10.0.0.0/8) at the swarm's subnet size, measured: " +
			"every network of the cluster, ingress included when it lies inside a pool, and every node's local network that overlaps a pool is counted. " +
			"A standalone host answers the pools its daemon reports (source daemon) or, when it reports none, Docker's built-in pools (source defaults), always estimated. " +
			"used and capacity count blocks of the pool's mask; state is decided on the exact ratio (normal under 70%, getting_full from 70%, almost_full from 90%) and percent is that ratio rounded down. " +
			"largestFree is the largest aligned free block and largestFreeSubnets how many blocks it holds. " +
			"networks is the subnet map: every network of every reporting node (a swarm-wide network once, a local network once per node) with its subnets, owner, attachments, age and, for an overlay, its usage, at most 5000 rows with networksTotal and networksTruncated. " +
			"overlays is every swarm overlay network, ingress included, with its addresses used against what it can still hand out (read from the manager's address status, Engine 29 and later), the fullest first; a network whose count cannot be read has measured false and no numbers. " +
			"ingress repeats the ingress network with outsidePool (absent when the swarm lists none). " +
			"When no manager of a swarm answers, notMeasured is no_manager and the pools last read are returned with state unknown and nothing counted; notReporting names the nodes that did not answer. " +
			"The server reads the engines at most once in 5 seconds per environment; the answer is never cached by the browser. IPv4 only. " +
			"Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*AddressSpaceOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.AddressSpaces == nil || d.Aggregator == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			space, err := d.AddressSpaces.Read(ctx, env.ID)
			if err != nil {
				return nil, snapshotProblem(d.Log, env.ID, err)
			}
			body := addressSpaceBody(space)
			// Every row of the map comes from this one aggregate, so a network made during the
			// read is in the next answer whole.
			agg, _, err := d.Aggregator.Snapshot(ctx, env.ID)
			if err != nil {
				return nil, snapshotProblem(d.Log, env.ID, err)
			}
			use := networkUsageOf(ctx, d.Clusters, env.ID, agg)
			known := loadStackIDs(ctx, d.Store, env.ID, d.Log)
			body.Networks, body.NetworksTotal, body.NetworksTruncated = mapRows(agg.Networks, use, known, space.Overlays)
			body.NotReporting = unionNames(body.NotReporting, agg.NotReporting)
			body.Orphans, body.OrphansWithheld = orphanBody(ctx, d, env.ID)
			return &AddressSpaceOutput{CacheControl: "no-store", Body: body}, nil
		})
	registerAddressOrphans(r, d)
}

// orphanBody is the orphan part of the page. A check that fails leaves the list empty and says
// no manager answered, and never fails the page: the pools and the map are worth showing without it.
func orphanBody(ctx context.Context, d AddressSpaceDeps, envID string) ([]OrphanRowBody, *OrphansWithheldBody) {
	rep, err := d.AddressSpaces.Orphans(ctx, envID)
	if err != nil {
		d.Log.Warn("could not check for orphaned networks", "environment", envID, "error", err)
		return []OrphanRowBody{}, &OrphansWithheldBody{Nodes: []string{}, Stacks: []string{}, ManagerMissing: true}
	}
	rows := make([]OrphanRowBody, 0, len(rep.Rows))
	for _, r := range rep.Rows {
		src := "created"
		if r.IdleFromSeen {
			src = "seen"
		}
		rows = append(rows, OrphanRowBody{
			NetworkID: r.NetworkID, Name: r.Name, Node: r.Node, NodeID: r.NodeID, Scope: scopeName(r.Scope),
			Subnets: append([]string{}, r.Subnets...), Class: string(r.Class), FormerOwner: r.FormerOwner,
			FormerOwnerKind: r.FormerOwnerKind, IdleSince: r.IdleSince.UTC().Format(time.RFC3339), IdleSinceSource: src,
		})
	}
	if len(rep.WithheldNodes) == 0 && len(rep.WithheldStacks) == 0 && !rep.ManagerMissing {
		return rows, nil
	}
	return rows, &OrphansWithheldBody{
		Nodes: append([]string{}, rep.WithheldNodes...), Stacks: append([]string{}, rep.WithheldStacks...), ManagerMissing: rep.ManagerMissing,
	}
}

// scopeName is the wire name of a network scope: swarm for a cluster-wide network, local for every other.
func scopeName(scope string) string {
	if scope == dockerx.ScopeSwarm {
		return "swarm"
	}
	return "local"
}

// addressSpaceBody is the wire form of an environment's address space.
func addressSpaceBody(s envs.AddressSpace) AddressSpaceBody {
	body := AddressSpaceBody{
		EnvType: "standalone", Measured: s.Measured, NotMeasured: s.NotMeasured,
		Pools: make([]AddressPoolBody, 0, len(s.Pools)), Overlays: make([]OverlayUsageBody, 0, len(s.Overlays)),
		NotReporting: s.NotReporting,
	}
	for _, o := range s.Overlays {
		body.Overlays = append(body.Overlays, overlayBody(o))
	}
	if s.Ingress != nil {
		ing := overlayBody(*s.Ingress)
		body.Ingress = &ing
	}
	if s.Swarm {
		body.EnvType = "swarm"
	}
	if body.NotReporting == nil {
		body.NotReporting = []string{}
	}
	for _, p := range s.Pools {
		row := AddressPoolBody{
			CIDR: p.Pool.Base.String(), Mask: p.Pool.Size,
			Used: blocks(p.Used), Capacity: blocks(p.Capacity), Percent: p.Percent, State: string(p.State),
			LargestFreeSubnets: blocks(p.LargestFreeBlocks), Source: p.Source, Estimated: p.Estimated,
		}
		if p.LargestFree.IsValid() {
			row.LargestFree = p.LargestFree.String()
		}
		body.Pools = append(body.Pools, row)
	}
	return body
}

// maxBlocks is the most blocks a pool can hold: a /0 base cut into /30 blocks is 2^30. Every
// count of a valid pool is at most this, which is why it fits an int64 and a JSON number.
const maxBlocks = 1 << 30

// blocks is a count of blocks as the wire number, held to maxBlocks.
func blocks(n uint64) int64 { return int64(min(n, maxBlocks)) } //nolint:gosec // G115: held to 2^30 on this line

// mapRows is the rows of the subnet map for the networks of one aggregate, with the total.
func mapRows(nets []envs.NetworkRow, use networkUsage, known stackIDs, overlays []envs.OverlayUsage) (rows []AddressMapRowBody, total int, truncated bool) {
	usage := make(map[string]*NetworkUsageBody, len(overlays))
	for _, o := range overlays {
		if !o.Measured || o.Capacity == 0 {
			continue
		}
		usage[o.ID] = &NetworkUsageBody{Used: addresses(o.Used), Capacity: addresses(o.Capacity), Percent: o.Percent, State: string(o.State)}
	}
	all := make([]AddressMapRowBody, 0, len(nets))
	items := make([]netspace.MapItem, 0, len(nets))
	prefixes := make([][]netip.Prefix, 0, len(nets))
	names := make([]string, 0, len(nets))
	for _, n := range nets {
		all = append(all, mapRow(n, use, known, usage))
		var ps []netip.Prefix
		for _, raw := range n.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil {
				ps = append(ps, p)
			}
		}
		prefixes = append(prefixes, ps)
		items = append(items, netspace.MapItem{Swarm: n.Scope == dockerx.ScopeSwarm, Node: n.Node, Prefixes: ps})
		names = append(names, n.Name)
	}
	// Overlaps are decided over every row, before the cap, so a row near the end still names its partner.
	partners := netspace.MapOverlaps(items)
	for i := range all {
		for _, j := range partners[i] {
			subnet, _ := netspace.FirstOverlap(prefixes[i], prefixes[j])
			node := ""
			if !items[j].Swarm {
				node = nets[j].Node
			}
			all[i].Overlaps = append(all[i].Overlaps, MapOverlapBody{Name: nets[j].Name, Subnet: subnet.String(), Node: node})
		}
	}
	// Rows leave in address order, each knowing its position.
	pos := netspace.AddressOrder(items, names)
	ordered := make([]AddressMapRowBody, len(all))
	for i, at := range pos {
		all[i].Order = at
		ordered[at] = all[i]
	}
	all = ordered
	total = len(all)
	rows = all[:min(total, maxMapRows)]
	return rows, total, total > len(rows)
}

// mapRow is one network as the subnet map shows it. Only the owner label values and the ID of a
// Coaming stack that owns the network leave the server; no other label does (T-10-16).
func mapRow(n envs.NetworkRow, use networkUsage, known stackIDs, usage map[string]*NetworkUsageBody) AddressMapRowBody {
	scope := "local"
	if n.Scope == dockerx.ScopeSwarm {
		scope = "swarm"
	}
	containers, services := use.of(n)
	owner, kind := networkOwner(n.Labels)
	ownerStack := ""
	switch {
	case kind != "":
		ownerStack = known.confirm(owner, n.Labels[stacks.LabelStackID])
	case scope == "swarm" && use.firstService[n.ID] != "":
		owner, kind = use.firstService[n.ID], "service"
	default:
		kind = "none"
	}
	return AddressMapRowBody{
		ID: n.ID, Name: n.Name, Node: n.Node, NodeID: n.NodeID, Scope: scope, Driver: n.Driver,
		Subnets: append([]string{}, n.Subnets...), Owner: owner, OwnerKind: kind, OwnerStackID: ownerStack,
		AttachedContainers: containers, AttachedServices: services, Created: rfc3339(n.Created),
		BuiltIn: dockerx.IsBuiltinNetwork(n.Name, n.Ingress), Ingress: n.Ingress,
		Usage: usage[n.ID], Overlaps: []MapOverlapBody{},
	}
}

// unionNames is a followed by the names of b that a lacks. The map and the pools are read
// moments apart, so a node that stopped answering in between must still be named.
func unionNames(a, b []string) []string {
	out := append([]string{}, a...)
	for _, n := range b {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
