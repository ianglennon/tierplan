// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The address-in-use check (D-11, A-05). keepalived claims the address and sends its gratuitous ARP at the
// instant of the MASTER transition, before the helper hears of it, so the check runs before keepalived starts.
// If another machine answers for the address and no VRRP peer with this router ID is advertising, the address is
// in use: the helper writes a flag file that makes the proxy check fail, so keepalived keeps the node in FAULT
// and never claims it, and it re-checks until the other machine is gone.
const (
	// AddressInUseFlag exists while another machine holds the address. `tierplan-ingress probe` exits 1 while it
	// does, whatever the proxy says.
	AddressInUseFlag = "/run/tierplan-vip/address-in-use"
	// AddressRecheck is the time between two checks while the flag exists.
	AddressRecheck = 12 * time.Second

	// arpProbes is how many probes one check sends; arpWait is how long each waits for an answer.
	arpProbes = 3
	arpWait   = 300 * time.Millisecond
	// vrrpListen is how long the listener waits for an advert: the holder sends one every second.
	vrrpListen = 3 * time.Second
	// vrrpProtocol is the IP protocol number of VRRP.
	vrrpProtocol = 112
)

// The three results of one check.
const (
	// addrFree: nobody answered for the address.
	addrFree = "free"
	// addrPeer: somebody answered and a VRRP router with this router ID is advertising, so the holder is one of
	// ours (or a foreign router the counters report, A-04).
	addrPeer = "peer"
	// addrInUse: somebody answered and no such router is advertising.
	addrInUse = "in_use"
)

// decide is the whole decision of the check (D-11): answered says whether an ARP probe was answered, seen
// whether an advert with this router ID was heard.
func decide(answered, seen bool) string {
	switch {
	case !answered:
		return addrFree
	case seen:
		return addrPeer
	}
	return addrInUse
}

// Prober is what the check needs from the network. The real one is in arp_linux.go; a test gives a fake. Every
// error it returns names a step in a closed set of words and never carries the system's text.
type Prober interface {
	// ARPProbe sends n ARP probes (sender 0.0.0.0) for addr on iface and reports whether any was answered.
	ARPProbe(ctx context.Context, iface string, addr netip.Addr, n int) (answered bool, err error)
	// VRRPListen listens on iface for up to d for a VRRP advert of the router ID.
	VRRPListen(ctx context.Context, iface string, routerID int, d time.Duration) (seen bool, err error)
}

// addressChecker runs the check and keeps the flag file and the fault line in step with its result.
type addressChecker struct {
	d     RunDeps
	p     Prober
	cfg   Config
	iface string
	em    *Emitter
	flag  string
	// inUse is true while the flag exists and the fault is written. A flag is written only when the fault line
	// was, and the fault only counts when the flag was, so the two never disagree.
	inUse bool
	// lastErr is the last check error shown, so one that repeats every 12 s is one line.
	lastErr string
}

func newAddressChecker(d RunDeps, cfg Config, iface string, em *Emitter) *addressChecker {
	flag := d.FlagPath
	if flag == "" {
		flag = AddressInUseFlag
	}
	return &addressChecker{d: d, p: d.Prober, cfg: cfg, iface: iface, em: em, flag: flag}
}

// clearStale removes a flag a previous run left, which would hold this node in FAULT for a check that has not
// run yet.
func (c *addressChecker) clearStale() {
	if err := c.d.RemoveFile(c.flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.warn("remove the address flag")
	}
}

// check runs one check and acts on its result. A check that cannot finish (a socket that cannot be opened, the
// stop) changes nothing: the node is never held out, and never let in, by a check that did not answer.
func (c *addressChecker) check(ctx context.Context) {
	answered, err := c.p.ARPProbe(ctx, c.iface, c.cfg.Address.Addr(), arpProbes)
	if err != nil {
		c.failed(ctx, err)
		return
	}
	seen := false
	if answered {
		if seen, err = c.p.VRRPListen(ctx, c.iface, c.cfg.RouterID, vrrpListen); err != nil {
			c.failed(ctx, err)
			return
		}
	}
	c.lastErr = ""
	switch decide(answered, seen) {
	case addrInUse:
		if c.inUse {
			return
		}
		if err := c.d.MkdirAll(filepath.Dir(c.flag), 0o755); err != nil { //nolint:gosec // a directory under /run the probe reads
			c.warn("create the address flag directory")
			return
		}
		if err := c.d.WriteFile(c.flag, []byte("address in use\n"), 0o600); err != nil {
			c.warn("write the address flag")
			return
		}
		c.inUse = true
		c.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: ingressbundle.VIPReasonAddressInUse, Active: true})
	default:
		if !c.inUse {
			return
		}
		if err := c.d.RemoveFile(c.flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.warn("remove the address flag")
			return
		}
		c.inUse = false
		c.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: ingressbundle.VIPReasonAddressInUse})
	}
}

// recheck repeats the check every AddressRecheck while the flag exists and ends when it is gone or ctx ends. It
// does not run while the flag is absent: a node that holds the address answers for it itself, and its own
// adverts would read as a peer's.
func (c *addressChecker) recheck(ctx context.Context) {
	for c.inUse {
		select {
		case <-ctx.Done():
			return
		case <-c.d.Clock.After(AddressRecheck):
		}
		c.check(ctx)
	}
}

// failed shows a check error once on standard error. A stop that ended the check is not an error.
func (c *addressChecker) failed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	c.warn("address check: " + err.Error())
}

func (c *addressChecker) warn(text string) {
	if text == c.lastErr {
		return
	}
	c.lastErr = text
	fmt.Fprintln(c.d.Stderr, "tierplan-ingress: vip-entrypoint:", text)
}

// The frames the real prober sends and reads. They are here, apart from the sockets, so a test can check them
// without a privilege. Nothing read from the network is written anywhere: a frame yields a yes or a router ID.
const (
	etherTypeARP = 0x0806
	// arpFrameLen is the length of an Ethernet frame holding an ARP request, padded to the 60-byte minimum.
	arpFrameLen = 60
	arpHeader   = 14 + 28
)

// arpProbeFrame builds the ARP probe of RFC 5227: a broadcast request from mac with sender address 0.0.0.0 for
// addr.
func arpProbeFrame(mac net.HardwareAddr, addr netip.Addr) []byte {
	f := make([]byte, arpFrameLen)
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[6:12], mac)
	f[12], f[13] = etherTypeARP>>8, etherTypeARP&0xff
	f[14], f[15] = 0, 1 // Ethernet
	f[16], f[17] = 0x08, 0x00
	f[18], f[19] = 6, 4
	f[20], f[21] = 0, 1 // request
	copy(f[22:28], mac)
	// bytes 28 to 31: the sender address 0.0.0.0; 32 to 37: the target hardware address, unknown.
	a := addr.As4()
	copy(f[38:42], a[:])
	return f
}

// claimsAddress reports whether the Ethernet frame is an ARP packet in which a machine other than own says it
// holds addr: a reply to a probe, or the holder's own announcement. Only the opcode, the sender's addresses and
// the lengths are read.
func claimsAddress(frame []byte, addr netip.Addr, own net.HardwareAddr) bool {
	if len(frame) < arpHeader || !addr.Is4() {
		return false
	}
	if frame[12] != etherTypeARP>>8 || frame[13] != etherTypeARP&0xff {
		return false
	}
	if frame[14] != 0 || frame[15] != 1 || frame[16] != 0x08 || frame[17] != 0x00 || frame[18] != 6 || frame[19] != 4 {
		return false
	}
	if op := int(frame[20])<<8 | int(frame[21]); op != 1 && op != 2 {
		return false
	}
	a := addr.As4()
	if [4]byte(frame[28:32]) != a {
		return false
	}
	return net.HardwareAddr(frame[22:28]).String() != own.String()
}

// vrrpAdvertRouter reads a raw IPv4 packet and returns the router ID of the VRRP advert it carries.
func vrrpAdvertRouter(pkt []byte) (int, bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return 0, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+8 || pkt[9] != vrrpProtocol {
		return 0, false
	}
	v := pkt[ihl:]
	// Version 2 or 3, type 1 (advert).
	if ver := v[0] >> 4; (ver != 2 && ver != 3) || v[0]&0x0f != 1 {
		return 0, false
	}
	return int(v[1]), true
}
