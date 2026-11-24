// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
)

func TestDelAddrMessage(t *testing.T) {
	b := delAddrMessage(7, netip.MustParsePrefix("192.168.1.50/24"), 9)
	ne := binary.NativeEndian
	if got := int(ne.Uint32(b[0:])); got != len(b) || len(b)%4 != 0 {
		t.Fatalf("length field %d, message %d bytes: want them equal and aligned", got, len(b))
	}
	if got := ne.Uint16(b[4:]); got != syscall.RTM_DELADDR {
		t.Errorf("type = %d, want RTM_DELADDR", got)
	}
	if got := ne.Uint16(b[6:]); got != syscall.NLM_F_REQUEST|syscall.NLM_F_ACK {
		t.Errorf("flags = %#x, want REQUEST|ACK", got)
	}
	if ne.Uint32(b[8:]) != 9 {
		t.Errorf("sequence = %d, want 9", ne.Uint32(b[8:]))
	}
	ifa := b[syscall.SizeofNlMsghdr:]
	if ifa[0] != syscall.AF_INET || ifa[1] != 24 || ne.Uint32(ifa[4:]) != 7 {
		t.Errorf("ifaddrmsg family %d prefix %d index %d, want AF_INET 24 7", ifa[0], ifa[1], ne.Uint32(ifa[4:]))
	}
	attrs := ifa[syscall.SizeofIfAddrmsg:]
	for i, typ := range []uint16{syscall.IFA_LOCAL, syscall.IFA_ADDRESS} {
		a := attrs[i*8:]
		if ne.Uint16(a[0:]) != 8 || ne.Uint16(a[2:]) != typ || !bytes.Equal(a[4:8], []byte{192, 168, 1, 50}) {
			t.Errorf("attribute %d = % x, want length 8, type %d, 192.168.1.50", i, a[:8], typ)
		}
	}
	if len(attrs) != 16 {
		t.Errorf("%d attribute bytes, want exactly the two", len(attrs))
	}
}

func ackMessage(typ uint16, seq uint32, code int32) []byte {
	b := make([]byte, syscall.SizeofNlMsghdr+4+syscall.SizeofNlMsghdr)
	ne := binary.NativeEndian
	ne.PutUint32(b[0:], uint32(len(b))) //nolint:gosec // G115: a test message of a few bytes
	ne.PutUint16(b[4:], typ)
	ne.PutUint32(b[8:], seq)
	ne.PutUint32(b[syscall.SizeofNlMsghdr:], uint32(code)) //nolint:gosec // G115: a test code that fits
	return b
}

func TestParseAck(t *testing.T) {
	if err := parseAck(ackMessage(syscall.NLMSG_ERROR, 1, 0), 1); err != nil {
		t.Errorf("an acknowledgement is an error: %v", err)
	}
	err := parseAck(ackMessage(syscall.NLMSG_ERROR, 1, -int32(syscall.EADDRNOTAVAIL)), 1)
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Errorf("error = %v, want EADDRNOTAVAIL", err)
	}
	if err := parseAck(ackMessage(syscall.NLMSG_ERROR, 2, 0), 1); err == nil {
		t.Error("an acknowledgement of another request was accepted")
	}
	for _, bad := range [][]byte{nil, {1, 2, 3}, make([]byte, syscall.SizeofNlMsghdr)} {
		if err := parseAck(bad, 1); err == nil {
			t.Errorf("parseAck(% x) accepted a malformed reply", bad)
		}
	}
}

// TestRemoveAddrAgainstTheKernel removes a real address through the kernel. It adds 192.168.77.5/24 to the
// loopback itself, so it needs NET_ADMIN in a network namespace the test may change, and it runs only when
// TIERPLAN_TEST_NETLINK is set: in a throwaway container started with --cap-add NET_ADMIN --network none.
func TestRemoveAddrAgainstTheKernel(t *testing.T) {
	if os.Getenv("TIERPLAN_TEST_NETLINK") == "" {
		t.Skip("set TIERPLAN_TEST_NETLINK in a container with --cap-add NET_ADMIN --network none to run against the kernel")
	}
	const name = "lo"
	p := netip.MustParsePrefix("192.168.77.5/24")
	held := func() bool {
		nic, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal(err)
		}
		addrs, _ := nic.Addrs()
		for _, a := range addrs {
			if a.String() == p.String() {
				return true
			}
		}
		return false
	}
	nic, err := net.InterfaceByName(name)
	if err != nil {
		t.Fatal(err)
	}
	add := delAddrMessage(nic.Index, p, 1)
	binary.NativeEndian.PutUint16(add[4:], syscall.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(add[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_EXCL)
	if err := netlinkRequest(add, 1); err != nil {
		t.Fatalf("add the address: %v", err)
	}
	if !held() {
		t.Fatalf("%s does not hold %s after it was added", name, p)
	}
	if err := RemoveAddr(name, p); err != nil {
		t.Fatalf("RemoveAddr: %v", err)
	}
	if held() {
		t.Fatalf("%s still holds %s after RemoveAddr", name, p)
	}
	// A second removal is the kernel's refusal, not a silent success.
	if err := RemoveAddr(name, p); !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("second RemoveAddr = %v, want EADDRNOTAVAIL", err)
	}
	// A prefix length that is not the one the address holds is refused too (the kernel matches it).
	if err := netlinkRequest(add, 1); err != nil {
		t.Fatalf("add the address again: %v", err)
	}
	if err := RemoveAddr(name, netip.MustParsePrefix("192.168.77.5/16")); err == nil {
		t.Fatal("RemoveAddr with another prefix length succeeded")
	}
	if err := RemoveAddr(name, p); err != nil {
		t.Fatalf("RemoveAddr cleanup: %v", err)
	}
}
