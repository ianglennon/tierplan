// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"log/slog"
	"net/netip"
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

// maxHostRouteReaders is how many agents one Get reads at once.
const maxHostRouteReaders = 4

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
	local string
	log   *slog.Logger

	mu     sync.Mutex
	cache  map[string]struct{}
	onJoin func()
}

// NewHostRoutes returns the route check over the registry's agents and src.
func NewHostRoutes(svc *Service, src HostNetSource, o HostRoutesOptions) *HostRoutes {
	h := &HostRoutes{svc: svc, src: src, now: o.Now, local: o.LocalRouteFile, log: o.Log}
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

// readAgent asks one connected agent that said it can answer.
func (h *HostRoutes) readAgent(ctx context.Context, a AgentView, n NodeRoutes) NodeRoutes {
	doc, err := h.src.HostNet(ctx, a.ID)
	if err != nil {
		h.log.Debug("could not read the host routes of an agent", "agent_id", a.ID, "error", err)
		n.State = RouteCheckFailed
		return n
	}
	routes, ok := validate(doc)
	if !ok {
		h.log.Warn("an agent sent host routes that are not valid; none were used", "agent_id", a.ID)
		n.State = RouteCheckFailed
		return n
	}
	n.State, n.Routes, n.Truncated = RouteCheckOK, routes, doc.Truncated
	return n
}

// validate turns an agent's document into routes. It refuses the whole document on
// any bad entry.
func validate(doc wire.HostNetDoc) ([]hostroute.Route, bool) {
	if doc.V != 1 || doc.Error != "" {
		return nil, false
	}
	routes := make([]hostroute.Route, 0, len(doc.Routes))
	for _, r := range doc.Routes {
		dst, err := netip.ParsePrefix(r.Dst)
		if err != nil {
			return nil, false
		}
		rt := hostroute.Route{Iface: r.Iface, Dst: dst}
		if r.Gateway != "" {
			gw, err := netip.ParseAddr(r.Gateway)
			if err != nil {
				return nil, false
			}
			rt.Gateway = gw
		}
		routes = append(routes, rt)
	}
	return routes, true
}

// readLocal answers for the host Coaming itself runs on from the route file mounted
// into Coaming's own container. Anything but a readable table is "unmounted".
func (h *HostRoutes) readLocal(node, nodeID string) NodeRoutes {
	return NodeRoutes{Node: node, NodeID: nodeID, Local: true, State: RouteCheckUnmounted}
}
