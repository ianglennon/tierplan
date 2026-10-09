// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// NodeRef names one node an aggregate was read from. ReadOnly is true when the node's
// agent reported that its host owner made it read-only; the local socket is never.
type NodeRef struct {
	ID, Name, Role string
	ReadOnly       bool
}

// Limits of the swarm fan-out (D-46, T-02-72).
const (
	// defaultNodeTimeout is how long one node may take before it is named in
	// NotReporting.
	defaultNodeTimeout = 5 * time.Second
)

// swarmScope is the Scope Docker gives a cluster-wide network.
const swarmScope = "swarm"

// NodeCluster is the Node of a swarm-scoped network: it belongs to the cluster, not
// to the manager it was read from (D-46).
const NodeCluster = "Cluster"

// The row types of an aggregate are Docker's, plus the node the row was read from.
// The node lives here and not in dockerx, which stays unaware of nodes.

// Every row carries the node's display name (Node) and its ID (NodeID). The name is
// for people and can be shared by two hosts (T-02-73); a write is routed by the ID,
// which is the agent's ID, or the environment's own ID for the local socket, or
// socket:<envId> for the folded host (Aggregator.OnNode).

// ContainerRow is a container and the node it runs on.
type ContainerRow struct {
	dockerx.ContainerRow
	Node   string
	NodeID string
}

// ImageRow is an image and the node that holds it.
type ImageRow struct {
	dockerx.ImageRow
	Node   string
	NodeID string
}

// VolumeRow is a volume and the node that holds it.
type VolumeRow struct {
	dockerx.VolumeRow
	Node   string
	NodeID string
}

// NetworkRow is a network and the node it was read from: the node's own for a local
// network, NodeCluster for a swarm-scoped one. NodeID is the node that was read in
// both cases, so for a swarm-scoped network it is the manager in use.
type NetworkRow struct {
	dockerx.NetworkRow
	Node   string
	NodeID string
}

// Aggregate is what an environment's Docker read answers with: the rows of every
// node that answered, each with its node, and the names of the nodes that did not.
// A standalone or local environment is one node.
type Aggregate struct {
	Containers []ContainerRow
	Images     []ImageRow
	Volumes    []VolumeRow
	Networks   []NetworkRow
	// Counts are the totals of the rows above; each equals the length of its list.
	// Nodes and Services are the cluster's, from the manager in use, and nil where
	// no manager answered.
	Counts dockerx.Counts
	// ByNode is each reporting node's own counts, by node name. A node's networks
	// are its local ones: a swarm-scoped network is counted in the total only.
	ByNode map[string]dockerx.Counts
	// FetchedAt is when the oldest of the reads was taken.
	FetchedAt time.Time
	// Target is the monitor's status of the target the read went to: the manager
	// in use for a swarm.
	Target Status
	// Nodes are the nodes the data was read from.
	Nodes []NodeRef
	// NotReporting names the nodes expected to answer whose rows are left out:
	// agent missing, read failed or timed out (D-46). It is never nil.
	NotReporting []string
}

// ErrNotOnline is the error of a read of an environment that cannot be read: the
// environment's rolled-up Health says why (state, D-40 reason, since), and Target
// is the status of the target that was tried, which carries the engine of an
// unsupported one. A degraded environment can be read and never gets this error.
//
// errors.Is(err, ErrOffline) and errors.Is(err, ErrUnsupported) hold, so a caller
// that only distinguishes the Phase 1 pair keeps working.
type ErrNotOnline struct {
	Health Health
	Target Status
}

func (e ErrNotOnline) Error() string { return "envs: the environment is " + string(e.Health.State) }

// Is makes an unsupported environment match ErrUnsupported and every other one
// ErrOffline.
func (e ErrNotOnline) Is(target error) bool {
	switch target {
	case ErrUnsupported:
		return e.Health.State == StateUnsupported
	case ErrOffline:
		return e.Health.State != StateUnsupported
	}
	return false
}

// Aggregator is the single read path from an environment to its Docker target or
// targets (D-47). Every handler that needs an environment's Docker data asks it,
// and none asks the monitor or the snapshot cache by environment: an agent
// environment has no monitor of its own. Today it picks one target (the local
// socket, or the primary agent); the manager choice for cluster calls is plan 20's
// and the fan-out over every node of a swarm is plan 21's, inside this type.
type Aggregator struct {
	svc    *Service
	snaps  *Snapshots
	events *Hub
	now    func() time.Time
	// nodeTimeout is how long one node of a swarm may take.
	nodeTimeout time.Duration
	// feedPoll is how often a merged event feed looks for changes of target.
	feedPoll time.Duration

	// ctx ends the merged feeds; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	fmu    sync.Mutex
	feeds  map[string]*mergedFeed
	closed bool
}

// NewAggregator returns the aggregator over the registry, the snapshot cache and
// the events hub. now defaults to time.Now.
func NewAggregator(svc *Service, snaps *Snapshots, events *Hub, now func() time.Time) *Aggregator {
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Aggregator{
		svc: svc, snaps: snaps, events: events, now: now, nodeTimeout: defaultNodeTimeout,
		ctx: ctx, cancel: cancel, feeds: map[string]*mergedFeed{},
	}
}

// WithNodeTimeout sets how long one node of a swarm may take to answer before it is
// named in NotReporting; the default is five seconds. It returns a for chaining.
func (a *Aggregator) WithNodeTimeout(d time.Duration) *Aggregator {
	a.nodeTimeout = d
	return a
}

// Snapshot reads the counts and lists of an environment through its target and
// returns them with the environment's health. An environment that is not online or
// degraded answers ErrNotOnline carrying the health; an unknown one ErrNotFound.
// The snapshot cache and its single flight stay per target.
func (a *Aggregator) Snapshot(ctx context.Context, envID string) (Aggregate, Health, error) {
	now := a.now()
	in, err := a.svc.Input(ctx, envID, now)
	if err != nil {
		return Aggregate{}, Health{}, err
	}
	if in.Kind == KindLocal {
		return a.local(ctx, in)
	}

	h := Rollup(in)
	if h.State != StateOnline && h.State != StateDegraded {
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: primaryStatus(a.svc, in)}
	}
	if in.EnvType == envTypeSwarm {
		return a.swarm(ctx, in, h)
	}
	key, ok := a.svc.primary(in)
	if !ok {
		// Present inside the offline window but between sessions: nothing to read
		// through yet.
		return Aggregate{}, h, a.between(in)
	}
	snap, st, err := a.snaps.Get(ctx, key)
	switch {
	case errors.Is(err, ErrUnsupported):
		h := Health{State: StateUnsupported, Reason: ReasonUnsupportedEngine, Since: firstSet(st.Since, now)}
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case errors.Is(err, ErrOffline):
		h := Health{State: StateOffline, Reason: ReasonEngineUnreachable, Since: firstSet(st.Since, now)}
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case errors.Is(err, errNotMonitored):
		// The session is registered and its monitor has not started yet.
		return Aggregate{}, h, a.between(in)
	case err != nil:
		return Aggregate{}, h, err
	}
	ref := nodeOf(in, key, st)
	return oneNode(snap, st, ref, st.Probe.Type == dockerx.TypeSwarmManager), h, nil
}

// nodeOf is the node a single target's rows come from: the host name its agent
// reported, or for the local socket the engine's own.
func nodeOf(in RollupInput, key string, st Status) NodeRef {
	for _, a := range in.Agents {
		if a.TargetKey() == key {
			return NodeRef{ID: a.ID, Name: nodeName(a), Role: a.Role, ReadOnly: a.ReadOnly}
		}
	}
	name := cleanText(st.Probe.Hostname, maxNodeLen)
	if name == "" {
		name = "local"
	}
	return NodeRef{ID: in.EnvID, Name: name, Role: st.Probe.Type}
}

// swarmNets says what to do with the swarm-scoped networks a node lists.
type swarmNets int

const (
	// netsOfNode keeps them as the node's own, for an engine read alone.
	netsOfNode swarmNets = iota
	// netsOfCluster keeps them, labelled NodeCluster: the node is the manager in use.
	netsOfCluster
	// netsDropped leaves them out: another manager's list is the cluster's.
	netsDropped
)

// oneNode is the aggregate of an environment read through one target.
func oneNode(snap dockerx.Snapshot, st Status, ref NodeRef, manager bool) Aggregate {
	agg := Aggregate{
		Counts: snap.Counts, FetchedAt: snap.FetchedAt, Target: st, Nodes: []NodeRef{ref}, NotReporting: []string{},
		Containers: []ContainerRow{}, Images: []ImageRow{}, Volumes: []VolumeRow{}, Networks: []NetworkRow{},
	}
	nets := netsOfNode
	if manager {
		nets = netsOfCluster
	}
	agg.addRows(snap, ref, nets)
	agg.ByNode = map[string]dockerx.Counts{ref.Name: snap.Counts}
	return agg
}

// addRows appends a node's rows, each with its node name and ID.
func (a *Aggregate) addRows(snap dockerx.Snapshot, ref NodeRef, nets swarmNets) {
	node := ref.Name
	for _, c := range snap.Containers {
		a.Containers = append(a.Containers, ContainerRow{ContainerRow: c, Node: node, NodeID: ref.ID})
	}
	for _, i := range snap.Images {
		a.Images = append(a.Images, ImageRow{ImageRow: i, Node: node, NodeID: ref.ID})
	}
	for _, v := range snap.Volumes {
		a.Volumes = append(a.Volumes, VolumeRow{VolumeRow: v, Node: node, NodeID: ref.ID})
	}
	for _, n := range snap.Networks {
		label := node
		if n.Scope == swarmScope {
			switch nets {
			case netsDropped:
				continue
			case netsOfCluster:
				label = NodeCluster
			}
		}
		a.Networks = append(a.Networks, NetworkRow{NetworkRow: n, Node: label, NodeID: ref.ID})
	}
}

// nodeRead is the outcome of reading one node of a swarm.
type nodeRead struct {
	agent AgentView
	snap  dockerx.Snapshot
	st    Status
	err   error
}

// swarm reads every connected agent of a swarm at once, at most 16 at a
// time and each for at most the node timeout, and merges what answered (D-46).
// Containers, images and volumes are node-local, so every row keeps its node and a
// name used on two nodes stays two rows. Swarm-scoped networks are the cluster's:
// they come from the manager in use only and are labelled NodeCluster. A node that
// is expected and did not answer is named in NotReporting and its rows are left
// out, so a total is never presented as complete when it is not.
//
// With no agent connected at all the lists are empty and every expected node is
// named; the health says whether the environment is still considered readable.
func (a *Aggregator) swarm(ctx context.Context, in RollupInput, h Health) (Aggregate, Health, error) {
	reads := a.fanOut(ctx, connectedAgents(in))
	if err := ctx.Err(); err != nil {
		return Aggregate{}, h, err
	}
	var ok []nodeRead
	var denied error
	deniedAll := true
	for _, r := range reads {
		if r.err == nil {
			ok = append(ok, r)
			continue
		}
		a.svc.log.Debug("a node of a swarm did not report", "environment", in.EnvID, "node", nodeName(r.agent), "error", r.err)
		err := ClassifyDockerError(r.err)
		var pd *AgentPolicyDeniedError
		if errors.As(err, &pd) {
			denied = err
		} else {
			deniedAll = false
		}
	}
	if len(ok) == 0 && len(reads) > 0 && denied != nil && deniedAll {
		// Every node's agent refused by its own policy: that is a Bayplan fault, not
		// a node that is missing, and the data route says so.
		return Aggregate{}, h, denied
	}

	// The manager in use answers for the cluster; if it did not, another manager that
	// did is as good a source.
	designated := -1
	inUse, _ := a.svc.primary(in)
	for i, r := range ok {
		if r.agent.Role != roleManager {
			continue
		}
		if r.agent.TargetKey() == inUse {
			designated = i
			break
		}
		if designated < 0 {
			designated = i
		}
	}

	agg := Aggregate{
		Containers: []ContainerRow{}, Images: []ImageRow{}, Volumes: []VolumeRow{}, Networks: []NetworkRow{},
		ByNode: map[string]dockerx.Counts{}, Nodes: []NodeRef{}, NotReporting: []string{},
	}
	reported := map[string]bool{}
	for i, r := range ok {
		node := nodeName(r.agent)
		reported[agentKey(r.agent)] = true
		nets := netsDropped
		if i == designated {
			nets = netsOfCluster
			agg.Target = r.st
			agg.Counts.Nodes, agg.Counts.Services = r.snap.Counts.Nodes, r.snap.Counts.Services
		}
		before := len(agg.Networks)
		ref := NodeRef{ID: r.agent.ID, Name: node, Role: r.agent.Role, ReadOnly: r.agent.ReadOnly}
		agg.addRows(r.snap, ref, nets)
		agg.Nodes = append(agg.Nodes, ref)
		agg.ByNode[node] = nodeCounts(r.snap, agg.Networks[before:])
		if agg.FetchedAt.IsZero() || r.snap.FetchedAt.Before(agg.FetchedAt) {
			agg.FetchedAt = r.snap.FetchedAt
		}
	}
	if designated < 0 {
		agg.Target = primaryStatus(a.svc, in)
	}
	if agg.FetchedAt.IsZero() {
		agg.FetchedAt = a.now()
	}
	agg.total()
	agg.sortRows()
	slices.SortFunc(agg.Nodes, func(x, y NodeRef) int { return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.ID, y.ID)) })
	for _, e := range a.expectedNodes(in) {
		if !reported[e.key] {
			agg.NotReporting = append(agg.NotReporting, e.name)
		}
	}
	sort.Strings(agg.NotReporting)
	return agg, h, nil
}

// fanOut reads the snapshot of each agent's target in parallel. Each read is bounded
// by the node timeout, so one slow or hostile node cannot hold the page (T-02-72); a
// read that times out stops being waited for while the shared fetch carries on and
// fills the cache for the next one.
func (a *Aggregator) fanOut(ctx context.Context, agents []AgentView) []nodeRead {
	reads := make([]nodeRead, len(agents))
	var g errgroup.Group
	g.SetLimit(16) // at most 16 nodes are read at once (T-02-72)
	for i, ag := range agents {
		g.Go(func() error {
			nctx, cancel := context.WithTimeout(ctx, a.nodeTimeout)
			defer cancel()
			snap, st, err := a.snaps.Get(nctx, ag.TargetKey())
			reads[i] = nodeRead{agent: ag, snap: snap, st: st, err: err}
			return nil
		})
	}
	_ = g.Wait() // no read returns an error: each outcome is in its slot
	return reads
}

// agentKey identifies the node an agent runs on: the swarm node when it said so,
// else the agent.
func agentKey(a AgentView) string {
	if a.SwarmNodeID != "" {
		return a.SwarmNodeID
	}
	return a.TargetKey()
}

type expectedNode struct{ key, name string }

// expectedNodes are the nodes that should answer a read: every agent not revoked,
// and every node the manager's last node list says should run one (active, and
// ready or down: a drained or paused node gets no agent task). An agent and its
// node are one entry.
func (a *Aggregator) expectedNodes(in RollupInput) []expectedNode {
	var out []expectedNode
	seen := map[string]bool{}
	add := func(key, name string) {
		if !seen[key] {
			seen[key] = true
			out = append(out, expectedNode{key, name})
		}
	}
	for _, ag := range in.Agents {
		if !ag.Revoked {
			add(agentKey(ag), nodeName(ag))
		}
	}
	if a.svc.clusters != nil {
		if st, ok := a.svc.clusters.Cached(in.EnvID); ok && st.Known() {
			for _, n := range st.Read.Nodes {
				if n.Availability == "active" && (n.State == "ready" || n.State == "down") {
					name := cleanText(n.Hostname, maxNodeLen)
					if name == "" {
						name = cleanText(n.ID, maxNodeLen)
					}
					add(n.ID, name)
				}
			}
		}
	}
	return out
}

// nodeCounts are one node's counts from its own rows; nets are the network rows it
// contributed, so a swarm-scoped network left out is not counted for it.
func nodeCounts(snap dockerx.Snapshot, nets []NetworkRow) dockerx.Counts {
	c := dockerx.Counts{Containers: len(snap.Containers), Images: len(snap.Images), Volumes: len(snap.Volumes)}
	for _, n := range nets {
		if n.Scope != swarmScope {
			c.Networks++
		}
	}
	for _, row := range snap.Containers {
		if row.State == string(container.StateRunning) {
			c.ContainersRunning++
		}
	}
	c.ContainersStopped = c.Containers - c.ContainersRunning
	return c
}

// total sets the cluster totals from the rows, so each count is the length of the
// list it links to. Nodes and Services are the manager's and are set by the caller.
func (a *Aggregate) total() {
	for _, c := range a.ByNode {
		a.Counts.Containers += c.Containers
		a.Counts.ContainersRunning += c.ContainersRunning
		a.Counts.ContainersStopped += c.ContainersStopped
		a.Counts.Images += c.Images
		a.Counts.Volumes += c.Volumes
	}
	a.Counts.Networks = len(a.Networks)
}

// sortRows puts the merged rows in the order each list is shown in: the order of
// dockerx, with the node as the last tie-break so equal rows keep a stable order.
func (a *Aggregate) sortRows() {
	slices.SortFunc(a.Containers, func(x, y ContainerRow) int {
		return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.Node, y.Node), cmp.Compare(x.ID, y.ID))
	})
	slices.SortFunc(a.Images, func(x, y ImageRow) int {
		return cmp.Or(y.Created.Compare(x.Created), cmp.Compare(x.ID, y.ID), cmp.Compare(x.Node, y.Node))
	})
	slices.SortFunc(a.Volumes, func(x, y VolumeRow) int {
		return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.Node, y.Node))
	})
	slices.SortFunc(a.Networks, func(x, y NetworkRow) int {
		return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.ID, y.ID), cmp.Compare(x.Node, y.Node))
	})
}

// local reads the local socket: its health is the monitor's status, kept exactly as
// Phase 1 answered it, including waiting for the first probe of a new environment.
func (a *Aggregator) local(ctx context.Context, in RollupInput) (Aggregate, Health, error) {
	snap, st, err := a.snaps.Get(ctx, in.EnvID)
	in.Local = st
	h := Rollup(in)
	switch {
	case errors.Is(err, ErrOffline), errors.Is(err, ErrUnsupported):
		return Aggregate{}, h, ErrNotOnline{Health: h, Target: st}
	case err != nil:
		return Aggregate{}, h, err
	}
	return oneNode(snap, st, nodeOf(in, in.EnvID, st), st.Probe.Type == dockerx.TypeSwarmManager), h, nil
}

// between is the error for an environment whose agents are present but have no
// live session: reconnecting, since the last session ended.
func (a *Aggregator) between(in RollupInput) error {
	var ended time.Time
	for _, ag := range in.Agents {
		ended = later(ended, ag.Ended)
	}
	h := Health{State: StateReconnecting, Reason: ReasonAgentOffline, Since: firstSet(ended, in.ServerStart)}
	return ErrNotOnline{Health: h}
}

// primaryStatus is the monitor's status of the environment's primary target, or
// unknown when it has none.
func primaryStatus(svc *Service, in RollupInput) Status {
	if key, ok := svc.primary(in); ok {
		return svc.mon.Status(key)
	}
	return Status{State: StateUnknown}
}
