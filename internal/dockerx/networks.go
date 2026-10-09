// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// The only drivers a network can be created with. An ingress, config-only or
// config-from network is not one of the shapes NetworkCreateSpec can express, and
// a driver outside this list is refused before anything is sent (T-04-46).
const (
	DriverBridge  = "bridge"
	DriverOverlay = "overlay"
	DriverMacvlan = "macvlan"
	DriverIPvlan  = "ipvlan"

	// ScopeSwarm and ScopeLocal are the scopes of a network: cluster wide, or one
	// engine's.
	ScopeSwarm = "swarm"
	ScopeLocal = "local"
)

// ErrDriverNotAllowed means the driver of a network create is not bridge,
// overlay, macvlan or ipvlan.
var ErrDriverNotAllowed = errors.New("dockerx: network driver is not allowed")

// ErrAddressing means a subnet, gateway or IP range of a network create is not
// valid, or the gateway or range is not inside the subnet.
var ErrAddressing = errors.New("dockerx: network addressing is not valid")

// NetworkScope is the scope a network of the given driver is created with: overlay
// networks are swarm scoped, every other allowed driver is local to one engine.
func NetworkScope(driver string) string {
	if driver == DriverOverlay {
		return ScopeSwarm
	}
	return ScopeLocal
}

// NetworkCreateSpec is a network to create. It can say nothing about ingress,
// config-only or config-from networks, so none can be made through it.
type NetworkCreateSpec struct {
	Name string
	// Driver is bridge, overlay, macvlan or ipvlan.
	Driver string
	// Scope is swarm for a network created through a manager as a cluster network,
	// and empty otherwise: a local network is left for the engine to scope, as the
	// docker CLI does.
	Scope                      string
	Attachable, Internal, IPv6 bool
	Labels, Options            map[string]string
	// Subnet and IPRange are in CIDR form and Gateway is an address. Empty leaves
	// the choice to the engine.
	Subnet, Gateway, IPRange string
}

// NetworkCreate creates the network once and never retries: a create whose answer
// was lost may have happened (P2 D-42). It returns the ID and the engine's warning,
// if it gave one.
func (c *Client) NetworkCreate(ctx context.Context, spec NetworkCreateSpec) (id, warning string, err error) {
	switch spec.Driver {
	case DriverBridge, DriverOverlay, DriverMacvlan, DriverIPvlan:
	default:
		return "", "", fmt.Errorf("%w: %q", ErrDriverNotAllowed, spec.Driver)
	}
	ipam, err := ipamOf(spec)
	if err != nil {
		return "", "", err
	}
	opts := client.NetworkCreateOptions{
		Driver: spec.Driver, Scope: spec.Scope, IPAM: ipam,
		Internal: spec.Internal, Attachable: spec.Attachable,
		Options: spec.Options, Labels: spec.Labels,
	}
	if spec.IPv6 {
		on := true
		opts.EnableIPv6 = &on
	}
	res, err := c.cur().NetworkCreate(ctx, spec.Name, opts)
	if err != nil {
		return "", "", fmt.Errorf("create network on %s: %w", c.t.Describe(), err)
	}
	return res.ID, strings.Join(res.Warning, "; "), nil
}

// ipamOf builds the address management of a create from its CIDR fields, or nil
// when none is set.
func ipamOf(spec NetworkCreateSpec) (*network.IPAM, error) {
	if spec.Subnet == "" && spec.Gateway == "" && spec.IPRange == "" {
		return nil, nil
	}
	var cfg network.IPAMConfig
	if spec.Subnet != "" {
		p, err := netip.ParsePrefix(spec.Subnet)
		if err != nil {
			return nil, fmt.Errorf("%w: subnet %q", ErrAddressing, spec.Subnet)
		}
		cfg.Subnet = p.Masked()
	}
	if spec.Gateway != "" {
		a, err := netip.ParseAddr(spec.Gateway)
		if err != nil {
			return nil, fmt.Errorf("%w: gateway %q", ErrAddressing, spec.Gateway)
		}
		cfg.Gateway = a
	}
	if spec.IPRange != "" {
		p, err := netip.ParsePrefix(spec.IPRange)
		if err != nil {
			return nil, fmt.Errorf("%w: IP range %q", ErrAddressing, spec.IPRange)
		}
		cfg.IPRange = p.Masked()
	}
	return &network.IPAM{Config: []network.IPAMConfig{cfg}}, nil
}
