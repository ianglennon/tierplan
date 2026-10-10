// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/network"
)

var _ = registerFakeHandler((*Fake).serveNetworkCreate)
var _ = registerFakeHandler((*Fake).serveNetworkRemove)
var _ = registerFakeHandler((*Fake).serveNetworkInspect)

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
	fn := FakeNetwork{ID: newFakeNetworkID(seq), Name: req.Name, Driver: req.Driver, Scope: scope, Created: time.Now().UTC()}
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

// newFakeNetworkID is a 64 character hex ID like a daemon's, made from n.
func newFakeNetworkID(n int) string { return fmt.Sprintf("%064x", 0xbeef0000+n) }

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
	writeJSON(w, http.StatusOK, out)
	return true
}

func firstOf(names []string, fallback string) string {
	if len(names) > 0 {
		return names[0]
	}
	return fallback
}
