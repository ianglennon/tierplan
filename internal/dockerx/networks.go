// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
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

// builtinNetworks are the networks the engine and the swarm make themselves. Their
// names are exactly these five (RESEARCH Pitfall 7).
var builtinNetworks = []string{"bridge", "host", "none", "ingress", "docker_gwbridge"}

// IsBuiltinNetwork reports whether a network is one the engine or the swarm made and
// that can never be deleted: one of the five built-in names, or any network that
// carries the ingress flag, whatever it is called.
func IsBuiltinNetwork(name string, ingress bool) bool {
	return ingress || slices.Contains(builtinNetworks, name)
}

// NetworkRemove removes the network id once and never retries: a remove whose answer
// was lost may have happened (P2 D-42). The caller checks that nothing uses the
// network; the engine's own refusal of a network with active endpoints is
// IsNetworkInUse.
func (c *Client) NetworkRemove(ctx context.Context, id string) error {
	if _, err := c.cur().NetworkRemove(ctx, id, client.NetworkRemoveOptions{}); err != nil {
		return fmt.Errorf("remove network on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// EndpointSpec is how a container joins a network: an extra DNS name and, optionally,
// fixed addresses. Anything else about an endpoint (links, driver options, MAC address)
// is not expressible, so it can never be sent (T-04-49).
type EndpointSpec struct {
	// Alias is another name other containers on the network can use; empty adds none.
	Alias string
	// IPv4 and IPv6 are addresses to give the container on the network; empty lets the
	// network choose.
	IPv4, IPv6 string
}

// NetworkConnect connects the container to the network once and never retries: a
// connect whose answer was lost may have happened (P2 D-42). Addresses are parsed
// before anything is sent: an IPv4 field must hold an IPv4 address and an IPv6 field an
// IPv6 one (ErrAddressing).
func (c *Client) NetworkConnect(ctx context.Context, networkID, containerID string, ep EndpointSpec) error {
	var cfg *network.EndpointSettings
	if ep.Alias != "" || ep.IPv4 != "" || ep.IPv6 != "" {
		cfg = &network.EndpointSettings{}
		if ep.Alias != "" {
			cfg.Aliases = []string{ep.Alias}
		}
		if ep.IPv4 != "" || ep.IPv6 != "" {
			ipam := &network.EndpointIPAMConfig{}
			if ep.IPv4 != "" {
				a, err := netip.ParseAddr(ep.IPv4)
				if err != nil || !a.Is4() {
					return fmt.Errorf("%w: IPv4 address %q", ErrAddressing, ep.IPv4)
				}
				ipam.IPv4Address = a
			}
			if ep.IPv6 != "" {
				a, err := netip.ParseAddr(ep.IPv6)
				if err != nil || !a.Is6() || a.Is4In6() {
					return fmt.Errorf("%w: IPv6 address %q", ErrAddressing, ep.IPv6)
				}
				ipam.IPv6Address = a
			}
			cfg.IPAMConfig = ipam
		}
	}
	if _, err := c.cur().NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: containerID, EndpointConfig: cfg}); err != nil {
		return fmt.Errorf("connect to network on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// IsRefusal reports whether err is the engine itself answering that it will not do what
// was asked (a bad parameter, a refusal, a missing object or a conflicting state), as
// opposed to a connection that failed. The words are DaemonText.
func IsRefusal(err error) bool {
	return cerrdefs.IsInvalidArgument(err) || cerrdefs.IsPermissionDenied(err) || cerrdefs.IsConflict(err) ||
		cerrdefs.IsNotFound(err) || cerrdefs.IsFailedPrecondition(err)
}

// DaemonText is the engine's own sentence in err, without the client's wrapping: what
// follows "Error response from daemon: ". An error without that marker is returned whole.
func DaemonText(err error) string {
	const marker = "Error response from daemon: "
	msg := err.Error()
	if i := strings.LastIndex(msg, marker); i >= 0 {
		return strings.TrimSpace(msg[i+len(marker):])
	}
	return strings.TrimSpace(msg)
}

// IsNetworkInUse reports whether err is the engine refusing to remove a network that
// something still uses. Older engines answer 409 and newer ones 403 with the words
// "has active endpoints"; a swarm manager says the network is in use by a service. A
// 403 that is an agent's policy refusal is not one of these.
func IsNetworkInUse(err error) bool {
	if err == nil {
		return false
	}
	if cerrdefs.IsConflict(err) {
		return true
	}
	msg := err.Error()
	if strings.Contains(msg, "agent_policy_denied") {
		return false
	}
	return strings.Contains(msg, "active endpoints") || strings.Contains(msg, "is in use by")
}

// IPAMRow is one address pool of a network: the subnet, the gateway and the range
// containers are given addresses from. A part the network does not set is empty.
type IPAMRow struct {
	Subnet, Gateway, IPRange string
}

// NetworkService is a swarm service on a swarm-scoped network, with the number of its
// tasks that have an endpoint on it.
type NetworkService struct {
	Name  string
	Tasks int
}

// NetworkDetail is one network as its detail page shows it. Which containers are
// attached is not here: the lists of every node say that, with the addresses.
type NetworkDetail struct {
	ID, Name, Driver, Scope string
	Internal, Attachable    bool
	IPv6, Ingress           bool
	Created                 time.Time
	// Labels and Options are never nil.
	Labels, Options map[string]string
	// IPAM is the network's address pools, in the engine's order. Never nil.
	IPAM []IPAMRow
	// Services are the services on the network by name, filled only when verbose was
	// asked for and the engine answered with them (a swarm-scoped network, read on a
	// manager). Never nil.
	Services []NetworkService
}

// NetworkDetail inspects one network and returns the typed detail and the raw
// document, for the Inspect tab. verbose asks a manager for the services on a
// swarm-scoped network too; a local network ignores it.
func (c *Client) NetworkDetail(ctx context.Context, id string, verbose bool) (NetworkDetail, []byte, error) {
	res, err := c.cur().NetworkInspect(ctx, id, client.NetworkInspectOptions{Verbose: verbose})
	if err != nil {
		return NetworkDetail{}, nil, fmt.Errorf("inspect network on %s: %w", c.t.Describe(), err)
	}
	in := res.Network
	d := NetworkDetail{
		ID: in.ID, Name: in.Name, Driver: in.Driver, Scope: in.Scope,
		Internal: in.Internal, Attachable: in.Attachable, IPv6: in.EnableIPv6, Ingress: in.Ingress,
		Created: in.Created, Labels: map[string]string{}, Options: map[string]string{},
		IPAM: []IPAMRow{}, Services: []NetworkService{},
	}
	for k, v := range in.Labels {
		d.Labels[k] = v
	}
	for k, v := range in.Options {
		d.Options[k] = v
	}
	for _, cfg := range in.IPAM.Config {
		var row IPAMRow
		if cfg.Subnet.IsValid() {
			row.Subnet = cfg.Subnet.String()
		}
		if cfg.Gateway.IsValid() {
			row.Gateway = cfg.Gateway.String()
		}
		if cfg.IPRange.IsValid() {
			row.IPRange = cfg.IPRange.String()
		}
		d.IPAM = append(d.IPAM, row)
	}
	for name, svc := range in.Services {
		d.Services = append(d.Services, NetworkService{Name: name, Tasks: len(svc.Tasks)})
	}
	slices.SortFunc(d.Services, func(a, b NetworkService) int { return strings.Compare(a.Name, b.Name) })
	return d, res.Raw, nil
}
