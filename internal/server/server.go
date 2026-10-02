// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package server assembles Bayplan from its configuration. Later plans add
// their dependencies (database, sessions, Docker transport) in Build.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/internal/webui"
	"github.com/ianglennon/bayplan/web"
)

const (
	// readHeaderTimeout and idleTimeout bound slow clients (T-01-17, Pitfall 8).
	// There is deliberately no global WriteTimeout: the SSE events stream
	// (plan 21) lives for hours and clears its own write deadline.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// Server is the assembled application.
type Server struct {
	cfg     config.Config
	log     *slog.Logger
	dataDir datadir.Info
	store   *store.Store
	vault   *vault.Vault
	audit   *audit.Writer
	setup   *auth.SetupGate
	inst    *instance.Instance
	session *auth.Sessions
	envs    *envs.Service
	monitor *envs.Monitor
	// snapshots caches the resource lists per environment for a few seconds.
	// The events hub invalidates it on every Docker event.
	snapshots *envs.Snapshots
	// hub holds one Docker events stream per online environment and fans it
	// out to the browsers watching (plan 21).
	hub    *envs.Hub
	router *httpx.Router

	// stopBackground ends the nightly audit prune and waits for it, so Close
	// never closes the database under a running prune.
	stopBackground context.CancelFunc
	background     sync.WaitGroup
}

// NewRouter wires every operation. It takes plain dependencies so the
// `openapi` subcommand can build the same handlers without a database or a
// socket.
func NewRouter(opts httpx.Options, sys api.SystemDeps, setup api.SetupDeps, authDeps api.AuthDeps, account api.AccountDeps, localHost api.LocalHostDeps, environments api.EnvironmentsDeps, dashboard api.DashboardDeps, events api.EventsDeps) *httpx.Router {
	r := httpx.New(opts)
	api.RegisterSystem(r, sys)
	api.RegisterSetup(r, setup)
	api.RegisterAuth(r, authDeps)
	api.RegisterAccount(r, account)
	api.RegisterLocalHost(r, localHost)
	api.RegisterEnvironments(r, environments)
	api.RegisterDashboard(r, dashboard)
	api.RegisterEvents(r, events)
	return r
}

// Build prepares the data directory, loads (or on first start generates) the
// master key, opens and migrates the database and assembles the server. It logs the effective configuration with secrets
// redacted. A migration failure (or a database newer than this binary) is
// returned as an error so `serve` exits non-zero before it listens (D-82, D-84).
func Build(ctx context.Context, cfg config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	log.Info("starting bayplan",
		"version", buildinfo.Get().Display,
		"config", cfg.Redacted(),
	)

	// DOCKER_HOST is honoured for unix:// only (D-21). Checking it before
	// anything is written means a bad value stops startup with a clear message.
	socketPath, err := engine.ParseDockerHost(cfg.DockerHost)
	if err != nil {
		return nil, err
	}

	info, err := datadir.Prepare(cfg.DataDir, datadir.Options{})
	if err != nil {
		return nil, fmt.Errorf("prepare data directory: %w", err)
	}
	if !info.Persisted {
		log.Warn("the data directory is not persisted: data written to it is lost when the container is removed; mount a named volume or bind mount at "+info.Path,
			"path", info.Path, "reason", info.Reason)
	}

	// The master key comes before the database: a bad BAYPLAN_ENCRYPTION_KEY
	// should stop startup before anything is written (D-13).
	vlt, err := vault.LoadOrCreate(cfg.EncryptionKey, info.Path, log)
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}
	log.Info("encryption key loaded", "key_id", vlt.KeyID(), "source", vlt.KeySource())
	log.Info("docker socket", "path", socketPath, "present", engine.SocketExists(socketPath))

	st, err := store.Open(ctx, info.Path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	res, err := store.Migrate(ctx, st, store.MigrateOptions{
		AppVersion: buildinfo.Get().Version,
		DataDir:    info.Path,
		SkipBackup: cfg.SkipPreMigrateBackup,
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}
	attrs := []any{"from", res.From, "to", res.To}
	if res.SnapshotPath != "" {
		attrs = append(attrs, "snapshot", res.SnapshotPath)
	}
	log.Info("database ready", attrs...)

	// /readyz needs a usable data directory and a database at this binary's
	// schema version (D-86).
	ready := func(ctx context.Context) error {
		if err := datadir.CheckWritable(info.Path); err != nil {
			return err
		}
		return st.Ready(ctx)
	}

	// The admin role holds every registered permission, re-synced at every
	// start so a new permission reaches admins on upgrade (D-45).
	if err := rbac.SeedAdminRole(ctx, st); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("seed admin role: %w", err)
	}

	aud := audit.NewWriter(st, vlt, log, nil)

	// While no user exists every start mints a fresh setup token, writes it to
	// the data directory and logs it (D-01). There is no setting that skips or
	// prefills it (D-79).
	gate := auth.NewSetupGate(st, info.Path, log)
	if err := gate.Init(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("initialise the setup gate: %w", err)
	}

	// The instance name and public URL come from the settings table, with
	// BAYPLAN_PUBLIC_URL winning (D-04). The Origin guard reads the cached
	// origin on every mutation, so a change made at setup applies at once.
	inst, err := instance.Load(ctx, st, cfg)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("load instance settings: %w", err)
	}
	sessions := auth.NewSessions(st, vlt, cfg.SessionIdleTimeout, cfg.SessionRememberLifetime, nil)
	checker := rbac.NewChecker(st)

	// Sign-in: 10 attempts at once, then one every 2 seconds, per client address.
	// This is protection against stuffing across usernames; the per-account lock
	// is in auth.Login (D-29).
	login := auth.NewLogin(st, sessions, aud, cfg.LoginMaxFailures, cfg.LoginLockout,
		auth.NewIPLimiter(2*time.Second, 10, 10*time.Minute, nil), log, nil)

	// The environment registry and its health monitors. Nothing is added here:
	// the local host becomes an environment only when an admin asks (D-19).
	// Bayplan's own container is identified now, while mountinfo is readable
	// whatever user the process later runs as (RESEARCH A9).
	monitor := envs.NewMonitor(log, nil, nil)
	selfID := envs.ReadSelfContainerID()
	envSvc := envs.NewService(st, aud, monitor, cfg.DockerHost, selfID, nil)
	// The dashboard counts and the four lists read one snapshot of Docker, cached
	// for 5 seconds per environment (Pitfall 22, D-57).
	snapshots := envs.NewSnapshots(monitor, envs.DefaultSnapshotTTL, time.Now)
	// One Docker events stream per online environment, in memory only (D-57).
	// The hub watches the monitor, so it starts and stops with each environment
	// and needs no wiring in the environment service.
	hub := envs.NewHub(monitor, snapshots, log, time.Now)

	spa := webui.Handler(web.FS())
	router := NewRouter(httpx.Options{
		Fallback:       spa,
		Checker:        checker,
		Authenticator:  auth.NewAuthenticator(st, cfg.SessionIdleTimeout, nil),
		EnvResolver:    envSvc,
		Auditor:        aud,
		MutationGuard:  auth.NewCSRFGuard(sessions),
		TrustedProxies: cfg.TrustedProxies,
		PublicOrigin:   inst.PublicOrigin,
		Logger:         log,
	}, api.SystemDeps{Ready: ready}, api.SetupDeps{
		Gate: gate,
		// 5 attempts at once, then one every 12 seconds, per client address (D-05).
		Limiter:  auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
		Audit:    aud,
		Store:    st,
		Config:   cfg,
		Instance: inst,
		Sessions: sessions,
		SPA:      spa,
		Log:      log,
	}, api.AuthDeps{
		Login:    login,
		Sessions: sessions,
		Instance: inst,
		Store:    st,
		Checker:  checker,
		Log:      log,
	}, api.AccountDeps{
		Sessions: sessions,
		Instance: inst,
		Store:    st,
		Audit:    aud,
		DataDir:  info,
		Vault:    vlt,
		Log:      log,
	}, api.LocalHostDeps{DockerHost: cfg.DockerHost, Log: log, Envs: envSvc},
		api.EnvironmentsDeps{Envs: envSvc, Checker: checker, Log: log},
		api.DashboardDeps{Snapshots: snapshots, SelfContainerID: selfID, Log: log},
		api.EventsDeps{Hub: hub, Log: log})
	// A route that skipped the policy seam stops startup, before anything listens (D-44).
	if err := httpx.VerifyAllGuarded(router); err != nil {
		hub.Close()
		monitor.StopAll()
		_ = st.Close()
		return nil, err
	}
	srv := &Server{
		cfg:       cfg,
		log:       log,
		dataDir:   info,
		store:     st,
		vault:     vlt,
		audit:     aud,
		setup:     gate,
		inst:      inst,
		session:   sessions,
		envs:      envSvc,
		monitor:   monitor,
		snapshots: snapshots,
		hub:       hub,
		router:    router,
	}

	// The nightly audit.Prune runs in this process for as long as the server does.
	bg, stop := context.WithCancel(ctx)
	srv.stopBackground = stop
	srv.background.Go(func() { audit.RunNightly(bg, srv.audit, cfg.AuditRetentionDays, log) })
	// Expired sessions are refused on lookup; this hourly sweep only keeps the table small.
	srv.background.Go(func() { auth.RunSessionCleanup(bg, st, cfg.SessionIdleTimeout, nil) })
	// Monitoring resumes for the environments an admin already added. The
	// monitors live as long as bg, and Close waits for them.
	if err := envSvc.StartAll(bg); err != nil {
		stop()
		srv.background.Wait()
		hub.Close()
		monitor.StopAll()
		_ = st.Close()
		return nil, fmt.Errorf("start environment monitors: %w", err)
	}
	return srv, nil
}

// Store is the migrated database, for the dependencies later plans add.
func (s *Server) Store() *store.Store { return s.store }

// Vault is the loaded master key, for the dependencies later plans add.
func (s *Server) Vault() *vault.Vault { return s.vault }

// Audit is the audit-log writer every mutation records through.
func (s *Server) Audit() *audit.Writer { return s.audit }

// SetupGate is the first-run gate; the setup completion endpoint consumes it.
func (s *Server) SetupGate() *auth.SetupGate { return s.setup }

// Instance is the cached instance name and public URL.
func (s *Server) Instance() *instance.Instance { return s.inst }

// Sessions creates and describes server-side sessions; plan 12's
// authenticator and login endpoint build on it.
func (s *Server) Sessions() *auth.Sessions { return s.session }

// Environments is the environment registry.
func (s *Server) Environments() *envs.Service { return s.envs }

// Snapshots is the per-environment resource snapshot cache; the events feed
// invalidates it when Docker reports a change.
func (s *Server) Snapshots() *envs.Snapshots { return s.snapshots }

// Hub is the per-environment Docker events hub.
func (s *Server) Hub() *envs.Hub { return s.hub }

// Close stops background work, including the events streams and the
// environment monitors, and releases the database. Run calls it on exit; it is
// safe to call again.
func (s *Server) Close() error {
	s.stopBackground()
	s.background.Wait()
	s.hub.Close()
	s.monitor.StopAll()
	return s.store.Close()
}

// DataDir reports the prepared data directory, for the admin banner (plan 14).
func (s *Server) DataDir() datadir.Info { return s.dataDir }

// Handler is the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.router.Handler() }

// Run serves until ctx is cancelled, then shuts down gracefully within 10s.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s (BAYPLAN_LISTEN): %w", s.cfg.Listen, err)
	}
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	// Shutdown waits for open requests, and an events stream never ends by
	// itself. Closing the hub ends every stream, so a restart is not held up for
	// the whole shutdown timeout by a browser that has the feed open.
	srv.RegisterOnShutdown(s.hub.Close)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("bayplan listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}
