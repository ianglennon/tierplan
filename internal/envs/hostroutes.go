// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/coaming/proto/hostroute"
	"github.com/ianglennon/coaming/proto/wire"
)

// HostNetSource reads the host routes of one connected agent through the tunnel.
// tunnel.Hub is one. The document it returns is untrusted.
type HostNetSource interface {
	HostNet(ctx context.Context, agentID string) (wire.HostNetDoc, error)
}

// What the route check knows about one node (D-17 as amended).
const (
	// RouteCheckOK means the node's routes were read and are in NodeRoutes.Routes.
	RouteCheckOK = "ok"
	// RouteCheckOldAgent means the agent reported nothing about host routes: it is
	// older than Phase 10 and cannot be asked.
	RouteCheckOldAgent = "old_agent"
	// RouteCheckUnmounted means the agent is current but cannot read its host's
	// route file, because it is not mounted into the agent's container.
	RouteCheckUnmounted = "unmounted"
	// RouteCheckOffline means the node's agent is not connected.
	RouteCheckOffline = "offline"
	// RouteCheckFailed means the read errored or the answer was not a valid list of
	// routes.
	RouteCheckFailed = "failed"
)

const (
	// maxHostRouteReaders is how many agents one Get reads at once.
	maxHostRouteReaders = 4
	// defaultHostRoutesTTL is how long an agent's routes are kept: a subnet picker
	// that checks every 400 ms opens at most one stream per agent in that time.
	defaultHostRoutesTTL = 5 * time.Minute
	// hostRoutesErrorTTL is how long a read that errored is kept, so an agent that
	// is struggling is not asked again at every keystroke but is tried again soon.
	hostRoutesErrorTTL = 30 * time.Second
	// hostRoutesReadTimeout bounds one read of an agent, whatever the caller does.
	hostRoutesReadTimeout = 8 * time.Second
	// maxHostRoutes is the most routes a document may hold, as the agent keeps at
	// most hostroute.MaxRoutes.
	maxHostRoutes = hostroute.MaxRoutes
	// hostNetDocVersion is the only document version this server reads.
	hostNetDocVersion = 1
)

// NodeRoutes is what the route check knows about one node of an environment. Routes
// is set only when State is RouteCheckOK.
type NodeRoutes struct {
	// Node is the node's name and NodeID its swarm node ID, empty for a standalone
	// host.
	Node, NodeID string
	State        string
	// Local is true for the host Coaming itself runs on, read from the route file
	// mounted into Coaming's own container; the fix for State unmounted is then
	// worded for that container and not for an agent.
	Local bool
	// Routes are the node's routes, masked and sorted by address.
	Routes []hostroute.Route
	// Truncated is true when the node has more routes than the agent sends.
	Truncated bool
}

// HostRoutesOptions configure HostRoutes. The zero value is usable.
type HostRoutesOptions struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// TTL is how long an agent's routes are kept; zero means 5 minutes.
	TTL time.Duration
	// LocalRouteFile is the host's route file as mounted into Coaming's own
	// container, for an environment on Coaming's own host. Empty means none.
	LocalRouteFile string
	// Log receives what an agent sent that was refused; nil discards it.
	Log *slog.Logger
}

// HostRoutes answers, for each node of an environment, either its host routes or
// exactly why there are none. Everything an agent sends is untrusted and checked
// again here.
type HostRoutes struct {
	svc   *Service
	src   HostNetSource
	now   func() time.Time
	ttl   time.Duration
	local string
	log   *slog.Logger

	// mu guards cache and flights. cache is the last answer per agent ID and flights
	// the reads under way, by agent ID and session start.
	mu      sync.Mutex
	cache   map[string]routesEntry
	flights map[string]*routesFlight
	// onJoin, when set, is called each time a Get finds a read already under way and
	// waits for it, so a test knows when the second caller has joined.
	onJoin func()
}

// routesEntry is what one read of an agent found. It is valid for the session that
// answered it, until expires.
type routesEntry struct {
	connectedAt time.Time
	expires     time.Time
	routes      []hostroute.Route
	truncated   bool
	failed      bool
}

// routesFlight is a read under way; entry is set before done is closed.
type routesFlight struct {
	done  chan struct{}
	entry routesEntry
}

// apply puts what the read found into n.
func (e routesEntry) apply(n NodeRoutes) NodeRoutes {
	if e.failed {
		n.State = RouteCheckFailed
		return n
	}
	n.State, n.Routes, n.Truncated = RouteCheckOK, slices.Clone(e.routes), e.truncated
	return n
}

// NewHostRoutes returns the route check over the registry's agents and src.
func NewHostRoutes(svc *Service, src HostNetSource, o HostRoutesOptions) *HostRoutes {
	h := &HostRoutes{
		svc: svc, src: src, now: o.Now, ttl: o.TTL, local: o.LocalRouteFile, log: o.Log,
		cache: map[string]routesEntry{}, flights: map[string]*routesFlight{},
	}
	if h.ttl <= 0 {
		h.ttl = defaultHostRoutesTTL
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h
}

// Get returns one NodeRoutes per node of the environment, ordered by node name.
// ErrNotFound for an unknown environment.
func (h *HostRoutes) Get(ctx context.Context, envID string) ([]NodeRoutes, error) {
	in, err := h.svc.Input(ctx, envID, h.now())
	if err != nil {
		return nil, err
	}
	var out []NodeRoutes
	if in.Kind == KindLocal {
		node := cleanText(in.Local.Probe.Hostname, maxNodeLen)
		if node == "" {
			node = "local"
		}
		return []NodeRoutes{h.readLocal(node, "")}, nil
	}
	var asked []int // positions in out of the nodes an agent must answer
	var views []AgentView
	for _, a := range in.Agents {
		if a.Revoked || (!a.Connected && !a.Socket) {
			// A session that ended takes what it answered with it.
			h.forget(a.ID)
		}
		if a.Revoked {
			continue
		}
		n := NodeRoutes{Node: a.Node, NodeID: a.SwarmNodeID}
		switch {
		case a.Socket:
			n = h.readLocal(a.Node, a.SwarmNodeID)
		case !a.Connected:
			n.State = RouteCheckOffline
		case a.HostNet == wire.HostNetUnmounted:
			n.State = RouteCheckUnmounted
		case a.HostNet != wire.HostNetOK:
			n.State = RouteCheckOldAgent
		default:
			asked, views = append(asked, len(out)), append(views, a)
		}
		out = append(out, n)
	}
	var g errgroup.Group
	g.SetLimit(maxHostRouteReaders)
	for i, a := range views {
		pos := asked[i]
		g.Go(func() error {
			out[pos] = h.readAgent(ctx, a, out[pos])
			return nil
		})
	}
	_ = g.Wait()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

// forget drops what is kept of an agent.
func (h *HostRoutes) forget(agentID string) {
	h.mu.Lock()
	delete(h.cache, agentID)
	h.mu.Unlock()
}

// readAgent answers for one connected agent that said it can: from what a read of
// this session found within the TTL, from a read already under way, or from a new
// read. One read per agent is under way at a time, and it is not cancelled when the
// Get that started it is, because others may wait on it.
func (h *HostRoutes) readAgent(ctx context.Context, a AgentView, n NodeRoutes) NodeRoutes {
	now := h.now()
	flightKey := fmt.Sprintf("%s@%d", a.ID, a.ConnectedAt.UnixNano())
	h.mu.Lock()
	if e, ok := h.cache[a.ID]; ok && e.connectedAt.Equal(a.ConnectedAt) && now.Before(e.expires) {
		h.mu.Unlock()
		return e.apply(n)
	}
	f, joined := h.flights[flightKey]
	if !joined {
		f = &routesFlight{done: make(chan struct{})}
		h.flights[flightKey] = f
		go h.read(ctx, a, flightKey, f)
	}
	onJoin := h.onJoin
	h.mu.Unlock()
	if joined && onJoin != nil {
		onJoin()
	}
	select {
	case <-f.done:
		return f.entry.apply(n)
	case <-ctx.Done():
		n.State = RouteCheckFailed
		return n
	}
}

// read asks the agent and records the answer.
func (h *HostRoutes) read(ctx context.Context, a AgentView, flightKey string, f *routesFlight) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hostRoutesReadTimeout)
	defer cancel()
	e := routesEntry{connectedAt: a.ConnectedAt}
	doc, err := h.src.HostNet(ctx, a.ID)
	switch {
	case err != nil:
		h.log.Debug("could not read the host routes of an agent", "agent_id", a.ID, "error", err)
		e.failed = true
		e.expires = h.now().Add(min(h.ttl, hostRoutesErrorTTL))
	default:
		routes, ok := validate(doc)
		if !ok {
			h.log.Warn("an agent sent host routes that are not valid; none were used", "agent_id", a.ID)
		}
		e.failed, e.routes, e.truncated = !ok, routes, ok && doc.Truncated
		e.expires = h.now().Add(h.ttl)
	}
	h.mu.Lock()
	h.cache[a.ID] = e
	delete(h.flights, flightKey)
	f.entry = e
	h.mu.Unlock()
	close(f.done)
}

// validate turns an agent's document into routes, sorted by address. Nothing the
// agent says is trusted: the document must be version 1 with no error and at most
// 512 routes, and every route a masked IPv4 prefix of 1 to 31 bits, a gateway that
// is an IPv4 address or empty, and an interface name of the safe bytes and at most
// 15. It refuses the whole document on any bad entry, so a hostile agent cannot
// smuggle a partial list.
func validate(doc wire.HostNetDoc) ([]hostroute.Route, bool) {
	if doc.V != hostNetDocVersion || doc.Error != "" || len(doc.Routes) > maxHostRoutes {
		return nil, false
	}
	routes := make([]hostroute.Route, 0, len(doc.Routes))
	for _, r := range doc.Routes {
		dst, err := netip.ParsePrefix(r.Dst)
		if err != nil || !dst.Addr().Is4() || dst.Bits() < 1 || dst.Bits() > 31 || dst != dst.Masked() {
			return nil, false
		}
		if len(r.Iface) > hostroute.MaxIfaceLen || hostroute.SanitizeIface(r.Iface) != r.Iface {
			return nil, false
		}
		rt := hostroute.Route{Iface: r.Iface, Dst: dst}
		if r.Gateway != "" {
			gw, err := netip.ParseAddr(r.Gateway)
			if err != nil || !gw.Is4() {
				return nil, false
			}
			rt.Gateway = gw
		}
		routes = append(routes, rt)
	}
	slices.SortStableFunc(routes, func(a, b hostroute.Route) int {
		return cmp.Or(a.Dst.Addr().Compare(b.Dst.Addr()), cmp.Compare(a.Dst.Bits(), b.Dst.Bits()), cmp.Compare(a.Iface, b.Iface))
	})
	return routes, true
}

// readLocal answers for the host Coaming itself runs on from the route file mounted
// into Coaming's own container. Anything but a readable table is "unmounted".
func (h *HostRoutes) readLocal(node, nodeID string) NodeRoutes {
	return NodeRoutes{Node: node, NodeID: nodeID, Local: true, State: RouteCheckUnmounted}
}
