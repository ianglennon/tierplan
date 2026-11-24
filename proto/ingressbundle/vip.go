// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import "time"

// What the keepalived helper (the ingress module's vip subcommands) and the TierPlan server must agree on
// lives here, once (D-15). The helper is a separate module that cannot import internal/, and the server
// reads the helper's output from a node it does not control, so no second copy of any string exists: the
// env names the server writes into the VIP service, the event vocabulary the helper writes and the server
// parses, and the numbers both sides depend on.

// The ports and times of the virtual-IP health check and its authentication.
const (
	// ReadyPublishedPort is the host port the swarm proxy publishes its readiness endpoint (ReadyPort) on,
	// so the keepalived container, which runs in the host network namespace, can ask the node's own proxy
	// whether it is ready (D-13). It sits next to TierPlan's own 7480 and below the ephemeral range, so no
	// outgoing connection picks it by chance. It is visible on the LAN; the endpoint stays a bare status
	// code and carries no body.
	ReadyPublishedPort = 7483
	// VIPProbeTimeout is how long `tierplan-ingress probe` waits for the readiness endpoint (D-12). It is
	// below the one-second vrrp_script timeout, so a stuck endpoint makes the probe report its own failure
	// instead of keepalived killing it.
	VIPProbeTimeout = 800 * time.Millisecond
	// VIPAuthWindow is the auth_hmac time window (A-04): an advert whose trailer is older than this, by the
	// sender's clock, is refused as stale, so two nodes' clocks must agree to within it.
	VIPAuthWindow = 30 * time.Second
)

// The environment variables the VIP service sets for the helper. The server writes them and the helper
// reads them.
const (
	// EnvVIPAddress is the virtual address with its prefix, for example 192.168.1.250/24.
	EnvVIPAddress = "TIERPLAN_VIP_ADDRESS"
	// EnvVIPRouterID is the VRRP router ID (VRID) of the environment, 1 to 255.
	EnvVIPRouterID = "TIERPLAN_VIP_ROUTER_ID"
	// EnvVIPProbePort is the port the probe asks on 127.0.0.1, ReadyPublishedPort.
	EnvVIPProbePort = "TIERPLAN_VIP_PROBE_PORT"
	// EnvVIPKeyFile is the path of the swarm secret holding the auth_hmac key.
	EnvVIPKeyFile = "TIERPLAN_VIP_KEY_FILE" //nolint:gosec // the name of a variable, not a credential
	// EnvVIPNodeID is the swarm node ID of the node the helper runs on.
	EnvVIPNodeID = "TIERPLAN_VIP_NODE_ID"
	// EnvVIPNodeName is the node's host name, for display.
	EnvVIPNodeName = "TIERPLAN_VIP_NODE_NAME"
)
