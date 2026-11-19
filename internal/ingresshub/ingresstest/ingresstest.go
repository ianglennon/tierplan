// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingresstest is the server end of the ingress channel, in process, for tests: a real ingresshub.Hub
// behind a TLS test server at /ingress/connect, a real migrated store, the real ingresshub.Service and audit
// writer, fakes for the two things the hub reads from elsewhere (the current bundle of an environment and a
// swarm's node list), and a scriptable test supervisor with a knob for everything a handshake can get wrong.
// Tests of the hub, of revocation and of the end-to-end security cases all use it, so they exercise the code
// that production runs. The test supervisor builds and checks every message with proto/ingresswire's client
// helpers, the ones Phase 23's supervisor calls, so the two cannot disagree on a byte (D-28).
package ingresstest

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// Clock is a clock a test can move. It is the server's clock: the service stamps rows with it and the hub's
// limiters and timers read it.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock set to t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now is the current time of the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Options configure Start. The zero value is a working server.
type Options struct {
	// Clock is the server's clock. Nil means a clock that starts at the real time and is moved only by the test.
	Clock *Clock
	// InstanceID is written to the settings row before the hub serves. Empty keeps the ID the store mints.
	InstanceID string
	// AuditRefusals records every refused handshake as ingress.node_enrol_refused with the real audit writer,
	// unthrottled, so a test can read the reason from the database. Off by default, so tests that count audit
	// rows are unaffected.
	AuditRefusals bool
	// Configure runs on the hub before it serves, after the defaults a test server gets, so it may override
	// them.
	Configure func(*ingresshub.Hub) *ingresshub.Hub
	// Wrap puts middleware around the hub's handler, so a test can make the loopback client a trusted proxy.
	Wrap func(http.Handler) http.Handler
	// Log is where the hub logs, so a test can read what it wrote. Nil discards it.
	Log *slog.Logger
}

// Server is a running in-process TierPlan ingress endpoint.
type Server struct {
	// Hub is the real hub. HTTP is the TLS test server in front of it.
	Hub  *ingresshub.Hub
	HTTP *httptest.Server
	// Service is the real ingress node service, and Store the real, migrated store it uses.
	Service *ingresshub.Service
	Store   *store.Store
	// Clock is the server's clock.
	Clock *Clock
	// Bundles is the current bundle of each environment, and Nodes the swarm node lists: what the hub reads
	// from elsewhere in production.
	Bundles *Bundles
	Nodes   *NodeLists
}

// Start serves a hub over TLS with a real store and service. Everything is shut down when the test ends.
func Start(t *testing.T, o Options) *Server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	clock := o.Clock
	if clock == nil {
		clock = NewClock(time.Now())
	}
	st := storetest.New(t)
	// The master key sits in the data directory next to the database, as it does in production.
	vlt, err := vault.LoadOrCreate("", filepath.Dir(st.Path), log)
	if err != nil {
		t.Fatalf("ingresstest: vault: %v", err)
	}
	aud := audit.NewWriter(st, vlt, log, clock.Now)
	if _, err := instance.EnsureInstanceID(context.Background(), st, clock.Now); err != nil {
		t.Fatalf("ingresstest: instance ID: %v", err)
	}
	s := &Server{
		Store: st, Clock: clock, Bundles: newBundles(), Nodes: newNodeLists(clock),
	}
	if o.InstanceID != "" {
		s.SetInstanceID(t, o.InstanceID)
	}
	s.Service = ingresshub.NewService(st, aud, clock.Now).WithNodeLists(s.Nodes)

	ts := httptest.NewUnstartedServer(nil)
	hubLog := log
	if o.Log != nil {
		hubLog = o.Log
	}
	hub := ingresshub.NewHub(s.Service, s.Bundles, hubLog, clock.Now).
		WithPublicURL(func() string { return "https://" + ts.Listener.Addr().String() }).
		WithInstanceID(func(ctx context.Context) (string, error) { return instance.InstanceID(ctx, st) })
	if o.AuditRefusals {
		hub = hub.WithRefusalAudit(aud)
	}
	if o.Configure != nil {
		hub = o.Configure(hub)
	}
	h := hub.Handler()
	if o.Wrap != nil {
		h = o.Wrap(h)
	}
	mux := http.NewServeMux()
	mux.Handle(httpx.IngressConnectPath, h)
	ts.Config.Handler = mux
	ts.StartTLS()
	t.Cleanup(func() {
		hub.Close()
		ts.CloseClientConnections()
		ts.Close()
	})
	s.Hub, s.HTTP = hub, ts
	return s
}

// URL is the address a node dials.
func (s *Server) URL() string {
	return "ws" + strings.TrimPrefix(s.HTTP.URL, "http") + httpx.IngressConnectPath
}

// Audience is the host:port a node must address its handshake to.
func (s *Server) Audience() string { return s.HTTP.Listener.Addr().String() }

// Client is the HTTP client that trusts the test server's certificate.
func (s *Server) Client() *http.Client { return s.HTTP.Client() }

// Environment is an environment with its ingress join token.
type Environment struct {
	ID, Token string
}

// NewEnvironment creates an agent environment of envType ("swarm" or "standalone") and its ingress join token.
func (s *Server) NewEnvironment(t testing.TB, envType string) Environment {
	t.Helper()
	ctx := context.Background()
	id := newID(t)
	if _, err := s.Store.Write.ExecContext(ctx, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES (?, 'agent', ?, '', 1, 1, 1, ?)`, id, id, envType); err != nil {
		t.Fatalf("ingresstest: insert an environment: %v", err)
	}
	tok, err := s.Service.IssueToken(ctx, audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"}, id, "203.0.113.9", "ingresstest")
	if err != nil {
		t.Fatalf("ingresstest: issue a join token: %v", err)
	}
	return Environment{ID: id, Token: tok.Token}
}

// SetInstanceID overwrites the installation's instance ID, as a restore of another backup would.
func (s *Server) SetInstanceID(t testing.TB, id string) {
	t.Helper()
	if _, err := s.Store.Write.ExecContext(context.Background(),
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		instance.SettingInstanceID, id, s.Clock.Now().UnixNano()); err != nil {
		t.Fatalf("ingresstest: set the instance ID: %v", err)
	}
}
