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
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	if cfg.RouterID, err = parseInt(ingressbundle.EnvVIPRouterID, getenv(ingressbundle.EnvVIPRouterID), 1, 255); err != nil {
		return Config{}, err
	}
	if cfg.ProbePort, err = parseInt(ingressbundle.EnvVIPProbePort, getenv(ingressbundle.EnvVIPProbePort), 1, 65535); err != nil {
		return Config{}, err
	}
	if cfg.KeyFile, err = parseKeyFile(getenv(ingressbundle.EnvVIPKeyFile)); err != nil {
		return Config{}, err
	}
	if cfg.NodeID, err = parseNodeID(getenv(ingressbundle.EnvVIPNodeID)); err != nil {
		return Config{}, err
	}
	if cfg.NodeName, err = parseNodeName(getenv(ingressbundle.EnvVIPNodeName)); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// maxNodeName is the longest node name the helper's event line carries; Linux host names are at most 64.
const maxNodeName = 64

var (
	// keyFilePath is an absolute path of plain characters. The path is written into keepalived.conf, where a
	// space, a quote, a brace or a comment mark would end the token and a line break would start a new
	// directive, so anything outside this set is refused instead of escaped (T-26-16).
	keyFilePath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)
	// nodeID is the shape of a swarm node ID: 25 lower-case alphanumerics.
	nodeID = regexp.MustCompile(`^[a-z0-9]{25}$`)
)

func parseKeyFile(raw string) (string, error) {
	if raw == "" {
		return "", errNotSet(ingressbundle.EnvVIPKeyFile)
	}
	if !keyFilePath.MatchString(raw) {
		return "", fmt.Errorf("%s is not an absolute path of letters, digits and . _ / -", ingressbundle.EnvVIPKeyFile)
	}
	return raw, nil
}

func parseNodeID(raw string) (string, error) {
	if raw == "" {
		return "", errNotSet(ingressbundle.EnvVIPNodeID)
	}
	if !nodeID.MatchString(raw) {
		return "", fmt.Errorf("%s is not a swarm node ID", ingressbundle.EnvVIPNodeID)
	}
	return raw, nil
}

func parseNodeName(raw string) (string, error) {
	if raw == "" {
		return "", errNotSet(ingressbundle.EnvVIPNodeName)
	}
	if utf8.RuneCountInString(raw) > maxNodeName || !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s is not a host name of at most %d characters", ingressbundle.EnvVIPNodeName, maxNodeName)
	}
	return raw, nil
}

func errNotSet(name string) error { return fmt.Errorf("%s is not set", name) }

func parseAddress(raw string) (netip.Prefix, error) {
	if raw == "" {
		return netip.Prefix{}, errNotSet(ingressbundle.EnvVIPAddress)
	}
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s is not an IPv4 address with a prefix", ingressbundle.EnvVIPAddress)
	}
	return p, nil
}

func parseInt(name, raw string, lo, hi int) (int, error) {
	if raw == "" {
		return 0, errNotSet(name)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s is not a number from %d to %d", name, lo, hi)
	}
	return n, nil
}
