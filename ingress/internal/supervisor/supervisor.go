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
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/ingress/internal/cache"
	"github.com/ianglennon/tierplan/ingress/internal/channel"
	"github.com/ianglennon/tierplan/ingress/internal/config"
	"github.com/ianglennon/tierplan/ingress/internal/identity"
	"github.com/ianglennon/tierplan/ingress/internal/ready"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
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

// node is the state the bundle handler shares with the start-up path: whether Traefik has been started.
type node struct {
	d *Deps
	// traefikCtx outlives every session. A session's context ends with its connection, and Traefik must not.
	traefikCtx context.Context

	mu      sync.Mutex
	started bool
}

// ensureTraefik starts Traefik when it has not been started. Traefik's own restart-on-exit is the process
// supervisor's job, so Start is called once; a failed Start is tried again at the next bundle.
func (n *node) ensureTraefik() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return nil
	}
	if err := n.d.Traefik.Start(n.traefikCtx); err != nil {
		return err
	}
	n.started = true
	return nil
}

// handle is the answer to one bundle frame (D-15, D-16). Accept decodes it against the pinned instance,
// checks it and puts it in force or leaves the volume as it was. It never restarts or signals Traefik.
func (n *node) handle(_ context.Context, frame []byte, instanceID string) (id, reason string) {
	id, reason = n.d.Store.Accept(frame, instanceID)
	if reason != "" {
		n.d.Log.Warn("bundle rejected", "id", id, "reason", reason)
		return id, reason
	}
	if err := n.ensureTraefik(); err != nil {
		// The bundle is on the volume, so the next frame (the same bundle is answered applied without a
		// write) tries the start again. The node reports it as not applied.
		n.d.Log.Error("bundle is on disk but Traefik did not start", "id", id)
		return id, ingressbundle.ReasonApplyFailed
	}
	n.d.Ready.SetApplied(true)
	n.d.Log.Info("bundle applied", "id", id)
	if err := n.d.Store.GCErr(); err != nil {
		n.d.Log.Warn("certificate cleanup after the bundle failed; extra certificates stay on the volume", "error", err)
	}
	return id, ""
}

// Run starts the node and keeps it running until ctx ends, then stops it in order. It returns an error only
// when the node cannot start (a listener that cannot open, a volume it cannot write, a Traefik it cannot
// run from a good cache); TierPlan being unreachable, a revoked node and a rejected bundle are never errors
// (PROH-23-03).
func Run(ctx context.Context, d Deps) error {
	d.fill()
	log := d.Log

	// 1. Readiness first, so a probe that arrives during start-up gets 503 and not a refused connection.
	ln, err := d.Listen("tcp", d.ReadyAddr)
	if err != nil {
		return fmt.Errorf("supervisor: listen for readiness: %w", err)
	}
	srv := &http.Server{
		Handler:           ready.Handler(d.Ready, d.Ping),
		ReadHeaderTimeout: readyHeaderTimeout,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the readiness listener stopped", "error", err)
		}
	}()
	closeReady := func() {
		sctx, cancel := context.WithTimeout(context.Background(), readyShutdown)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close()
		}
	}

	n := &node{d: &d, traefikCtx: context.WithoutCancel(ctx)}

	key, err := prepareVolume(&d)
	if err != nil {
		closeReady()
		return err
	}

	// 2. Cache-first: Traefik starts before TierPlan is contacted, and only from a bundle that verifies.
	b, ok, loadErr := d.Store.Load()
	switch {
	case ok:
		if loadErr != nil {
			log.Warn("the cached bundle serves but its record could not be completed", "error", loadErr)
		}
		if err := n.ensureTraefik(); err != nil {
			closeReady()
			return fmt.Errorf("supervisor: start Traefik from the cached bundle: %w", err)
		}
		d.Ready.SetApplied(true)
		log.Info("serving the cached bundle", "id", b.ID)
	default:
		if loadErr != nil {
			log.Warn("the cache could not be read; nothing is served from it", "error", loadErr)
		}
		log.Info("no verified cached bundle: Traefik not started; waiting for the first bundle from TierPlan")
	}

	// 3. TierPlan last.
	runErr := d.RunChannel(ctx, n.loopOptions(key))

	// Shutdown order (D-21): the channel has ended; now Traefik, then the readiness listener.
	if err := d.Traefik.Stop(); err != nil {
		log.Warn("Traefik did not stop cleanly", "error", err)
	}
	closeReady()
	return runErr
}

// prepareVolume makes the volume's layout, writes Traefik's static configuration and returns the node's key.
// A node that cannot write its own volume cannot serve, so a failure here ends the process.
func prepareVolume(d *Deps) (ed25519.PrivateKey, error) {
	if err := d.Store.Prepare(); err != nil {
		return nil, fmt.Errorf("supervisor: prepare the volume: %w", err)
	}
	if err := d.Store.WriteStatic(ingressbundle.StaticTraefik()); err != nil {
		return nil, fmt.Errorf("supervisor: write Traefik's static configuration: %w", err)
	}
	k, err := d.Identity.LoadOrCreateKey()
	if err != nil {
		return nil, fmt.Errorf("supervisor: the node key: %w", err)
	}
	return k, nil
}

// loopOptions builds the channel loop: one Dial closure that reads the identity fresh every attempt, so a node
// that enrolled in one attempt connects with its key in the next (D-38), and the session's two callbacks.
func (n *node) loopOptions(key ed25519.PrivateKey) channel.LoopOptions {
	d := n.d
	opts := channel.Options{
		URL:         d.Config.URL,
		InstallPins: d.Config.Pins,
		Version:     d.Version,
		AppliedID:   d.Store.AppliedID,
		OnEnrolled: func(w ingresswire.Welcome) error {
			return d.Identity.Save(identity.Record{NodeID: w.NodeID, EnvID: w.EnvID, InstanceID: w.InstanceID})
		},
		Log: d.Log,
	}
	return channel.LoopOptions{
		Dial: func(ctx context.Context) (*channel.Conn, error) {
			cr := channel.Credentials{
				Key:         key,
				SwarmNodeID: d.Config.NodeID,
				Name:        channel.NodeName(d.Hostname, d.Config.NodeID),
				// Dial calls Token in enrol mode only (D-22): with a NodeID it never does.
				Token: func() (string, error) { return d.Config.JoinToken(d.ReadFile) },
			}
			rec, enrolled, err := d.Identity.Load()
			if err != nil {
				return nil, fmt.Errorf("supervisor: the enrolment record: %w", err)
			}
			if enrolled {
				cr.NodeID = rec.NodeID
				cr.InstanceID = rec.InstanceID
			}
			return d.Dial(ctx, opts, cr)
		},
		Session: channel.SessionOptions{
			Handle: n.handle,
			Health: func(ctx context.Context) (bool, string) {
				return d.Ping(ctx), d.Store.AppliedID()
			},
			Log: d.Log,
		},
		Log: d.Log,
	}
}
