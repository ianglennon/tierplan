// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package engine is the seam between Bayplan and a Docker engine: how bytes
// reach it. Phase 1 has one Transport, the local unix socket. Phase 2 adds the
// agent tunnel as another Transport behind the same interface.
package engine

import (
	"context"
	"errors"
	"net"
)

// DefaultSocketPath is where Docker listens unless DOCKER_HOST says otherwise.
const DefaultSocketPath = "/var/run/docker.sock"

// ErrUnsupportedDockerHost is returned for a DOCKER_HOST that is not a unix
// socket path (D-21).
var ErrUnsupportedDockerHost = errors.New("unsupported DOCKER_HOST")

// Transport carries bytes to one Docker engine.
type Transport interface {
	// DialContext opens a connection to the engine. The network and address
	// come from the HTTP client and are ignored: the transport alone decides
	// where the bytes go.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// Describe names the engine address for logs and errors, for example
	// unix:///var/run/docker.sock.
	Describe() string
}

// LocalSocketTransport dials a unix socket on this host.
type LocalSocketTransport struct {
	Path string
}

// DialContext implements Transport.
func (t LocalSocketTransport) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, errors.New("not implemented")
}

// Describe implements Transport.
func (t LocalSocketTransport) Describe() string { return "" }

// ParseDockerHost turns a DOCKER_HOST value into a socket path. Empty means
// the default socket. Only unix:// is accepted (D-21, D-22).
func ParseDockerHost(raw string) (string, error) {
	return "", errors.New("not implemented")
}

// SocketExists reports whether path exists and is a unix socket.
func SocketExists(path string) bool { return false }
