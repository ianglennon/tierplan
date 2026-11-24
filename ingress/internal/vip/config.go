// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package vip holds the pure parts of the keepalived helper: reading the environment the VIP service gives
// it (D-14), finding the one host interface in the virtual address's network (D-09), and rendering the
// keepalived configuration (D-02, D-04, D-12, A-03, A-04). It uses the standard library and the proto
// module only, and it never starts a process or opens the key.
package vip

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// Config is what the VIP service tells the helper. Every node of one environment gets the same values
// except NodeID and NodeName; the interface is not here because it is known only on the node (D-02).
type Config struct {
	// Address is the virtual address with its prefix, for example 192.168.1.50/24.
	Address netip.Prefix
	// RouterID is the VRRP router ID of the environment.
	RouterID int
	// ProbePort is the port the probe asks on 127.0.0.1.
	ProbePort int
	// KeyFile is the path of the swarm secret holding the auth_hmac key. Only the path is held: the key is
	// read by keepalived and by nothing here (A-03).
	KeyFile string
	// NodeID is the swarm node ID of this node.
	NodeID string
	// NodeName is this node's host name, for display.
	NodeName string
}

// Load reads the configuration. getenv looks up an environment variable, injected so tests need no real
// environment. Error text names a variable and never a value (T-26-16).
func Load(getenv func(string) string) (Config, error) {
	var cfg Config
	var err error
	if cfg.Address, err = parseAddress(getenv(ingressbundle.EnvVIPAddress)); err != nil {
		return Config{}, err
	}
	if cfg.RouterID, err = parseInt(ingressbundle.EnvVIPRouterID, getenv(ingressbundle.EnvVIPRouterID)); err != nil {
		return Config{}, err
	}
	if cfg.ProbePort, err = parseInt(ingressbundle.EnvVIPProbePort, getenv(ingressbundle.EnvVIPProbePort)); err != nil {
		return Config{}, err
	}
	if cfg.KeyFile = getenv(ingressbundle.EnvVIPKeyFile); cfg.KeyFile == "" {
		return Config{}, errNotSet(ingressbundle.EnvVIPKeyFile)
	}
	if cfg.NodeID = getenv(ingressbundle.EnvVIPNodeID); cfg.NodeID == "" {
		return Config{}, errNotSet(ingressbundle.EnvVIPNodeID)
	}
	if cfg.NodeName = getenv(ingressbundle.EnvVIPNodeName); cfg.NodeName == "" {
		return Config{}, errNotSet(ingressbundle.EnvVIPNodeName)
	}
	return cfg, nil
}

func errNotSet(name string) error { return fmt.Errorf("%s is not set", name) }

func parseAddress(raw string) (netip.Prefix, error) {
	if raw == "" {
		return netip.Prefix{}, errNotSet(ingressbundle.EnvVIPAddress)
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s is not an address with a prefix", ingressbundle.EnvVIPAddress)
	}
	return p, nil
}

func parseInt(name, raw string) (int, error) {
	if raw == "" {
		return 0, errNotSet(name)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s is not a number", name)
	}
	return n, nil
}
