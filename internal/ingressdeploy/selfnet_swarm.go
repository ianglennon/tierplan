// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// SelfNetworkPrefix is the prefix length of the self network's subnet: a /24. It holds one address per proxy task and
// TierPlan's own, so a /24 leaves room on any swarm TierPlan manages, and it comes from the same planner as the ingress
// network's /22.
const SelfNetworkPrefix = 24

// SelfNetworkSpec is the self network on the given subnet (D-02): a swarm-scoped overlay that is not attachable, so a plain
// `docker run --network tierplan-self` is refused and only service specs join it, carrying SelfNetworkLabels. It is created
// without the overlay's `encrypted` option: the proxy speaks TLS to TierPlan over it already (RESEARCH A2), and IPsec on top
// would only cost every request.
func SelfNetworkSpec(subnet string) dockerx.NetworkCreateSpec {
	return dockerx.NetworkCreateSpec{
		Name:       ingress.SelfNetworkName,
		Driver:     "overlay",
		Scope:      dockerx.ScopeSwarm,
		Attachable: false,
		Labels:     ingress.SelfNetworkLabels(),
		Subnet:     subnet,
	}
}

// selfNet is what the self network step found or made on one environment.
type selfNet struct {
	// attach is whether the proxy is to be on the network: it is TierPlan's and wanted.
	attach bool
	// id is the network's ID when TierPlan's own exists, "" otherwise.
	id string
	// foreign is a network of the reserved name that is not TierPlan's: it is never attached, adopted or removed.
	foreign bool
	// keptInUse is TierPlan's own network left in place though not wanted: TierPlan is on it, or the engine refused its
	// removal because something still is.
	keptInUse bool
	// removed is TierPlan's own network removed by this pass, so a read after it that fails still publishes it gone (WR-01).
	removed bool
}

// selfPass is what one ensure pass knows of the self network before it reads the swarm.
type selfPass struct {
	info EnvInfo
	// place is where TierPlan runs, from SelfLocation.
	place SelfPlace
	// wanted is whether the self network is wanted; known is false when that could not be read, and then nothing is made or
	// removed and the proxy keeps the attachment it has.
	wanted, known bool
	// net is what the step under the lock found, when it ran.
	net selfNet
}

// selfPassOf reads where TierPlan runs and whether the environment wants the self network.
func (s *Service) selfPassOf(ctx context.Context, info EnvInfo) *selfPass {
	place, _, err := s.SelfLocation(ctx, info)
	if err != nil {
		place = s.selfUnknown()
	}
	sp := &selfPass{info: info, place: place}
	wanted, err := s.selfWanted(ctx, info, place)
	if err != nil {
		s.log().Warn("the ingress ensure loop could not tell whether TierPlan's own network is wanted", "environment", info.ID, "error", err)
		return sp
	}
	sp.wanted, sp.known = wanted, true
	return sp
}

// selfWanted is whether the environment needs the self network: TierPlan's own route (the system claim) is held in it, or
// TierPlan's own certificate renews over HTTP-01 and TierPlan runs here, so the proxy carries the challenge to it (the
// carve-out, SELF-04).
func (s *Service) selfWanted(ctx context.Context, info EnvInfo, place SelfPlace) (bool, error) {
	claim, ok, err := s.d.Store.SystemClaim(ctx)
	if err != nil {
		return false, err
	}
	if ok && claim.EnvID == info.ID {
		return true, nil
	}
	if s.d.OwnHTTP01InUse == nil || place.Here != SelfHereYes {
		return false, nil
	}
	return s.d.OwnHTTP01InUse(ctx)
}

// attachWanted is whether the proxy is to be on the self network as the swarm was read in l: the network is TierPlan's and
// wanted. When wanted could not be read, the proxy keeps what it has.
func (sp *selfPass) attachWanted(l live) bool {
	row, ok := ownSelfNetwork(l.networks)
	if !ok {
		return false
	}
	if !sp.known {
		return l.service != nil && specNamesNetwork(l.service.Spec, row)
	}
	return sp.wanted
}

// selfWork is whether the self network has a write waiting: it is wanted and missing, or it is TierPlan's, not wanted and
// ready to go (removable).
func (sp *selfPass) selfWork(l live) bool {
	if !sp.known {
		return false
	}
	if sp.wanted {
		_, ok := selfNetworkRowOf(l.networks)
		return !ok
	}
	return sp.removable(l)
}

// removable is whether TierPlan's own self network, not wanted, may be removed now (A-06): the proxy as read no longer names
// it, so the update that dropped it has been written, and TierPlan's own container is not on it. A network TierPlan's service
// still names is the person's to take out of the stack file; TierPlan never updates its own service (D-03).
func (sp *selfPass) removable(l live) bool {
	if !sp.known || sp.wanted || sp.place.Attached {
		return false
	}
	row, ok := ownSelfNetwork(l.networks)
	if !ok {
		return false
	}
	return l.service == nil || !specNamesNetwork(l.service.Spec, row)
}

// removeSelfNetwork removes TierPlan's own self network when it is not wanted and removable as l was read under the lock.
// While TierPlan is on it, it is kept (KeptInUse) and nothing is tried; an engine that answers the network is still in use is
// the same, never a failed pass. c is a manager's client, inside one write.
func (s *Service) removeSelfNetwork(ctx context.Context, c *dockerx.Client, sp *selfPass, l live) error {
	if !sp.known || sp.wanted || sp.net.id == "" || sp.net.foreign {
		return nil
	}
	if sp.place.Attached {
		sp.net.keptInUse = true
		return nil
	}
	if !sp.removable(l) {
		// The proxy still names it: this pass's update takes it off, and a later pass removes the network.
		return nil
	}
	err := c.NetworkRemove(ctx, sp.net.id)
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

// selfNetworkRowOf is the network of the reserved name among rows, TierPlan's or not.
func selfNetworkRowOf(rows []dockerx.NetworkRow) (dockerx.NetworkRow, bool) {
	for _, row := range rows {
		if row.Name == ingress.SelfNetworkName {
			return row, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// wantNow is the wanted value the step under the lock is given: what selfWanted said, or, when that could not be read,
// whether the proxy is on the network already, so nothing is made and nothing taken away.
func (sp *selfPass) wantNow(l live) bool {
	if sp.known {
		return sp.wanted
	}
	return sp.attachWanted(l)
}

// ensureSelfNetwork is the self network step of a swarm pass (D-02), before the proxy spec is built. TierPlan's own network
// (the name and the label pair, OwnsSelfNetwork) is adopted whatever its subnet; one of the name that is not TierPlan's is
// foreign and is never attached. When wanted and absent, a /24 is made from the swarm's pools under the address-space lock,
// by the planner the ingress network's /22 comes from. c is a manager's client, inside one write.
func (s *Service) ensureSelfNetwork(ctx context.Context, c *dockerx.Client, info EnvInfo, wanted bool) (selfNet, error) {
	rows, err := c.Networks(ctx)
	if err != nil {
		return selfNet{}, err
	}
	for _, row := range rows {
		if row.Name != ingress.SelfNetworkName {
			continue
		}
		if !ownsSelfNetworkRow(row, true) {
			return selfNet{foreign: true}, nil
		}
		return selfNet{attach: wanted, id: row.ID}, nil
	}
	if !wanted {
		return selfNet{}, nil
	}
	if s.d.AddressSpaces != nil {
		defer s.d.AddressSpaces.LockAllocation(info.ID)()
	}
	subnet, err := s.chooseSubnetSized(ctx, info.ID, true, SelfNetworkPrefix)
	if err != nil {
		return selfNet{}, fmt.Errorf("choose the self network's subnet: %w", err)
	}
	id, _, err := c.NetworkCreateAllocated(ctx, SelfNetworkSpec(subnet), s.d.AllocationWait)
	if err != nil {
		return selfNet{}, fmt.Errorf("create the self network: %w", err)
	}
	return selfNet{attach: true, id: id}, nil
}

// ownSelfNetwork is TierPlan's own self network among the rows of a swarm.
func ownSelfNetwork(rows []dockerx.NetworkRow) (dockerx.NetworkRow, bool) {
	for _, row := range rows {
		if ownsSelfNetworkRow(row, true) {
			return row, true
		}
	}
	return dockerx.NetworkRow{}, false
}

// ownsSelfNetworkRow is whether a network is TierPlan's own self network (WR-05): the name and both labels
// (ingress.OwnsSelfNetwork), and the shape TierPlan makes it in, since labels are anyone's to set. On a swarm that is an
// overlay of swarm scope that is not attachable (D-02): an attachable one would let any plain container join the trusted set,
// and a local bridge of the name could not be put in the proxy's spec. On a standalone host it is a bridge.
func ownsSelfNetworkRow(row dockerx.NetworkRow, swarm bool) bool {
	return ingress.OwnsSelfNetwork(row.Name, row.Labels) && selfShapeReason(row, swarm) == ""
}

// selfShapeReason says what is wrong with the shape of a network named tierplan-self, for the log, "" when it is the shape
// TierPlan makes and its labels are TierPlan's.
func selfShapeReason(row dockerx.NetworkRow, swarm bool) string {
	switch {
	case !ingress.OwnsSelfNetwork(row.Name, row.Labels):
		return "it lacks TierPlan's labels"
	case swarm && row.Driver != "overlay":
		return "it is not an overlay network"
	case swarm && row.Scope != dockerx.ScopeSwarm:
		return "it is not swarm-scoped"
	case swarm && row.Attachable:
		return "it is attachable"
	case !swarm && row.Driver != dockerx.DriverBridge:
		return "it is not a bridge network"
	}
	return ""
}

// specNamesNetwork is whether a stored service spec attaches to the network, which swarm stores by ID and a spec may name.
func specNamesNetwork(stored json.RawMessage, row dockerx.NetworkRow) bool {
	var spec swarm.ServiceSpec
	if json.Unmarshal(stored, &spec) != nil {
		return false
	}
	for _, t := range spec.TaskTemplate.Networks {
		if (row.ID != "" && t.Target == row.ID) || t.Target == row.Name {
			return true
		}
	}
	return false
}

// CodeSelfNetworkForeign is the log code of a network named tierplan-self that is not TierPlan's: it lacks the ingress label
// or the self role. It is never attached, adopted or trusted (T-28-37); the person removes or renames it.
const CodeSelfNetworkForeign = "self_network_foreign"

// CodeSelfNetworkForeignMember is the log code, and the card's warning, of a service or container on TierPlan's own self
// network that is neither the proxy nor TierPlan (CR-02). TierPlan believes no forwarded address from the network while one
// is there.
const CodeSelfNetworkForeignMember = "self_network_foreign_member"

// selfBridgeNetworks are the local bridges whose gateways a connection from the node itself can carry. Their gateways go into
// the observation so the trusted set never holds them (D-06).
var selfBridgeNetworks = []string{"bridge", "docker_gwbridge"}

// SelfObservation is what one ensure pass read of an environment's self network, published to the Service's memory and to
// the OnSelfObserved callback after every pass that could read the swarm (D-06, A-08). It carries the inputs of
// ingress.SelfNetFacts (Facts), which the server turns into the trusted-proxy set.
type SelfObservation struct {
	EnvID string
	// Exists is TierPlan's own self network (the name and the label pair) being there. A foreign one is not: Exists is false,
	// Foreign true, and nothing of it is reported.
	Exists bool
	// Wanted is whether the environment wants the self network: the system claim is held in it, or TierPlan's own
	// certificate renews over HTTP-01 and TierPlan runs here. It is false when that could not be read.
	Wanted bool
	// NetworkID, Subnet and Gateway are the self network's, from the engine's inspect of it; empty unless Exists.
	NetworkID string
	Subnet    netip.Prefix
	Gateway   netip.Addr
	// IngressSubnets are the subnets of every tierplan-ingress network seen, and BridgeGateways the gateways of the node's
	// bridge and docker_gwbridge networks, as the manager read reports them.
	IngressSubnets []netip.Prefix
	BridgeGateways []netip.Addr
	// Attached is TierPlan's own container being on the self network, from SelfLocation; false unless Exists.
	Attached bool
	// Kind is where TierPlan runs (SelfPlace.Kind), "" when it is not here or cannot be told.
	Kind string
	// Namespace is the swarm stack namespace of TierPlan's own service when Kind is SelfKindSwarmService, "" otherwise.
	Namespace string
	// BackendHost is the name the proxy reaches TierPlan by on the network: TierPlan's swarm service name, or on a standalone
	// host the alias TierPlan connects itself with (SelfBackendAlias).
	BackendHost string
	// Foreign is a network of the reserved name that is not TierPlan's, and ForeignReason what makes it so (its labels or its
	// shape), for the log.
	Foreign       bool
	ForeignReason string
	// ForeignMembers name, sorted, the services (on a swarm) or containers (on a standalone host) on TierPlan's own self
	// network that are neither the proxy nor TierPlan's own (CR-02). While there is one, the trusted-proxy set is withheld:
	// such a member could send any client address and have it believed. Empty unless Exists.
	ForeignMembers []string
	// KeptInUse is TierPlan's own network left in place, though not wanted, because TierPlan (its service, or on a standalone
	// host its container) or something else is still on it.
	KeptInUse bool
	// Observed is true for every observation a pass made; the zero value is "no pass has read it yet".
	Observed bool
}

// Facts are the observation as the trusted-set builder's input. A foreign or missing network gives an invalid subnet, which
// builds an empty set.
func (o SelfObservation) Facts() ingress.SelfNetFacts {
	return ingress.SelfNetFacts{
		Subnet: o.Subnet, Gateway: o.Gateway, IngressSubnets: slices.Clone(o.IngressSubnets), BridgeGateways: slices.Clone(o.BridgeGateways),
	}
}

// OnSelfObserved registers the function every SelfObservation is handed to, replacing any earlier one. It is called after the
// observation is in the Service's memory, outside its locks, and may be called for two environments at once.
func (s *Service) OnSelfObserved(f func(SelfObservation)) {
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	s.selfCb = f
}

// SelfObservationOf is the last observation of the environment's self network, and whether a pass has made one since start.
func (s *Service) SelfObservationOf(envID string) (SelfObservation, bool) {
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	o, ok := s.selfObs[envID]
	return o, ok
}

// SelfStackNamespace is the swarm stack namespace of TierPlan's own service in the environment, as the last pass observed
// it, "" when TierPlan does not run there as a service of a stack or no pass has observed it. The stack check lets that one
// stack point at tierplan-self (CR-02). It reads memory only.
func (s *Service) SelfStackNamespace(envID string) string {
	o, ok := s.SelfObservationOf(envID)
	if !ok || o.Kind != SelfKindSwarmService {
		return ""
	}
	return o.Namespace
}

// observeSelf reads the self network, the ingress networks and the node's bridges on a manager and publishes what it found. A
// read that fails publishes nothing, so the last observation stands rather than one with a part missing; so does a pass that
// could not tell where TierPlan runs (its own container or the local engine did not answer), which does not know whether
// TierPlan is on the network (WR-03).
func (s *Service) observeSelf(ctx context.Context, sp *selfPass) {
	s.observeSelfOn(ctx, sp, func(fn func(*dockerx.Client) error) error { return s.d.Engine.ClusterRead(ctx, sp.info.ID, fn) })
}

// observeSelfOn is observeSelf through read, which runs a read-only function on the engine that holds the self network: a
// swarm's manager, or a standalone host's one engine. It returns whether it published.
func (s *Service) observeSelfOn(ctx context.Context, sp *selfPass, read func(func(*dockerx.Client) error) error) (SelfObservation, bool) {
	envID := sp.info.ID
	if sp.place.Here == SelfHereUnknown && !sp.net.removed {
		// "Not attached" would be a guess: it would drop the route and the trusted set until the next pass. A pass that
		// removed the network still publishes, since that is known whatever TierPlan's container says.
		s.log().Debug("the ingress ensure loop could not tell where TierPlan runs; the last observation of its own network stands", "environment", envID)
		return SelfObservation{}, false
	}
	o := SelfObservation{EnvID: envID, Wanted: sp.known && sp.wanted, Kind: sp.place.Kind, Observed: true}
	switch sp.place.Kind {
	case SelfKindSwarmService:
		o.BackendHost, o.Namespace = sp.place.ServiceName, sp.place.Namespace
	case SelfKindStandalone:
		o.BackendHost = SelfBackendAlias
	}
	err := read(func(c *dockerx.Client) error {
		o.Exists, o.NetworkID, o.Subnet, o.Gateway, o.Foreign, o.ForeignReason = false, "", netip.Prefix{}, netip.Addr{}, false, ""
		o.IngressSubnets, o.BridgeGateways, o.ForeignMembers = nil, nil, nil
		rows, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Name == ingress.NetworkName {
				for _, sub := range row.Subnets {
					if p, err := netip.ParsePrefix(sub); err == nil {
						o.IngressSubnets = append(o.IngressSubnets, p.Masked())
					}
				}
			}
		}
		if row, ok := selfNetworkRowOf(rows); ok {
			if !ownsSelfNetworkRow(row, sp.info.Swarm) {
				o.Foreign, o.ForeignReason = true, selfShapeReason(row, sp.info.Swarm)
			} else {
				d, _, err := c.NetworkDetail(ctx, row.ID, false)
				if err != nil {
					return err
				}
				o.Exists, o.NetworkID = true, d.ID
				o.Subnet, o.Gateway = firstIPAM(d.IPAM)
				if o.ForeignMembers, err = foreignSelfMembers(ctx, c, sp, d.ID); err != nil {
					return err
				}
			}
		}
		for _, name := range selfBridgeNetworks {
			d, _, err := c.NetworkDetail(ctx, name, false)
			if dockerx.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			for _, r := range d.IPAM {
				if g, err := netip.ParseAddr(r.Gateway); err == nil {
					o.BridgeGateways = append(o.BridgeGateways, g)
				}
			}
		}
		return nil
	})
	if err != nil {
		s.log().Warn("the ingress ensure loop could not read TierPlan's own network", "environment", envID, "error", err)
		if !sp.net.removed {
			return SelfObservation{}, false
		}
		// This pass removed the network: its subnet is free for the engine to hand out again, so it must not stay trusted on
		// the strength of the last observation. Only the removal is published; the rest waits for the next read.
		gone := SelfObservation{EnvID: envID, Wanted: o.Wanted, Kind: o.Kind, Namespace: o.Namespace, BackendHost: o.BackendHost, Observed: true}
		s.publishSelf(gone)
		return gone, true
	}
	o.Attached = o.Exists && sp.place.Attached
	o.KeptInUse = o.Exists && sp.known && !sp.wanted && (sp.place.Attached || sp.net.keptInUse)
	s.publishSelf(o)
	return o, true
}

// foreignSelfMembers names, sorted, what is on the self network networkID besides the proxy and TierPlan's own: on a swarm the
// services whose spec or virtual IPs name it, from the manager's records, other than the proxy service and TierPlan's own
// service; on a standalone host the containers attached to it other than the proxy container and TierPlan's own (CR-02). A
// container connected to an overlay at engine level on a node that holds it is not seen on a swarm (the S5 caveat, residual
// risk 76).
func foreignSelfMembers(ctx context.Context, c *dockerx.Client, sp *selfPass, networkID string) ([]string, error) {
	var out []string
	if sp.info.Swarm {
		refs, err := c.ServiceRefs(ctx)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			on := func(id string) bool { return id == networkID || id == ingress.SelfNetworkName }
			if !slices.ContainsFunc(ref.Networks, on) && !slices.ContainsFunc(ref.VIPNetworks, on) {
				continue
			}
			if ref.Name == ingress.ServiceName || (sp.place.ServiceID != "" && ref.ID == sp.place.ServiceID) ||
				(sp.place.ServiceName != "" && ref.Name == sp.place.ServiceName) {
				continue
			}
			out = append(out, ref.Name)
		}
	} else {
		rows, err := c.ContainerRowsOnNetwork(ctx, ingress.SelfNetworkName)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if ingress.OwnsProxyContainer(row.Labels) || sameContainer(row.ID, sp.place.ContainerID) {
				continue
			}
			out = append(out, row.Name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// sameContainer is whether two container IDs, either of which may be the short form, name the same container.
func sameContainer(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// firstIPAM is the first pool of a network with a valid subnet, and its gateway when the engine reports one.
func firstIPAM(rows []dockerx.IPAMRow) (netip.Prefix, netip.Addr) {
	for _, r := range rows {
		p, err := netip.ParsePrefix(r.Subnet)
		if err != nil {
			continue
		}
		g, _ := netip.ParseAddr(r.Gateway)
		return p.Masked(), g
	}
	return netip.Prefix{}, netip.Addr{}
}

// publishSelf keeps the observation and hands it to the registered function. A foreign network is logged when it is first
// seen while the self network is wanted, not on every pass.
func (s *Service) publishSelf(o SelfObservation) {
	s.selfMu.Lock()
	prev, had := s.selfObs[o.EnvID]
	if s.selfObs == nil {
		s.selfObs = map[string]SelfObservation{}
	}
	s.selfObs[o.EnvID] = o
	if s.selfSeq == nil {
		s.selfSeq = map[string]uint64{}
	}
	s.selfSeq[o.EnvID]++
	if s.selfTick != nil {
		close(s.selfTick)
		s.selfTick = nil
	}
	cb := s.selfCb
	s.selfMu.Unlock()
	if o.Foreign && o.Wanted && (!had || !prev.Foreign || !prev.Wanted) {
		s.log().Warn("a network named "+ingress.SelfNetworkName+" exists that is not TierPlan's, so the proxy is not attached to it: remove or rename it",
			"environment", o.EnvID, "code", CodeSelfNetworkForeign, "reason", o.ForeignReason)
	}
	if len(o.ForeignMembers) > 0 && (!had || !slices.Equal(prev.ForeignMembers, o.ForeignMembers)) {
		s.log().Warn("something other than the proxy and TierPlan is on "+ingress.SelfNetworkName+", so TierPlan believes no client address "+
			"forwarded from it until it leaves: remove it from that network", "environment", o.EnvID, "members", strings.Join(o.ForeignMembers, ", "),
			"code", CodeSelfNetworkForeignMember)
	}
	if o.KeptInUse && (!had || !prev.KeptInUse) {
		s.log().Info("TierPlan's own network "+ingress.SelfNetworkName+" is no longer needed but is kept while something is still on it. "+
			"If TierPlan's stack file names it, take those lines out and redeploy TierPlan; a later pass removes the network", "environment", o.EnvID)
	}
	if cb != nil {
		cb(o)
	}
}
