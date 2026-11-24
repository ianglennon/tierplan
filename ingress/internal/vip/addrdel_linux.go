// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// RemoveAddr deletes the IPv4 address p (with its prefix length) from the interface named iface through one
// RTM_DELADDR request on a NETLINK_ROUTE socket, and reads the kernel's acknowledgement (A-15, D-10). It is the
// whole of the helper's address removal: the node's own stale address on an interface keepalived no longer
// manages, and nothing else. It needs NET_ADMIN, which the service adds, and it uses the standard library's
// syscall package only (the module holds no netlink library; syscall.NetlinkRIB is read-only).
func RemoveAddr(iface string, p netip.Prefix) error {
	if !p.Addr().Is4() {
		return errors.New("only an IPv4 address can be removed")
	}
	nic, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("find interface: %w", err)
	}
	const seq = 1
	return netlinkRequest(delAddrMessage(nic.Index, p, seq), seq)
}

// netlinkRequest sends one request on a NETLINK_ROUTE socket and returns the kernel's answer to it.
func netlinkRequest(msg []byte, seq uint32) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("open netlink socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("bind netlink socket: %w", err)
	}
	// A kernel that never answers must not stall the helper's start.
	tv := syscall.NsecToTimeval(int64(2 * time.Second))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return fmt.Errorf("set netlink timeout: %w", err)
	}
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("send netlink request: %w", err)
	}
	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return fmt.Errorf("read the acknowledgement: %w", err)
	}
	return parseAck(buf[:n], seq)
}

// align4 rounds n up to netlink's four-byte alignment.
func align4(n int) int { return (n + 3) &^ 3 }

// delAddrMessage builds an RTM_DELADDR request: a nlmsghdr, an ifaddrmsg for the interface and the prefix
// length, and the address as IFA_LOCAL and IFA_ADDRESS. All integers are in the host's byte order, as netlink
// requires.
func delAddrMessage(ifindex int, p netip.Prefix, seq uint32) []byte {
	const attrLen = syscall.SizeofRtAttr + 4 // an IPv4 address is four bytes, already aligned
	total := syscall.SizeofNlMsghdr + align4(syscall.SizeofIfAddrmsg) + 2*attrLen
	b := make([]byte, total)
	ne := binary.NativeEndian

	// nlmsghdr: length, type, flags, sequence, port (0: the kernel).
	ne.PutUint32(b[0:], uint32(total))
	ne.PutUint16(b[4:], syscall.RTM_DELADDR)
	ne.PutUint16(b[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK)
	ne.PutUint32(b[8:], seq)
	ne.PutUint32(b[12:], 0)

	// ifaddrmsg: family, prefix length, flags, scope, interface index.
	off := syscall.SizeofNlMsghdr
	b[off] = syscall.AF_INET
	b[off+1] = byte(p.Bits())
	ne.PutUint32(b[off+4:], uint32(ifindex))

	// The two attributes: length, type, the four address bytes.
	off += align4(syscall.SizeofIfAddrmsg)
	a4 := p.Addr().As4()
	for _, typ := range []uint16{syscall.IFA_LOCAL, syscall.IFA_ADDRESS} {
		ne.PutUint16(b[off:], attrLen)
		ne.PutUint16(b[off+2:], typ)
		copy(b[off+syscall.SizeofRtAttr:], a4[:])
		off += attrLen
	}
	return b
}

// parseAck reads the kernel's answer to a request with the sequence number seq: NLMSG_ERROR whose error is 0 is
// the acknowledgement, a negative one is -errno.
func parseAck(b []byte, seq uint32) error {
	ne := binary.NativeEndian
	for len(b) >= syscall.SizeofNlMsghdr {
		length := int(ne.Uint32(b[0:]))
		if length < syscall.SizeofNlMsghdr || length > len(b) {
			return errors.New("malformed netlink reply")
		}
		if ne.Uint16(b[4:]) == syscall.NLMSG_ERROR && ne.Uint32(b[8:]) == seq {
			if length < syscall.SizeofNlMsghdr+4 {
				return errors.New("short netlink error reply")
			}
			if code := int32(ne.Uint32(b[syscall.SizeofNlMsghdr:])); code != 0 {
				return fmt.Errorf("netlink: %w", syscall.Errno(-code))
			}
			return nil
		}
		b = b[align4(length):]
	}
	return errors.New("no acknowledgement in the netlink reply")
}
