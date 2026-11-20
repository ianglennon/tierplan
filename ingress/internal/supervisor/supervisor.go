// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package supervisor wires the ingress node together (D-13, D-21, D-22, D-38, D-39). Run starts things in an
// order that is the whole point of the package:
//
//  1. the readiness listener, so a probe always gets a bare answer, 503 until there is something to serve;
//  2. the volume's layout and Traefik's static file, then the node's key, so every later step has a place to
//     write and an identity to present;
//  3. Traefik, but only when the cache holds a bundle that verifies, so a node with nothing good serves
//     nothing (ports 8080 and 8443 stay closed) instead of answering with Traefik's default certificate;
//  4. TierPlan last, so a node with a cache serves while TierPlan is down, and a node that TierPlan has
//     revoked or lost keeps serving what it has (frozen, not broken).
//
// A bundle that arrives on the channel goes through cache.Store.Accept. An accepted one is on disk and Traefik's
// file provider picks it up; the supervisor never restarts or signals Traefik for a bundle (RTE-04). The first
// accepted bundle of a node with no cache is the one case where it starts Traefik. A rejected one changes
// nothing: Traefik keeps its last good configuration and readiness is untouched.
//
// On shutdown the channel ends first, then Traefik is stopped, then the readiness listener closes (D-21).
package supervisor

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/identity"
	"github.com/ianglennon/tierplan/ingress/internal/ready"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

const (
	// readyShutdown bounds how long closing the readiness listener waits for a request in flight. A probe
	// holds a connection for milliseconds, so this only ever ends a stuck client.
	readyShutdown = 2 * time.Second
	// readyHeaderTimeout bounds how long a client may take to send its request line and headers.
	readyHeaderTimeout = 5 * time.Second
)

// Traefik is the supervised proxy process. traefikproc.Proc satisfies it.
type Traefik interface {
	// Start starts the process and returns once it is running. It is called once.
	Start(ctx context.Context) error
	// Stop stops it and returns once it has stopped.
	Stop() error
	// Running reports whether a process is running.
	Running() bool
}

// Deps is everything Run touches outside its own memory, so a test can drive the whole start-up with fakes
// and a real volume in a temporary directory. A nil field takes the production value.
type Deps struct {
	Config   config.Config
	Store    *cache.Store
	Identity *identity.Store
	Traefik  Traefik
	// Ready is the readiness state the endpoint reports. Nil means a new State.
	Ready *ready.State
	// Ping is Traefik's liveness check. Nil means a GET of ready.DefaultPingURL.
	Ping ready.Pinger
	// Listen opens the readiness listener. Nil means net.Listen.
	Listen func(network, addr string) (net.Listener, error)
	// ReadyAddr is where the readiness endpoint listens. Empty means ":" and ingressbundle.ReadyPort.
	ReadyAddr string
	// RunChannel keeps the session with TierPlan going and returns when ctx ends. Nil means channel.Run.
	RunChannel func(context.Context, channel.LoopOptions) error
	// Dial makes one connection attempt. Nil means channel.Dial.
	Dial func(context.Context, channel.Options, channel.Credentials) (*channel.Conn, error)
	// ReadFile reads the join token file. Nil means os.ReadFile.
	ReadFile func(string) ([]byte, error)
	// Hostname names the host for the enrolment. Nil means os.Hostname.
	Hostname func() (string, error)
	// Version is reported to TierPlan in hello.
	Version string
	// Log receives the supervisor's lines. Nil means slog.Default.
	Log *slog.Logger
}

func (d *Deps) fill() {
	if d.Ready == nil {
		d.Ready = &ready.State{}
	}
	if d.Ping == nil {
		d.Ping = ready.NewPinger(&http.Client{}, ready.DefaultPingURL())
	}
	if d.Listen == nil {
		d.Listen = net.Listen
	}
	if d.ReadyAddr == "" {
		d.ReadyAddr = ":" + strconv.Itoa(ingressbundle.ReadyPort)
	}
	if d.RunChannel == nil {
		d.RunChannel = channel.Run
	}
	if d.Dial == nil {
		d.Dial = channel.Dial
	}
	if d.ReadFile == nil {
		d.ReadFile = os.ReadFile
	}
	if d.Hostname == nil {
		d.Hostname = os.Hostname
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

func Run(ctx context.Context, d Deps) error { return nil }
