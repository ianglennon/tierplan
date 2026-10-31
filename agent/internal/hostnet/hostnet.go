// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package hostnet reads the routes of the agent's host from one file the host
// owner bind-mounts read-only (/proc/1/net/route on the host, /host/route in
// the container). It reads that file and nothing else: no Docker call, no
// command, no extra privilege, so the agent stays read-only, UID 1000 and
// --cap-drop ALL. A file that is missing or unreadable is the "unmounted" state,
// not an error.
package hostnet

import (
	"bufio"
	"errors"
	"io"
	"net/netip"
	"os"

	"github.com/ianglennon/coaming/proto/hostroute"
	"github.com/ianglennon/coaming/proto/wire"
)

// DefaultRouteFile is where the install command mounts the host's route file.
const DefaultRouteFile = "/host/route"

// Probe says whether the route file at path can be read: wire.HostNetOK when its
// first line is readable, wire.HostNetUnmounted otherwise.
func Probe(path string) string {
	f, err := os.Open(path) //nolint:gosec // the path is the host owner's setting (COAMING_AGENT_HOST_ROUTE_FILE), never the server's
	if err != nil {
		return wire.HostNetUnmounted
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 1024)
	line, err := br.ReadSlice('\n')
	// A directory, a device that errors or an empty file is not a route file. A
	// first line with no line break (EOF) or longer than the buffer still proves
	// the file reads.
	if len(line) == 0 || (err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull)) {
		return wire.HostNetUnmounted
	}
	return wire.HostNetOK
}

// Read parses the route file at path afresh and returns the document the server
// asked for. A file that cannot be opened or read answers Error "unreadable", one
// that exceeds the parser's limits "too_large"; both carry no routes.
func Read(path string) wire.HostNetDoc {
	doc := wire.HostNetDoc{V: 1, Routes: []wire.HostRoute{}}
	f, err := os.Open(path) //nolint:gosec // the path is the host owner's setting (COAMING_AGENT_HOST_ROUTE_FILE), never the server's
	if err != nil {
		doc.Error = wire.HostNetUnreadable
		return doc
	}
	defer func() { _ = f.Close() }()
	routes, truncated, err := hostroute.Parse(f)
	if err != nil {
		if errors.Is(err, hostroute.ErrTooLarge) {
			doc.Error = wire.HostNetTooLarge
		} else {
			doc.Error = wire.HostNetUnreadable
		}
		return doc
	}
	doc.Routes = toWire(routes)
	doc.Truncated = truncated
	return doc
}

func toWire(routes []hostroute.Route) []wire.HostRoute {
	out := make([]wire.HostRoute, 0, len(routes))
	for _, r := range routes {
		hr := wire.HostRoute{Iface: r.Iface, Dst: r.Dst.String()}
		if r.Gateway != (netip.Addr{}) {
			hr.Gateway = r.Gateway.String()
		}
		out = append(out, hr)
	}
	return out
}
