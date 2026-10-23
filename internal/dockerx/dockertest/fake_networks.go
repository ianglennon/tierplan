// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

var _ = registerFakeHandler((*Fake).serveNetworkCreate)
var _ = registerFakeHandler((*Fake).serveNetworkRemove)
var _ = registerFakeHandler((*Fake).serveNetworkInspect)
var _ = registerFakeHandler((*Fake).serveNetworkConnect)
var _ = registerFakeHandler((*Fake).serveNetworkDisconnect)

// serveNetworkCreate answers POST /networks/create. A name the daemon already has is
// the 409 a real one gives. The network joins the lists the daemon answers, with the
// subnet of its IPAM configuration, and the body is kept for NetworkCreates. The write
// is recorded whatever the answer.
func (f *Fake) serveNetworkCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/networks/create" {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	var req network.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	for _, n := range cfg.Networks {
		if n.Name == req.Name {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "network with name " + req.Name + " already exists"})
			return true
		}
	}
	scope := "local"
	if req.Driver == "overlay" || req.Scope == "swarm" {
		scope = "swarm"
	}
	f.mu.Lock()
	seq := len(cfg.Networks) + len(f.netGone)
	f.mu.Unlock()
	fn := FakeNetwork{
		ID: newFakeNetworkID(seq), Name: req.Name, Driver: req.Driver, Scope: scope, Created: time.Now().UTC(),
		Attachable: req.Attachable, Internal: req.Internal, Labels: req.Labels, Options: req.Options,
	}
	if req.IPAM != nil {
		for _, c := range req.IPAM.Config {
			if c.Subnet.IsValid() {
				fn.Subnets = append(fn.Subnets, c.Subnet.String())
			}
		}
	}
	f.mu.Lock()
	f.nets = append(f.nets, fn)
	f.netCreates = append(f.netCreates, req)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, network.CreateResponse{ID: fn.ID})
	return true
}

// newFakeNetworkID is a 64 character hex ID like a daemon's, made from n. What tells one
// network from another comes first, so that, like a daemon's random IDs, a prefix of any
// length a client uses, including the ID less its last character, names one network.
func newFakeNetworkID(n int) string {
	return fmt.Sprintf("%08x%s", 0xbeef0000+n, strings.Repeat("0", 56))
}

// NetworkCreates returns the body of every network create the daemon accepted, in
// order, as the daemon decoded it.
func (f *Fake) NetworkCreates() []network.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.netCreates)
}

// withNetworks returns c with the networks created through the daemon listed after its
// own. The slice is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withNetworks(created []FakeNetwork) FakeConfig {
	if len(created) == 0 {
		return c
	}
	c.Networks = append(slices.Clone(c.Networks), created...)
	return c
}

// networkPath matches DELETE /networks/{id}.
var networkPath = regexp.MustCompile(`^/networks/([^/]+)$`)

// serveNetworkRemove answers DELETE /networks/{id}. An ID or name the daemon does not
// know is the 404 a real one gives; bridge, host and none are the 403 "pre-defined
// network" refusal; a network some container is attached to is the 403 "has active
// endpoints" refusal of a current engine. Otherwise the network leaves every list. The
// write is recorded whatever the answer.
func (f *Fake) serveNetworkRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := networkPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	var found *FakeNetwork
	for i, n := range cfg.Networks {
		if n.ID == m[1] || n.Name == m[1] || (len(m[1]) >= 4 && strings.HasPrefix(n.ID, m[1])) {
			found = &cfg.Networks[i]
			break
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "network " + m[1] + " not found"})
		return true
	}
	if msg, bad := cfg.RemoveErrors[found.Name]; bad {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": msg})
		return true
	}
	switch found.Name {
	case "bridge", "host", "none":
		writeJSON(w, http.StatusForbidden, map[string]string{"message": found.Name + " is a pre-defined network and cannot be removed"})
		return true
	}
	for _, c := range cfg.Containers {
		for name, ep := range c.Networks {
			if ep.NetworkID == found.ID || (ep.NetworkID == "" && name == found.Name) {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "error while removing network: network " + found.Name + " id " + found.ID + " has active endpoints"})
				return true
			}
		}
	}
	f.mu.Lock()
	if f.netGone == nil {
		f.netGone = map[string]bool{}
	}
	f.netGone[found.ID] = true
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	return true
}

// withoutNetworks returns c without the networks removed through the daemon. The slice
// is copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withoutNetworks(gone map[string]bool) FakeConfig {
	if len(gone) == 0 {
		return c
	}
	kept := make([]FakeNetwork, 0, len(c.Networks))
	for _, n := range c.Networks {
		if !gone[n.ID] {
			kept = append(kept, n)
		}
	}
	c.Networks = kept
	return c
}

// resolveNetwork finds a network the way a real daemon does: by its full ID, its name or
// an ID prefix.
func (c FakeConfig) resolveNetwork(ref string) (FakeNetwork, bool) {
	for _, n := range c.Networks {
		if n.ID == ref || n.Name == ref || (len(ref) >= 4 && strings.HasPrefix(n.ID, ref)) {
			return n, true
		}
	}
	return FakeNetwork{}, false
}

// serveNetworkInspect answers GET /networks/{id}: the network with its addressing,
// options and labels, the containers attached to it and, for a swarm-scoped network on
// a manager asked with verbose, the services whose specs mention it with a task for each
// one that runs. An unknown network is the 404 a real daemon gives.
//
// The Status follows Engine 29's getNetwork. A node lists its own networks first, so a
// reference equal to a network's full ID gets the node's own count (SetNetworkLocalUsage)
// when it has one. A name or ID prefix gets the swarm's count (SetNetworkUsage), or no
// Status when SetNetworkNoSwarmStatus is on; with no swarm count it falls back to the
// local one, as a node that cannot ask the swarm does. A network with local usage lists
// this node's load-balancer endpoint lb-<name> in Containers for every reference.
func (f *Fake) serveNetworkInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := networkPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	fn, ok := cfg.resolveNetwork(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "network " + m[1] + " not found"})
		return true
	}
	out := network.Inspect{
		Network: network.Network{
			ID: fn.ID, Name: fn.Name, Driver: fn.Driver, Scope: fn.Scope, Created: fn.Created,
			Labels: fn.Labels, Options: fn.Options, Internal: fn.Internal, Attachable: fn.Attachable,
			Ingress: fn.Ingress, EnableIPv6: fn.IPv6, EnableIPv4: true,
		},
		Containers: map[string]network.EndpointResource{},
	}
	for i, sub := range fn.Subnets {
		c := network.IPAMConfig{Subnet: netip.MustParsePrefix(sub)}
		if i < len(fn.Gateways) && fn.Gateways[i] != "" {
			c.Gateway = netip.MustParseAddr(fn.Gateways[i])
		}
		out.IPAM.Config = append(out.IPAM.Config, c)
	}
	for _, c := range cfg.Containers {
		for name, ep := range c.Networks {
			if ep.NetworkID != fn.ID && (ep.NetworkID != "" || name != fn.Name) {
				continue
			}
			er := network.EndpointResource{Name: strings.TrimPrefix(firstOf(c.Names, c.ID), "/")}
			if ep.IPv4 != "" {
				er.IPv4Address = netip.PrefixFrom(netip.MustParseAddr(ep.IPv4), 24)
			}
			if ep.IPv6 != "" {
				er.IPv6Address = netip.PrefixFrom(netip.MustParseAddr(ep.IPv6), 64)
			}
			if ep.MAC != "" {
				_ = er.MacAddress.UnmarshalText([]byte(ep.MAC))
			}
			out.Containers[c.ID] = er
		}
	}
	if fn.Scope == "swarm" && cfg.swarmManager() && r.URL.Query().Get("verbose") == "true" {
		out.Services = map[string]network.ServiceInfo{}
		for _, s := range cfg.ServiceRows {
			if !slices.ContainsFunc(s.Networks, func(n string) bool { return n == fn.ID || n == fn.Name }) {
				continue
			}
			info := network.ServiceInfo{Ports: []string{}, Tasks: []network.Task{}}
			for i := range s.Running {
				info.Tasks = append(info.Tasks, network.Task{Name: fmt.Sprintf("%s.%d", s.Name, i+1)})
			}
			out.Services[s.Name] = info
		}
	}
	f.mu.Lock()
	swarmUsage, localUsage, noSwarm := f.netUsage[fn.Name], f.netLocalUsage[fn.Name], f.netNoSwarmStatus[fn.Name]
	f.mu.Unlock()
	var usage []FakeSubnetUsage
	switch {
	case len(localUsage) > 0 && m[1] == fn.ID:
		usage = localUsage
	case noSwarm:
	case len(swarmUsage) > 0:
		usage = swarmUsage
	default:
		usage = localUsage
	}
	if len(localUsage) > 0 {
		out.Containers["lb-"+fn.Name] = network.EndpointResource{Name: fn.Name + "-endpoint"}
	}
	if len(usage) > 0 {
		subnets := network.SubnetStatuses{}
		for _, u := range usage {
			subnets[netip.MustParsePrefix(u.Subnet)] = network.SubnetStatus{IPsInUse: u.InUse, DynamicIPsAvailable: u.Available}
		}
		out.Status = &network.Status{IPAM: network.IPAMStatus{Subnets: subnets}}
	}
	writeJSON(w, http.StatusOK, out)
	return true
}

// FakeSubnetUsage is one subnet of a network inspect's Status.IPAM: the addresses in use
// and the dynamic addresses still available. Available 0 is a subnet that is full.
type FakeSubnetUsage struct {
	Subnet           string
	InUse, Available uint64
}

// SetNetworkUsage is what an Engine 29 manager reports for the network called name from
// now on, in its inspect Status.IPAM, for a network seeded in FakeConfig and one the
// client created alike. A network never given usage (or given none here) answers with no
// Status, as Engine 24 to 28 do.
func (f *Fake) SetNetworkUsage(name string, usage ...FakeSubnetUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(usage) == 0 {
		delete(f.netUsage, name)
		return
	}
	if f.netUsage == nil {
		f.netUsage = map[string][]FakeSubnetUsage{}
	}
	f.netUsage[name] = slices.Clone(usage)
}

// SetNetworkLocalUsage is what this node counts for its own instance of the network called
// name from now on, which a node holds while a task or a load-balancer endpoint of the
// network runs on it. Engine 29 answers that count for an inspect by the network's full ID,
// even with scope=swarm (moby getNetwork: "If a network appears both in swarm and local,
// assume it is in local first"). A name or ID prefix gets the swarm's count (SetNetworkUsage).
// A network with local usage and no swarm count answers the local one for every reference,
// as a node that cannot ask the swarm does. The network also lists this node's load-balancer
// endpoint lb-<name> in Containers. No usage removes it.
func (f *Fake) SetNetworkLocalUsage(name string, usage ...FakeSubnetUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(usage) == 0 {
		delete(f.netLocalUsage, name)
		return
	}
	if f.netLocalUsage == nil {
		f.netLocalUsage = map[string][]FakeSubnetUsage{}
	}
	f.netLocalUsage[name] = slices.Clone(usage)
}

// SetNetworkNoSwarmStatus makes the swarm answer the network called name with no Status: a
// name or ID prefix gets none, while a full ID still gets the SetNetworkLocalUsage count. It is
// how a test reaches a reader's no-swarm-Status branch. Off removes it.
func (f *Fake) SetNetworkNoSwarmStatus(name string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !on {
		delete(f.netNoSwarmStatus, name)
		return
	}
	if f.netNoSwarmStatus == nil {
		f.netNoSwarmStatus = map[string]bool{}
	}
	f.netNoSwarmStatus[name] = true
}

// networkTargetsByID returns the network targets of a service write with each one that names
// a network the daemon has as that network's ID, as a swarm manager stores them. The rest stay
// as given. in is not written to.
func (c FakeConfig) networkTargetsByID(in []swarm.NetworkAttachmentConfig) []swarm.NetworkAttachmentConfig {
	if in == nil {
		return nil
	}
	out := slices.Clone(in)
	for i := range out {
		if n, ok := c.resolveNetwork(out[i].Target); ok {
			out[i].Target = n.ID
		}
	}
	return out
}

func firstOf(names []string, fallback string) string {
	if len(names) > 0 {
		return names[0]
	}
	return fallback
}

// networkConnectPath matches POST /networks/{id}/connect.
var networkConnectPath = regexp.MustCompile(`^/networks/([^/]+)/connect$`)

// serveNetworkConnect answers POST /networks/{id}/connect. An unknown network or
// container is the 404 a real daemon gives; a swarm-scoped network that is not
// attachable is the 403 "not manually attachable" refusal; a container already on the
// network is the 403 "endpoint ... already exists" one. Otherwise the container is
// attached, with the addresses and the alias the body asked for, and every list and
// inspect shows it from then on. The body is kept for NetworkConnects and the write is
// recorded whatever the answer.
func (f *Fake) serveNetworkConnect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := networkConnectPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	var req network.ConnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	fn, ok := cfg.resolveNetwork(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "network " + m[1] + " not found"})
		return true
	}
	fc, ok := cfg.resolveContainer(req.Container)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + req.Container})
		return true
	}
	if fn.Scope == "swarm" && !fn.Attachable {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Could not attach to network " + fn.ID + ": rpc error: code = PermissionDenied desc = network " + fn.Name + " not manually attachable"})
		return true
	}
	if ep, on := fc.Networks[fn.Name]; on && (ep.NetworkID == "" || ep.NetworkID == fn.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "endpoint with name " + strings.TrimPrefix(firstOf(fc.Names, fc.ID), "/") + " already exists in network " + fn.Name})
		return true
	}
	ep := FakeEndpoint{NetworkID: fn.ID, IPv4: "10.99.0.2"}
	if req.EndpointConfig != nil {
		ep.Aliases = slices.Clone(req.EndpointConfig.Aliases)
		if ipam := req.EndpointConfig.IPAMConfig; ipam != nil {
			if ipam.IPv4Address.IsValid() {
				ep.IPv4 = ipam.IPv4Address.String()
			}
			if ipam.IPv6Address.IsValid() {
				ep.IPv6 = ipam.IPv6Address.String()
			}
		}
	}
	f.mu.Lock()
	if f.epChanges == nil {
		f.epChanges = map[[2]string]*FakeEndpoint{}
	}
	f.epChanges[[2]string{fc.ID, fn.Name}] = &ep
	f.netConnects = append(f.netConnects, req)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	return true
}

// networkDisconnectPath matches POST /networks/{id}/disconnect.
var networkDisconnectPath = regexp.MustCompile(`^/networks/([^/]+)/disconnect$`)

// serveNetworkDisconnect answers POST /networks/{id}/disconnect. An unknown network or
// container is the 404 a real daemon gives, and a container that is not on the network is
// the 500 "is not connected" a real engine gives. Otherwise the container leaves the network in every
// list and inspect. The body, force included, is kept for NetworkDisconnects and the
// write is recorded whatever the answer.
func (f *Fake) serveNetworkDisconnect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost {
		return false
	}
	m := networkDisconnectPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	var req network.DisconnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request body: " + err.Error()})
		return true
	}
	fn, ok := cfg.resolveNetwork(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "network " + m[1] + " not found"})
		return true
	}
	fc, ok := cfg.resolveContainer(req.Container)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + req.Container})
		return true
	}
	if ep, on := fc.Networks[fn.Name]; !on || (ep.NetworkID != "" && ep.NetworkID != fn.ID) {
		// A real engine answers this with a server error, not a refusal.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "container " + fc.ID + " is not connected to network " + fn.Name})
		return true
	}
	f.mu.Lock()
	if f.epChanges == nil {
		f.epChanges = map[[2]string]*FakeEndpoint{}
	}
	f.epChanges[[2]string{fc.ID, fn.Name}] = nil
	f.netDisconnects = append(f.netDisconnects, req)
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
	return true
}

// NetworkDisconnects returns the body of every network disconnect the daemon accepted,
// in order, as the daemon decoded it.
func (f *Fake) NetworkDisconnects() []network.DisconnectRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.netDisconnects)
}

// NetworkConnects returns the body of every network connect the daemon accepted, in
// order, as the daemon decoded it.
func (f *Fake) NetworkConnects() []network.ConnectRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.netConnects)
}

// withEndpoints returns c with the attachments the connects and disconnects left: a
// connected container has the endpoint in its networks, a disconnected one lacks it.
// The containers and their maps are copied, so the Fake's own configuration is not
// touched.
func (c FakeConfig) withEndpoints(changes map[[2]string]*FakeEndpoint) FakeConfig {
	if len(changes) == 0 || len(c.Containers) == 0 {
		return c
	}
	cs := make([]FakeContainer, 0, len(c.Containers))
	for _, fc := range c.Containers {
		nets := maps.Clone(fc.Networks)
		changed := false
		for key, ep := range changes {
			if key[0] != fc.ID {
				continue
			}
			if nets == nil {
				nets = map[string]FakeEndpoint{}
			}
			changed = true
			if ep == nil {
				delete(nets, key[1])
			} else {
				nets[key[1]] = *ep
			}
		}
		if changed {
			fc.Networks = nets
		}
		cs = append(cs, fc)
	}
	c.Containers = cs
	return c
}

// filterNetworksByLabel keeps the networks whose labels pass the request's label filter.
func filterNetworksByLabel(in []network.Summary, rawFilters string) []network.Summary {
	keep := labelFilter(rawFilters)
	out := make([]network.Summary, 0, len(in))
	for _, n := range in {
		if keep(n.Labels) {
			out = append(out, n)
		}
	}
	return out
}
