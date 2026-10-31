// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package hostroute reads the text of a Linux /proc/net/route file into the
// IPv4 routes of a host. The agent parses the file it is given and the server
// parses nothing it is sent without checking it again, so both ends use this one
// parser and the same limits. It never trusts the text: every field is checked,
// the output is capped, and an interface name is cut to a safe set. Standard
// library only; it must not import the wire package.
package hostroute

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"strconv"
	"strings"
)

const (
	// MaxRoutes is the most routes Parse keeps; more sets truncated.
	MaxRoutes = 512
	// MaxIfaceLen is the longest interface name kept (IFNAMSIZ is 16 with the
	// terminating byte).
	MaxIfaceLen = 15
	// MaxLine is the longest line Parse accepts, without its line break.
	MaxLine = 512
	// MaxFile is the most bytes Parse reads.
	MaxFile = 256 << 10

	// rtfUp is the RTF_UP flag: the route is usable.
	rtfUp = 0x0001
	// minFields is Iface Destination Gateway Flags RefCnt Use Metric Mask.
	minFields = 8
)

// ErrTooLarge means a line was longer than MaxLine or the input longer than
// MaxFile. Nothing of the input is returned with it.
var ErrTooLarge = errors.New("the route table is larger than the limits allow")

// Route is one usable IPv4 route. Dst is masked and has 1 to 31 bits. Gateway is
// the zero Addr when the route is directly connected.
type Route struct {
	Iface   string
	Dst     netip.Prefix
	Gateway netip.Addr
}

// SanitizeIface keeps the bytes of an interface name that are safe to store and
// show ([A-Za-z0-9._@:-]) and cuts the result to MaxIfaceLen.
func SanitizeIface(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < MaxIfaceLen; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '@', c == ':', c == '-':
		default:
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Parse reads the text of /proc/net/route. Destination, Gateway and Mask are
// eight hex digits in the byte order of the host that wrote them, which is the
// host Parse runs on. Only routes with the UP flag are kept; the default route
// and single-host (/32) routes are dropped; a line that is malformed is skipped,
// not an error. truncated is true when more than MaxRoutes routes were left out.
func Parse(r io.Reader) (routes []Route, truncated bool, err error) {
	lr := &io.LimitedReader{R: r, N: MaxFile + 1}
	br := bufio.NewReaderSize(lr, 4096)
	seen := make(map[string]struct{})
	first := true
	for {
		line, rerr := br.ReadSlice('\n')
		if errors.Is(rerr, bufio.ErrBufferFull) {
			return nil, false, ErrTooLarge
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, false, fmt.Errorf("read the route table: %w", rerr)
		}
		if lr.N <= 0 {
			return nil, false, ErrTooLarge
		}
		text := strings.TrimRight(string(line), "\r\n")
		if len(text) > MaxLine {
			return nil, false, ErrTooLarge
		}
		if first {
			first = false // the header
		} else if rt, ok := parseLine(text); ok {
			key := rt.Iface + "|" + rt.Dst.String()
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				if len(routes) >= MaxRoutes {
					truncated = true
				} else {
					routes = append(routes, rt)
				}
			}
		}
		if rerr != nil {
			return routes, truncated, nil
		}
	}
}

func parseLine(text string) (Route, bool) {
	f := strings.Fields(text)
	if len(f) < minFields {
		return Route{}, false
	}
	dst, ok := hex32(f[1])
	if !ok {
		return Route{}, false
	}
	gw, ok := hex32(f[2])
	if !ok {
		return Route{}, false
	}
	flags, err := strconv.ParseUint(f[3], 16, 32)
	if err != nil || flags&rtfUp == 0 {
		return Route{}, false
	}
	mask, ok := hex32(f[7])
	if !ok {
		return Route{}, false
	}
	ones := bits.OnesCount32(mask)
	// A contiguous mask is ones followed by zeros once read in network order.
	// The default route (0) and a host route (32) are not subnets to avoid.
	if ones == 0 || ones == 32 || mask != ^uint32(0)<<(32-ones) {
		return Route{}, false
	}
	rt := Route{
		Iface: SanitizeIface(f[0]),
		Dst:   netip.PrefixFrom(addr(dst), ones).Masked(),
	}
	if gw != 0 {
		rt.Gateway = addr(gw)
	}
	return rt, true
}

// hex32 reads exactly eight hex digits as the number the kernel printed, then
// puts it in network order: the host wrote it with its own byte order, so
// reading it back with the same order restores the address bytes. The result is
// the address as a big-endian number, which is how masks are compared.
func hex32(s string) (uint32, bool) {
	if len(s) != 8 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, false
	}
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], uint32(v)) //nolint:gosec // ParseUint with bitSize 32
	return binary.BigEndian.Uint32(b[:]), true
}

func addr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
