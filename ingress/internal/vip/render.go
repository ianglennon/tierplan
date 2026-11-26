// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"fmt"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The fixed paths and names of the helper's keepalived. They are constants because the container image fixes
// them: /run is a tmpfs the helper writes into (A-01).
const (
	// ConfPath is where the helper writes the configuration.
	ConfPath = "/run/keepalived.conf"
	// FIFOPath is the notify FIFO keepalived creates and the helper reads state changes from.
	FIFOPath = "/run/keepalived.fifo"
	// ProbeCommand is the vrrp_script: the helper's own probe subcommand (D-12).
	ProbeCommand = "/usr/local/bin/tierplan-ingress probe"
	// InstanceName is the vrrp_instance name, which appears in every FIFO line.
	InstanceName = "TIERPLAN_VIP"
)

// Render returns the keepalived configuration for one node. iface is the name Discover returned.
//
// Every node of an environment gets the same text apart from the interface: the mode is multicast, so there
// is no peer list and a node joining or leaving changes nothing on the others (D-02). Every node starts as
// BACKUP at one priority with nopreempt, so a recovered node never takes the address back (D-04).
//
// The key appears only as a file reference (A-03): keepalived reads the swarm secret itself, and Config holds
// no key, so nothing here can print it. The advertisements are authenticated in enforce mode with a time-based
// replay check whose window is ingressbundle.VIPAuthWindow (A-04).
//
// Render trusts cfg: Load has validated it.
func Render(cfg Config, iface string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `global_defs {
    enable_script_security
    script_user root
    vrrp_garp_master_refresh 60
    vrrp_notify_fifo %s
}
vrrp_script chk_proxy {
    script "%s"
    interval 1
    timeout 1
    fall 2
    rise 2
}
vrrp_instance %s {
    state BACKUP
    interface %s
    virtual_router_id %d
    priority 100
    advert_int 1
    nopreempt
    auth_hmac {
        key 1 file:%s
        active_key 1
        mode enforce
        anti_replay time
        time_window %d
    }
    virtual_ipaddress {
        %s dev %s
    }
    track_script { chk_proxy }
}
`, FIFOPath, ProbeCommand, InstanceName, iface, cfg.RouterID, cfg.KeyFile,
		int(ingressbundle.VIPAuthWindow.Seconds()), cfg.Address, iface)
	return b.Bytes()
}
