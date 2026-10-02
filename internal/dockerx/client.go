// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package dockerx is the only package that imports the Docker (moby) client.
// It exposes typed, read-only methods that return Bayplan types, never moby
// types, and has no raw passthrough to the Docker API: that is what keeps a
// compromise of the web layer from becoming arbitrary Docker access (D-52,
// Pitfall 1). Plugin and swarm init/join/leave/unlock calls have no method at
// all. The depguard rule in .golangci.yml enforces the import boundary.
package dockerx

import (
	"fmt"
	"sync"

	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/internal/engine"
)

// hostURL is only used by the moby client to build request URLs. The Transport
// decides where the bytes really go (RESEARCH A14).
const hostURL = "unix:///var/run/docker.sock"

// Client talks to one Docker engine through an engine.Transport.
type Client struct {
	t engine.Transport

	// mu guards the c pointer and nothing else. No lock is held across network
	// I/O: a caller copies the pointer through cur() and then talks to the
	// engine with no lock, so a slow Snapshot can never delay a Probe, and a
	// Probe that replaces the client never waits for a Snapshot (WR-06).
	mu sync.RWMutex
	c  *client.Client

	// probeSem serialises probes: at most one renegotiates at a time, so they
	// cannot race on swapping the client. It is a channel, not a mutex, so a
	// probe that is waiting can abandon the wait when its context ends.
	probeSem chan struct{}
}

// New builds a Client over t. It never reads DOCKER_* environment variables:
// the options are explicit, so a stray DOCKER_API_VERSION or DOCKER_CERT_PATH
// in the container cannot change how Bayplan talks to Docker (Pitfall 1).
func New(t engine.Transport) (*Client, error) {
	c, err := newMoby(t)
	if err != nil {
		return nil, err
	}
	return &Client{t: t, c: c, probeSem: make(chan struct{}, 1)}, nil
}

// newMoby builds a moby client over t. WithHost goes first and
// WithDialContext second, so the transport, not the host string, decides how
// bytes move.
func newMoby(t engine.Transport, extra ...client.Opt) (*client.Client, error) {
	opts := append([]client.Opt{
		client.WithHost(hostURL),
		client.WithDialContext(t.DialContext),
	}, extra...)
	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create the docker client for %s: %w", t.Describe(), err)
	}
	return c, nil
}

// cur is the moby client that is current now. A caller keeps the pointer for
// the whole of its call: a later Probe replaces the client but closes only its
// idle connections, so a call already in flight on the old one finishes.
func (c *Client) cur() *client.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.c
}

// Close releases the idle connections of the client.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.c.Close()
}
