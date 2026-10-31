// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/bits"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/netspace"
)

// Where the pool of a row came from (D-07 as amended on 2026-10-30).
const (
	// PoolSourceSwarm is the pool a swarm manager reports (or the swarm's built-in
	// 10.0.0.0/8 when it reports none).
	PoolSourceSwarm = "swarm"
	// PoolSourceDaemon is a pool the host's daemon reports as its default-address-pools
	// setting.
	PoolSourceDaemon = "daemon"
	// PoolSourceDefaults is one of Docker's built-in local pools, used because the daemon
	// reports no setting of its own.
	PoolSourceDefaults = "defaults"
	// PoolSourceEntered is a pool a person entered as an override.
	PoolSourceEntered = "entered"
)

// NotMeasuredNoManager is the reason a swarm's pools could not be measured: no manager
// answered, so the pools and the cluster's networks are not known now.
const NotMeasuredNoManager = "no_manager"

// How many overlay networks one read inspects at once. Each is two inspects through a
// manager, so a swarm with hundreds of overlays cannot fan out without bound (T-10-08).
const overlayParallel = 8

// maxMeasuredOverlays is the most overlays one read measures. The rest are listed with no
// numbers (Measured false), so a huge swarm costs a bounded number of inspects (T-10-08).
const maxMeasuredOverlays = 512

// maxSubnetAddrs is the most addresses an IPv4 subnet can hold. A count above it is not an
// IPv4 count (an engine bug, or the saturating counter of an IPv6 subnet) and is not trusted
// (T-10-09).
const maxSubnetAddrs = 1 << 32

// AddressSpaceOptions configure AddressSpaces. A zero field takes its default.
type AddressSpaceOptions struct {
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// TTL is how long a read is reused; it defaults to DefaultClusterTTL.
	TTL time.Duration
	// Log receives what cannot be returned; it defaults to the service's.
	Log *slog.Logger
	// Interval is how often Run reads every environment; it defaults to DefaultAddressInterval.
	Interval time.Duration
	// Ticks, when set, replaces the interval's ticker: Run reads every environment once per
	// value received. Tests send on it, so they never wait for a clock.
	Ticks <-chan time.Time
	// Subscribe opens the events of an environment, as Aggregator.EventRows does, which is its
	// default.
	Subscribe func(ctx context.Context, envID string) ([]dockerx.EventRow, <-chan dockerx.EventRow, func())
	// After returns a channel that receives once d has passed; it defaults to time.After. It
	// times the debounce of an event and the pause before a subscription is reopened.
	After func(d time.Duration) <-chan time.Time
}

// PoolUsage is one address pool and what is in use of it. Source says where the pool came
// from. Estimated is true whenever the number cannot be taken as exact: every standalone
// row, whatever its source, because the daemon skips pool slots that overlap host routes
// and engines differ.
type PoolUsage struct {
	netspace.PoolStat
	Source    string
	Estimated bool
}

// OverlayUsage is one swarm overlay network and how many of its addresses are handed out. The
// numbers are the swarm's own (the leader's allocator, read through a manager); Measured is
// false when they could not be read, and then Used, Capacity and Percent are 0 and State is
// unknown, never a made-up zero.
type OverlayUsage struct {
	ID, Name string
	// Subnets are the network's IPv4 subnets as the aggregate lists them, in its order.
	Subnets []string
	// Used and Capacity are addresses. Capacity is what the allocator has handed out plus
	// what it can still hand out, over the IPv4 subnets, so an --ip-range counts only the
	// range.
	Used, Capacity uint64
	Percent        int
	State          netspace.State
	Measured       bool
	// Ingress marks the swarm's routing mesh network.
	Ingress bool
	// OutsidePool is true for the ingress network when none of its IPv4 subnets lies inside
	// a pool base of the swarm, so no pool row counts it. It is false for every other
	// network.
	OutsidePool bool
}

// AddressSpace is the address pools of one environment and how full each is.
type AddressSpace struct {
	EnvID string
	// Swarm is true for a swarm environment.
	Swarm bool
	// Measured is true when the numbers were read from the cluster's own pools: a swarm whose
	// manager answered. It is false for a standalone host, whose numbers are estimated, and
	// when NotMeasured is set.
	Measured bool
	// NotMeasured says why a swarm could not be measured, empty otherwise.
	NotMeasured string
	// Pools are in the order the swarm or the daemon lists them.
	Pools []PoolUsage
	// Overlays are the swarm's overlay networks, ingress included, the fullest first and then
	// by name. Empty for a standalone host.
	Overlays []OverlayUsage
	// Ingress is the ingress network again, nil when the swarm has none in the aggregate.
	Ingress *OverlayUsage
	// NotReporting names the nodes that did not answer, whose networks are therefore left out
	// of the counts.
	NotReporting []string
	FetchedAt    time.Time
}

// addressEntry is what AddressSpaces holds for one environment.
type addressEntry struct {
	// read makes one read of the environment at a time, so a burst of callers shares one.
	read sync.Mutex

	mu    sync.Mutex
	value AddressSpace
	have  bool
	// pools are the pools of the last read that measured them, for the answer given when
	// the next cannot.
	pools []PoolUsage

	// orphans is the report of the orphan check. oread makes one check of the environment at a
	// time, so a burst of callers shares one, and omu guards the report.
	oread       sync.Mutex
	omu         sync.Mutex
	orphans     OrphanReport
	orphansAt   time.Time
	haveOrphans bool

	// nets are the networks of the last aggregate a read used, for the checks of other
	// environments' subnets (guarded by mu); netsAt is when that read was made.
	nets     []netspace.MapItemNamed
	netsAt   time.Time
	haveNets bool

	// alloc serialises the check and the create of a network's subnet (LockAllocation).
	alloc sync.Mutex

	// sum is the summary of the environment's address space (guarded by mu); haveSum is false
	// until a read or an orphan check has made one.
	sum     AddressSummary
	haveSum bool
}

// AddressSpaces reads how full the address pools of an environment are. Docker is the source of
// truth and nothing here is persisted: a read is kept for a short time so that a refresh storm
// costs one read of the engines. It is safe for concurrent use.
type AddressSpaces struct {
	svc      *Service
	agg      *Aggregator
	clusters *Clusters
	now      func() time.Time
	ttl      time.Duration
	log      *slog.Logger

	// overlayReadHook, when set, is called as the read of one overlay starts and the function it
	// returns as that read ends. Tests count the reads in flight with it, never by timing;
	// nothing sets it in production.
	overlayReadHook func() (done func())

	// facts are what Coaming's own stacks say about a network (WithStackFacts); nil until set.
	facts StackFacts

	mu      sync.Mutex
	entries map[string]*addressEntry

	interval  time.Duration
	ticks     <-chan time.Time
	subscribe func(ctx context.Context, envID string) ([]dockerx.EventRow, <-chan dockerx.EventRow, func())
	after     func(d time.Duration) <-chan time.Time

	// The background reader's state: the context Run was given (nil until it runs), the
	// environments with a read waiting out the debounce, and the event watchers by environment.
	bmu     sync.Mutex
	bg      context.Context
	pending map[string]bool
	watches map[string]context.CancelFunc
	wg      sync.WaitGroup

	// readHook, when set, is called as each background read of an environment starts and the
	// function it returns as that read ends, and passHook as each pass over every environment
	// ends. Tests count and wait on them instead of on a clock; nothing sets them in production.
	readHook func(envID string) (done func())
	passHook func()
}

// NewAddressSpaces returns the address reader of svc. agg gives every node's networks and
// clusters reads the swarm's pools through a manager. svc is what later readers of the phase
// reach the store through.
func NewAddressSpaces(svc *Service, agg *Aggregator, clusters *Clusters, o AddressSpaceOptions) *AddressSpaces {
	s := &AddressSpaces{
		svc: svc, agg: agg, clusters: clusters, now: o.Now, ttl: o.TTL, log: o.Log, entries: map[string]*addressEntry{},
		interval: o.Interval, ticks: o.Ticks, subscribe: o.Subscribe, after: o.After,
		pending: map[string]bool{}, watches: map[string]context.CancelFunc{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.ttl <= 0 {
		s.ttl = DefaultClusterTTL
	}
	if s.log == nil && svc != nil {
		s.log = svc.log
	}
	if s.interval <= 0 {
		s.interval = DefaultAddressInterval
	}
	if s.subscribe == nil && agg != nil {
		s.subscribe = agg.EventRows
	}
	if s.after == nil {
		s.after = time.After
	}
	return s
}

func (s *AddressSpaces) entry(envID string) *addressEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[envID]
	if e == nil {
		e = &addressEntry{}
		s.entries[envID] = e
	}
	return e
}

// LockAllocation takes the environment's allocation lock and returns the function that gives it
// back. A create that checks a subnet against the environment's networks and then makes the
// network holds it across both, so two creates through Coaming cannot both pass the check with
// the same subnet (NET-05). It does not stop a network made outside Coaming.
func (s *AddressSpaces) LockAllocation(envID string) (unlock func()) {
	e := s.entry(envID)
	e.alloc.Lock()
	return e.alloc.Unlock
}

// CachedNetworks are the networks of the environment as the last Read saw them, and when that
// was. It is false before any read. It never reads an engine, so a check of another
// environment's networks costs nothing there.
func (s *AddressSpaces) CachedNetworks(envID string) ([]netspace.MapItemNamed, time.Time, bool) {
	s.mu.Lock()
	e := s.entries[envID]
	s.mu.Unlock()
	if e == nil {
		return nil, time.Time{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.haveNets {
		return nil, time.Time{}, false
	}
	return slices.Clone(e.nets), e.netsAt, true
}

// Environments are the IDs of the environments with cached networks, sorted.
func (s *AddressSpaces) Environments() []string {
	s.mu.Lock()
	entries := make(map[string]*addressEntry, len(s.entries))
	for id, e := range s.entries {
		entries[id] = e
	}
	s.mu.Unlock()
	var ids []string
	for id, e := range entries {
		e.mu.Lock()
		if e.haveNets {
			ids = append(ids, id)
		}
		e.mu.Unlock()
	}
	slices.Sort(ids)
	return ids
}

// NetworkItems are the networks of an aggregate as items of the subnet scope rule: a swarm-wide
// network once whatever number of managers listed it, a local network once per node, each with
// its IPv4 subnets. A subnet that cannot be parsed is skipped.
func NetworkItems(nets []NetworkRow) []netspace.MapItemNamed {
	seen := map[string]bool{}
	out := make([]netspace.MapItemNamed, 0, len(nets))
	for _, n := range nets {
		swarm := n.Scope == dockerx.ScopeSwarm
		if swarm {
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
		}
		var ps []netip.Prefix
		for _, raw := range n.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() {
				ps = append(ps, p.Masked())
			}
		}
		if len(ps) == 0 {
			continue
		}
		out = append(out, netspace.MapItemNamed{MapItem: netspace.MapItem{Swarm: swarm, Node: n.Node, Prefixes: ps}, Name: n.Name})
	}
	return out
}

// Read is the address space of the environment. A read younger than the TTL is reused. An
// environment that cannot be read at all is the error of the read, as the aggregator gives it
// (ErrNotFound, ErrNotOnline, a refusal by an agent's policy); a swarm whose managers do not
// answer is not an error but an answer that says so.
func (s *AddressSpaces) Read(ctx context.Context, envID string) (AddressSpace, error) {
	e := s.entry(envID)
	e.read.Lock()
	defer e.read.Unlock()

	now := s.now()
	e.mu.Lock()
	if e.have && now.Sub(e.value.FetchedAt) < s.ttl && now.Sub(e.value.FetchedAt) >= 0 {
		v := cloneSpace(e.value)
		e.mu.Unlock()
		return v, nil
	}
	e.mu.Unlock()

	v, err := s.fetch(ctx, e, envID)
	if err != nil {
		return AddressSpace{}, err
	}
	v.FetchedAt = now
	e.mu.Lock()
	e.value, e.have = v, true
	if v.NotMeasured == "" {
		e.pools = slices.Clone(v.Pools)
	}
	wasFull := e.haveSum && e.sum.Full
	e.sum, e.haveSum = summarise(e.sum, v, now), true
	changed := e.sum.Full != wasFull
	e.mu.Unlock()
	if changed {
		// Whether the environment is degraded has changed: tell whoever shows health.
		s.clusters.notifyHealth(envID)
	}
	return cloneSpace(v), nil
}

func cloneSpace(v AddressSpace) AddressSpace {
	v.Pools = slices.Clone(v.Pools)
	v.NotReporting = slices.Clone(v.NotReporting)
	v.Overlays = slices.Clone(v.Overlays)
	for i := range v.Overlays {
		v.Overlays[i].Subnets = slices.Clone(v.Overlays[i].Subnets)
	}
	if v.Ingress != nil {
		ing := *v.Ingress
		ing.Subnets = slices.Clone(ing.Subnets)
		v.Ingress = &ing
	}
	return v
}

// fetch reads the engines.
func (s *AddressSpaces) fetch(ctx context.Context, e *addressEntry, envID string) (AddressSpace, error) {
	swarm, err := s.clusters.IsSwarm(ctx, envID)
	if err != nil {
		return AddressSpace{}, err
	}
	agg, _, err := s.agg.Snapshot(ctx, envID)
	if err != nil {
		return AddressSpace{}, err
	}
	e.mu.Lock()
	e.nets, e.netsAt, e.haveNets = NetworkItems(agg.Networks), s.now(), true
	e.mu.Unlock()
	out := AddressSpace{EnvID: envID, Swarm: swarm, Pools: []PoolUsage{}, Overlays: []OverlayUsage{}, NotReporting: slices.Clone(agg.NotReporting)}
	if out.NotReporting == nil {
		out.NotReporting = []string{}
	}
	if swarm {
		return s.fetchSwarm(ctx, e, agg, out)
	}
	return s.fetchStandalone(ctx, agg, out)
}

// fetchSwarm reads the swarm's pools through a manager and counts every network of the cluster
// and every node's local networks against them. Without a manager's answer the pools last read
// are returned with nothing counted.
func (s *AddressSpaces) fetchSwarm(ctx context.Context, e *addressEntry, agg Aggregate, out AddressSpace) (AddressSpace, error) {
	var pools dockerx.AddressPools
	err := s.clusters.ClusterRead(ctx, out.EnvID, func(c *dockerx.Client) (err error) {
		pools, err = c.AddressPools(ctx)
		return err
	})
	// Without the cluster's counts no manager listed the swarm's networks, so a count of the
	// pool would leave them out.
	if err == nil && (!pools.SwarmKnown || agg.Counts.Nodes == nil) {
		err = ErrNoManager
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return AddressSpace{}, cerr
		}
		if denied := ClassifyDockerError(err); isPolicyDenied(denied) {
			return AddressSpace{}, denied
		}
		s.log.Debug("a swarm's address pools could not be read", "environment", out.EnvID, "error", err)
		out.NotMeasured = NotMeasuredNoManager
		e.mu.Lock()
		for _, p := range e.pools {
			out.Pools = append(out.Pools, unmeasured(p))
		}
		e.mu.Unlock()
		out.Overlays, out.Ingress = rankOverlays(overlayList(agg.Networks))
		return out, nil
	}

	size := pools.SwarmSubnetSize
	if size == 0 {
		size = netspace.DefaultSwarmSubnetSize
	}
	bases := pools.SwarmPools
	if len(bases) == 0 {
		bases = []netip.Prefix{netspace.DefaultSwarmPool}
	}
	subnets := networkSubnets(agg.Networks, func(NetworkRow) bool { return true })
	out.Measured = true
	for _, b := range bases {
		p := netspace.Pool{Base: b, Size: size}
		if !p.Valid() {
			s.log.Warn("a swarm reported an address pool that cannot be counted", "environment", out.EnvID, "pool", b, "size", size)
			continue
		}
		out.Pools = append(out.Pools, PoolUsage{PoolStat: netspace.Stat(p, subnets), Source: PoolSourceSwarm})
	}
	list := overlayList(agg.Networks)
	if err := s.measureOverlays(ctx, out.EnvID, list); err != nil {
		return AddressSpace{}, err
	}
	markOutsidePool(list, out.Pools)
	out.Overlays, out.Ingress = rankOverlays(list)
	return out, nil
}

// overlayList is the swarm-scoped overlay networks of the aggregate, ingress included, one
// entry per network ID, none measured, in the order of their names.
func overlayList(nets []NetworkRow) []OverlayUsage {
	seen := map[string]bool{}
	out := []OverlayUsage{}
	for _, n := range nets {
		if n.Scope != dockerx.ScopeSwarm || n.Driver != "overlay" || seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		o := OverlayUsage{ID: n.ID, Name: n.Name, Ingress: n.Ingress, State: netspace.StateUnknown, Subnets: []string{}}
		for _, raw := range n.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() {
				o.Subnets = append(o.Subnets, p.Masked().String())
			}
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b OverlayUsage) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// measureOverlays reads the use of the listed overlays in place through one manager. A manager
// that cannot be reached leaves every overlay unmeasured; only the caller's cancellation and an
// agent's policy refusal are errors, as for the pools.
func (s *AddressSpaces) measureOverlays(ctx context.Context, envID string, list []OverlayUsage) error {
	err := s.clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var g errgroup.Group
		g.SetLimit(overlayParallel)
		for i := range min(len(list), maxMeasuredOverlays) {
			g.Go(func() error {
				s.measureOverlay(ctx, c, &list[i], envID)
				return nil
			})
		}
		return g.Wait()
	})
	if err == nil {
		return nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if denied := ClassifyDockerError(err); isPolicyDenied(denied) {
		return denied
	}
	s.log.Debug("a swarm's overlay networks could not be measured", "environment", envID, "error", err)
	return nil
}

// measureOverlay reads one overlay by its full ID, the swarm's count and not the manager's own
// instance of it. The addresses of its IPv4 subnets are summed: IPv6 subnets report saturating
// counters and are skipped. One that cannot be read, has no swarm count (an engine before 29,
// or a manager that cannot ask the leader) or reports a count no IPv4 subnet could hold is left
// unmeasured; it never fails the read of the others.
func (s *AddressSpaces) measureOverlay(ctx context.Context, c *dockerx.Client, o *OverlayUsage, envID string) {
	if s.overlayReadHook != nil {
		defer s.overlayReadHook()()
	}
	d, err := c.NetworkSwarmUsage(ctx, o.ID)
	if err != nil {
		s.log.Debug("an overlay network could not be measured", "environment", envID, "network", o.Name, "error", err)
		return
	}
	var used, avail uint64
	for _, u := range d.Usage {
		p, perr := netip.ParsePrefix(u.Subnet)
		if perr != nil || !p.Addr().Is4() {
			continue
		}
		if u.InUse > maxSubnetAddrs || u.Available > maxSubnetAddrs {
			return
		}
		used += u.InUse
		avail += u.Available
	}
	if capacity := used + avail; capacity > 0 {
		o.Used, o.Capacity, o.Measured = used, capacity, true
		o.Percent, o.State = netspace.PercentOf(used, capacity), netspace.StateOf(used, capacity)
	}
}

// markOutsidePool sets OutsidePool on the ingress network: true when none of its IPv4 subnets
// lies inside the base of a pool, so no pool row counts it. Every other network keeps false.
func markOutsidePool(list []OverlayUsage, pools []PoolUsage) {
	for i := range list {
		o := &list[i]
		if !o.Ingress || len(o.Subnets) == 0 {
			continue
		}
		inside := false
		for _, raw := range o.Subnets {
			sub, err := netip.ParsePrefix(raw)
			if err != nil {
				continue
			}
			for _, p := range pools {
				if base := p.Pool.Base.Masked(); base.Contains(sub.Addr()) && sub.Bits() >= base.Bits() {
					inside = true
				}
			}
		}
		o.OutsidePool = !inside
	}
}

// rankOverlays orders the overlays fullest first and then by name, and returns the ingress
// network again.
func rankOverlays(list []OverlayUsage) ([]OverlayUsage, *OverlayUsage) {
	slices.SortStableFunc(list, func(a, b OverlayUsage) int {
		return cmp.Or(cmp.Compare(b.Percent, a.Percent), strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	for _, o := range list {
		if o.Ingress {
			ing := o
			return list, &ing
		}
	}
	return list, nil
}

// isPolicyDenied reports whether err is an agent's refusal, which is a fault of Coaming's own
// policy and not a manager that is missing.
func isPolicyDenied(err error) bool {
	var d *AgentPolicyDeniedError
	return errors.As(err, &d)
}

// unmeasured is a remembered pool with nothing counted and an unknown state.
func unmeasured(p PoolUsage) PoolUsage {
	p.Used, p.Percent, p.State = 0, 0, netspace.StateUnknown
	p.LargestFree, p.LargestFreeBlocks = netip.Prefix{}, 0
	return p
}

// fetchStandalone reads the pools of a host from its own daemon, or takes Docker's defaults,
// and counts the host's local networks against them. Every row is an estimate.
func (s *AddressSpaces) fetchStandalone(ctx context.Context, agg Aggregate, out AddressSpace) (AddressSpace, error) {
	if len(agg.Nodes) == 0 {
		return AddressSpace{}, ErrNodeUnavailable
	}
	node := agg.Nodes[0]
	var pools dockerx.AddressPools
	err := s.agg.OnNode(ctx, out.EnvID, node.ID, func(c *dockerx.Client) (err error) {
		pools, err = c.AddressPools(ctx)
		return err
	})
	if err != nil {
		return AddressSpace{}, err
	}

	source := PoolSourceDaemon
	var defs []netspace.Pool
	for _, p := range pools.DaemonPools {
		defs = append(defs, netspace.Pool{Base: p.Base, Size: p.Size})
	}
	if len(defs) == 0 {
		source, defs = PoolSourceDefaults, netspace.DockerDefaultPools
	}
	// Only the host's own networks take its local pools: an overlay is the swarm's.
	subnets := networkSubnets(agg.Networks, func(n NetworkRow) bool { return n.Scope != dockerx.ScopeSwarm })
	for _, p := range defs {
		if !p.Valid() {
			s.log.Warn("a daemon reported an address pool that cannot be counted", "environment", out.EnvID, "pool", p.Base, "size", p.Size)
			continue
		}
		out.Pools = append(out.Pools, PoolUsage{PoolStat: netspace.Stat(p, subnets), Source: source, Estimated: true})
	}
	return out, nil
}

// networkSubnets are the distinct IPv4 subnets of the networks for which keep is true, in the
// order first met. A subnet that cannot be parsed is skipped.
func networkSubnets(nets []NetworkRow, keep func(NetworkRow) bool) []netip.Prefix {
	seen := map[netip.Prefix]bool{}
	var out []netip.Prefix
	for _, n := range nets {
		if !keep(n) {
			continue
		}
		for _, raw := range n.Subnets {
			p, err := netip.ParsePrefix(raw)
			if err != nil || !p.Addr().Is4() {
				continue
			}
			p = p.Masked()
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// The meters a summary names (WorstMeter.Kind) and what each counts (WorstMeter.Unit).
const (
	MeterPool    = "pool"
	MeterIngress = "ingress"
	MeterOverlay = "overlay"

	UnitSubnets   = "subnets"
	UnitAddresses = "addresses"
)

// The percentages at which a swarm becomes, and stops being, address_space_full (user decision 3,
// 2026-10-30). Leaving takes a lower number than entering, so a network that hovers around 90%
// does not flip the environment between degraded and online on every read.
const (
	fullEnter = 90
	fullLeave = 85
)

// DefaultAddressInterval is how often Run reads every enabled environment's address space and
// orphan list when nobody asks (D-09).
const DefaultAddressInterval = 60 * time.Second

// WorstMeter is the fullest pool or network of an environment, by the exact ratio of what is used
// to what there is. Estimated is true for a standalone host's pool, whose number is a guess.
type WorstMeter struct {
	// Kind is MeterPool, MeterIngress or MeterOverlay.
	Kind string
	// Name is the pool's CIDR or the network's name.
	Name           string
	Used, Capacity uint64
	Percent        int
	State          netspace.State
	// Unit is UnitSubnets for a pool and UnitAddresses for a network.
	Unit      string
	Estimated bool
}

// AddressSummary is what the overview, the dashboard and the health of an environment need of its
// address space, from the last read: the worst meter, how many networks are orphaned and whether
// the environment counts as out of address space.
type AddressSummary struct {
	// Worst is nil when nothing could be measured.
	Worst *WorstMeter
	// Orphaned counts the networks the last orphan check called orphaned.
	Orphaned int
	// Full is true from the first measured swarm meter at fullEnter percent until every one is
	// below fullLeave. An estimated pool never sets it. FullName and FullPercent name the meter
	// that does, and FullSince is when Full turned on.
	Full        bool
	FullName    string
	FullPercent int
	FullSince   time.Time
	// Measured is true when Worst is set.
	Measured bool
	// At is when the summary was last updated.
	At time.Time
}

func (a AddressSummary) clone() AddressSummary {
	if a.Worst != nil {
		w := *a.Worst
		a.Worst = &w
	}
	return a
}

// metersOf are the meters of a read that have a number: the pools, then the ingress network, then
// the other overlays in the order of the read. Ties go to the earlier one.
func metersOf(v AddressSpace) []WorstMeter {
	var out []WorstMeter
	for _, p := range v.Pools {
		if p.State == netspace.StateUnknown || p.Capacity == 0 {
			continue
		}
		out = append(out, WorstMeter{
			Kind: MeterPool, Name: p.Pool.Base.String(), Used: p.Used, Capacity: p.Capacity, Percent: p.Percent,
			State: p.State, Unit: UnitSubnets, Estimated: p.Estimated,
		})
	}
	for _, ingress := range []bool{true, false} {
		for _, o := range v.Overlays {
			if o.Ingress != ingress || !o.Measured || o.Capacity == 0 {
				continue
			}
			kind := MeterOverlay
			if o.Ingress {
				kind = MeterIngress
			}
			out = append(out, WorstMeter{
				Kind: kind, Name: o.Name, Used: o.Used, Capacity: o.Capacity, Percent: o.Percent,
				State: o.State, Unit: UnitAddresses,
			})
		}
	}
	return out
}

// fuller reports whether a is strictly fuller than b, by the exact ratio (the cross products are
// compared in 128 bits, so no count can overflow it).
func fuller(a, b WorstMeter) bool {
	ahi, alo := bits.Mul64(a.Used, b.Capacity)
	bhi, blo := bits.Mul64(b.Used, a.Capacity)
	return ahi > bhi || ahi == bhi && alo > blo
}

// atLeast reports whether the meter is at pct percent or more, by the exact ratio.
func (m WorstMeter) atLeast(pct uint64) bool { return m.Used*100 >= m.Capacity*pct }

// summarise works out the summary of an environment from a fresh read and the summary before it.
// This is where the hysteresis lives (RESEARCH Pitfall 7); the rollup only reads the answer.
//
// Only measured meters of a swarm decide Full: an estimated pool (a standalone host) never does
// (D-07). Once Full, it stays on while any such meter is at fullLeave percent or more, and while
// the meter that turned it on cannot be measured now (the manager is not answering); a read that
// measures nothing, or finds that meter gone, is not a reason to turn it off by itself, but a
// measurement below fullLeave always is.
func summarise(prev AddressSummary, v AddressSpace, now time.Time) AddressSummary {
	ms := metersOf(v)
	sum := AddressSummary{Orphaned: prev.Orphaned, At: now, Measured: len(ms) > 0}
	var worst, decisive *WorstMeter
	for i := range ms {
		m := &ms[i]
		if worst == nil || fuller(*m, *worst) {
			worst = m
		}
		if v.Swarm && !m.Estimated && (decisive == nil || fuller(*m, *decisive)) {
			decisive = m
		}
	}
	if worst != nil {
		w := *worst
		sum.Worst = &w
	}
	switch {
	case decisive != nil && decisive.atLeast(fullEnter):
		sum.Full, sum.FullName, sum.FullPercent = true, decisive.Name, decisive.Percent
	case prev.Full && decisive != nil && decisive.atLeast(fullLeave):
		sum.Full, sum.FullName, sum.FullPercent = true, decisive.Name, decisive.Percent
	case prev.Full && cannotMeasure(prev.FullName, v):
		sum.Full, sum.FullName, sum.FullPercent = true, prev.FullName, prev.FullPercent
	}
	if sum.Full {
		sum.FullSince = now
		if prev.Full && !prev.FullSince.IsZero() {
			sum.FullSince = prev.FullSince
		}
	}
	return sum
}

// cannotMeasure is true when the read says nothing about the named pool or network: nothing was
// measured at all (no manager answered), or the network is listed without numbers.
func cannotMeasure(name string, v AddressSpace) bool {
	if v.NotMeasured != "" {
		return true
	}
	for _, o := range v.Overlays {
		if o.Name == name && !o.Measured {
			return true
		}
	}
	for _, p := range v.Pools {
		if p.Pool.Base.String() == name && (p.State == netspace.StateUnknown || p.Capacity == 0) {
			return true
		}
	}
	return false
}

// Summary is the summary of the environment's address space from the last read, and false before
// any read or orphan check has been made. It never reads an engine.
func (s *AddressSpaces) Summary(envID string) (AddressSummary, bool) {
	s.mu.Lock()
	e := s.entries[envID]
	s.mu.Unlock()
	if e == nil {
		return AddressSummary{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.haveSum {
		return AddressSummary{}, false
	}
	return e.sum.clone(), true
}

// setOrphaned records the count of orphaned networks the last check found.
func (s *AddressSpaces) setOrphaned(envID string, n int) {
	e := s.entry(envID)
	e.mu.Lock()
	e.sum.Orphaned, e.haveSum = n, true
	e.mu.Unlock()
}

// enteredPool is the stored form of a pool a person entered.
type enteredPool struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

// EncodeEnteredPools is the text stored for the pools a person entered (environments.address_pools),
// a JSON list in the order given. No pools is "[]".
func EncodeEnteredPools(pools []netspace.Pool) string {
	list := make([]enteredPool, 0, len(pools))
	for _, p := range pools {
		list = append(list, enteredPool{Base: p.Base.String(), Size: p.Size})
	}
	raw, err := json.Marshal(list)
	if err != nil { // a list of strings and numbers cannot fail to marshal
		return "[]"
	}
	return string(raw)
}

// DecodeEnteredPools reads what EncodeEnteredPools stored. A pool that cannot be counted is
// dropped rather than failing the read: the text is Coaming's own, but a damaged row must not
// take the page down.
func DecodeEnteredPools(raw string) []netspace.Pool {
	var list []enteredPool
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil
	}
	out := make([]netspace.Pool, 0, len(list))
	for _, e := range list {
		base, err := netip.ParsePrefix(e.Base)
		if err != nil {
			continue
		}
		if p := (netspace.Pool{Base: base, Size: e.Size}); p.Valid() {
			out = append(out, p)
		}
	}
	return out
}
