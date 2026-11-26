// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// SystemProber is the real Prober (A-05): an AF_PACKET socket for the ARP probes and a raw IPPROTO_VRRP socket
// joined to 224.0.0.18 for the adverts. It uses the standard library's syscall package only and needs
// CAP_NET_RAW, which the service already adds for keepalived. Every socket has a receive deadline, every read is
// bounded to a few hundred milliseconds so a stop is seen at once, and every error is one of the closed words
// below, never the system's own text.
type SystemProber struct{}

// The errors the prober returns. Their text is shown on standard error, so none carries anything but a step.
var (
	errProbeInterface = errors.New("interface not usable")
	errProbeSocket    = errors.New("cannot open the socket")
	errProbeBind      = errors.New("cannot bind the socket")
	errProbeSend      = errors.New("cannot send the probe")
	errProbeReceive   = errors.New("cannot read the socket")
	errProbeGroup     = errors.New("cannot join the multicast group")
)

// readSlice is the longest one read waits, so the context is looked at about this often.
const readSlice = 100 * time.Millisecond

// htons is the 16-bit value in network byte order as the kernel's socket address wants it in this host's
// byte order.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

// recvWithin reads one datagram from fd, waiting at most wait. ok is false when nothing came in time.
func recvWithin(fd int, buf []byte, wait time.Duration) (n int, ok bool, err error) {
	// A zero timeout would block forever.
	wait = max(wait, time.Millisecond)
	tv := syscall.NsecToTimeval(int64(wait))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return 0, false, errProbeReceive
	}
	n, _, err = syscall.Recvfrom(fd, buf, 0)
	switch {
	case err == nil:
		return n, true, nil
	case errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR):
		return 0, false, nil
	}
	return 0, false, errProbeReceive
}

// ARPProbe sends n probes, each waiting arpWait for a frame in which another machine claims addr.
func (SystemProber) ARPProbe(ctx context.Context, iface string, addr netip.Addr, n int) (bool, error) {
	nic, err := net.InterfaceByName(iface)
	if err != nil || len(nic.HardwareAddr) != 6 || !addr.Is4() {
		return false, errProbeInterface
	}
	proto := htons(syscall.ETH_P_ARP)
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return false, errProbeSocket
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: proto, Ifindex: nic.Index}); err != nil {
		return false, errProbeBind
	}
	frame := arpProbeFrame(nic.HardwareAddr, addr)
	to := &syscall.SockaddrLinklayer{Protocol: proto, Ifindex: nic.Index, Halen: 6, Addr: [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}
	buf := make([]byte, 1536)
	for range n {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := syscall.Sendto(fd, frame, 0, to); err != nil {
			return false, errProbeSend
		}
		end := time.Now().Add(arpWait) // wall-clock: a socket deadline, not a schedule
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			remain := time.Until(end)
			if remain <= 0 {
				break
			}
			got, ok, err := recvWithin(fd, buf, min(remain, readSlice))
			if err != nil {
				return false, err
			}
			if ok && claimsAddress(buf[:got], addr, nic.HardwareAddr) {
				return true, nil
			}
		}
	}
	return false, nil
}

// VRRPListen joins 224.0.0.18 on iface and reads VRRP adverts for d, and reports whether one carried routerID.
func (SystemProber) VRRPListen(ctx context.Context, iface string, routerID int, d time.Duration) (bool, error) {
	nic, err := net.InterfaceByName(iface)
	if err != nil {
		return false, errProbeInterface
	}
	local, ok := firstIPv4(nic)
	if !ok {
		return false, errProbeInterface
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, vrrpProtocol)
	if err != nil {
		return false, errProbeSocket
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface); err != nil {
		return false, errProbeBind
	}
	mreq := &syscall.IPMreq{Multiaddr: [4]byte{224, 0, 0, 18}, Interface: local}
	if err := syscall.SetsockoptIPMreq(fd, syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq); err != nil {
		return false, errProbeGroup
	}
	buf := make([]byte, 1536)
	end := time.Now().Add(d) // wall-clock: a socket deadline, not a schedule
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remain := time.Until(end)
		if remain <= 0 {
			return false, nil
		}
		got, ok, err := recvWithin(fd, buf, min(remain, readSlice))
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if id, isAdvert := vrrpAdvertRouter(buf[:got]); isAdvert && id == routerID {
			return true, nil
		}
	}
}

// firstIPv4 is the first IPv4 address of the interface, which the group is joined through.
func firstIPv4(nic *net.Interface) ([4]byte, bool) {
	addrs, err := nic.Addrs()
	if err != nil {
		return [4]byte{}, false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if v4 := ipn.IP.To4(); v4 != nil {
				return [4]byte(v4), true
			}
		}
	}
	return [4]byte{}, false
}
