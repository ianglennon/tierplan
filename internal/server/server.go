// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package server assembles Bayplan from its configuration. Later plans add
// their dependencies (database, sessions, Docker transport) in Build.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
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
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/internal/webui"
	"github.com/ianglennon/bayplan/web"
)

const (
	// readHeaderTimeout, readTimeout and idleTimeout bound slow clients
	// (T-01-17, Pitfall 8). There is deliberately no global WriteTimeout: the
	// SSE events stream (plan 21) lives for hours and clears its own write
	// deadline.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// readTimeout bounds reading a whole request, headers and body, so a client
// that sends the headers and then withholds the body is cut off (WR-14,
// T-01-17). Go clears the read deadline once a request with no body is being
// served, so a long-lived response such as the events stream is unaffected.
// It is a variable so an internal test can shorten it.
var readTimeout = 30 * time.Second

// newHTTPServer returns the http.Server Bayplan serves with. huma already caps
// JSON bodies (MaxBodyBytes), so no MaxBytesReader is added here.
func newHTTPServer(h http.Handler, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		TLSConfig:         tlsCfg,
	}
}

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
	hub *envs.Hub
	// clusters reads each swarm's managers on a timer and decides which one a
	// cluster read goes to.
	clusters *envs.Clusters
	// aggregator is the one read path to an environment's Docker data; it holds
	// the merged event feeds (plan 02-21).
	aggregator *envs.Aggregator
	// broker turns agent connections and health changes into the global
	// environments stream (plan 02-12).
	broker *envs.Broker
	// tunnel holds the live agent connections; agents creates agent
	// environments and enrols agents.
	tunnel  *tunnel.Hub
	agents  *agents.Service
	updater *agents.Updater
	router  *httpx.Router
	// tlsConfig is nil when the server speaks plain HTTP (D-12).
	tlsConfig *tls.Config

	// stopBackground ends the nightly audit prune and waits for it, so Close
	// never closes the database under a running prune.
	stopBackground context.CancelFunc
	background     sync.WaitGroup
}

// NewRouter wires every operation. It takes plain dependencies so the
// `openapi` subcommand can build the same handlers without a database or a
// socket.
//
// agentConnect serves the agent WebSocket at /agent/connect. It is the one route
// outside the policy besides the probes and the setup page, because the handshake
// inside it is its own authentication. Nil mounts a handler that answers 404,
// which is what the `openapi` subcommand and the router-walk tests pass: the route
// exists in every router, so its exemption is always checked.
func NewRouter(opts httpx.Options, sys api.SystemDeps, setup api.SetupDeps, authDeps api.AuthDeps, account api.AccountDeps, localHost api.LocalHostDeps, environments api.EnvironmentsDeps, agentsDeps api.AgentsDeps, dashboard api.DashboardDeps, events api.EventsDeps, envStream api.EnvStreamDeps, recent api.RecentDeps, auditDeps api.AuditDeps, settings api.SettingsDeps, agentConnect http.Handler) *httpx.Router {
	r := httpx.New(opts)
	if agentConnect == nil {
		agentConnect = http.NotFoundHandler()
	}
	r.AgentConnect(agentConnect)
	api.RegisterSystem(r, sys)
	api.RegisterSetup(r, setup)
	api.RegisterAuth(r, authDeps)
	api.RegisterAccount(r, account)
	api.RegisterSettings(r, settings)
	api.RegisterLocalHost(r, localHost)
	api.RegisterEnvironments(r, environments)
	api.RegisterAgents(r, agentsDeps)
	api.RegisterEnvWarnings(r, api.EnvWarningsDeps{Envs: environments.Envs, Checker: environments.Checker, Agents: agentsDeps, Log: environments.Log})
	api.RegisterDashboard(r, dashboard)
	api.RegisterEvents(r, events)
	api.RegisterEnvStream(r, envStream)
	api.RegisterRecent(r, recent)
	api.RegisterAudit(r, auditDeps)
	return r
}

// Option adjusts Build.
type Option func(*buildOptions)

type buildOptions struct {
	selfContainerID string
}

// WithSelfContainerID supplies the ID of the container Bayplan runs in. The
// command line reads it from /proc/self/mountinfo before the privilege drop
// (RESEARCH A9, D-27) and passes it in, so Build never probes /proc itself.
// Without it nothing is badged "this instance".
func WithSelfContainerID(id string) Option {
	return func(o *buildOptions) { o.selfContainerID = id }
}

// Build prepares the data directory, loads (or on first start generates) the
// master key, opens and migrates the database and assembles the server. It logs the effective configuration with secrets
// redacted. A migration failure (or a database newer than this binary) is
// returned as an error so `serve` exits non-zero before it listens (D-82, D-84).
func Build(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Option) (*Server, error) {
	var bo buildOptions
	for _, opt := range opts {
		opt(&bo)
	}
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

	// HTTPS comes from a supplied pair or a generated self-signed certificate
	// (D-12). It is prepared here so a bad pair stops startup before listening.
	tlsCfg, err := TLSConfig(cfg, inst.PublicURL(), inst.AgentURL(), nil)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	switch {
	case tlsCfg == nil:
		log.Info("serving plain HTTP; set BAYPLAN_TLS_CERT_FILE and BAYPLAN_TLS_KEY_FILE, BAYPLAN_TLS_SELF_SIGNED=true, or put a TLS-terminating proxy in front")
	case cfg.TLSSelfSigned:
		log.Info("serving HTTPS with a self-signed certificate; compare this fingerprint in your browser",
			"sha256", fingerprint(tlsCfg), "tls_pin", spkiPin(tlsCfg),
			"certificate", filepath.Join(info.Path, selfSignedDir, selfSignedCertFile))
	default:
		log.Info("serving HTTPS", "certificate_file", cfg.TLSCertFile, "sha256", fingerprint(tlsCfg))
	}

	sessions := auth.NewSessions(st, vlt, cfg.SessionIdleTimeout, cfg.SessionRememberLifetime, nil)
	checker := rbac.NewChecker(st)

	// Sign-in: 10 attempts at once, then one every 2 seconds, per client address.
	// This is protection against stuffing across usernames; the per-account lock
	// is in auth.Login (D-29).
	login := auth.NewLogin(st, sessions, aud, cfg.LoginMaxFailures, cfg.LoginLockout,
		auth.NewIPLimiter(2*time.Second, 10, 10*time.Minute, nil), log, nil)

	// Refusals an anonymous client can repeat for free (a denied route, a setup
	// attempt after setup, a rate-limited sign-in) would each take the single
	// write connection and could starve signed-in users (WR-04). Each address
	// gets 20 audit rows at once, then 10 a minute; the rest are counted and
	// written as one summary row per key a minute, so every refusal is still
	// recorded (D-49, T-01-35). Signed-in actors pass straight through.
	anonAudit := auth.NewAuditThrottle(aud, auth.NewIPLimiter(6*time.Second, 20, 10*time.Minute, nil), log, nil)
	login.SetAnonymousAuditor(anonAudit)

	// The environment registry and its health monitors. Nothing is added here:
	// the local host becomes an environment only when an admin asks (D-19).
	// Bayplan's own container was identified before the privilege drop and is
	// handed in with WithSelfContainerID (RESEARCH A9).
	monitor := envs.NewMonitor(log, nil, nil)
	selfID := bo.selfContainerID
	envSvc := envs.NewService(st, aud, monitor, cfg.DockerHost, selfID, nil)
	// The dashboard counts and the four lists read one snapshot of Docker, cached
	// for 5 seconds per environment (Pitfall 22, D-57).
	snapshots := envs.NewSnapshots(monitor, envs.DefaultSnapshotTTL, time.Now)
	// One Docker events stream per online environment, in memory only (D-57).
	// The hub watches the monitor, so it starts and stops with each environment
	// and needs no wiring in the environment service.
	hub := envs.NewHub(monitor, snapshots, log, time.Now)

	// Agents dial in over /agent/connect. Enrolment checks the join token and
	// issues a certificate from the environment's own CA (D-01, D-07); the hub
	// holds the live sessions and tells the environment registry about each, so
	// a connected agent becomes a monitor target (RESEARCH Pattern 5).
	agentSvc := agents.NewService(st, vlt, aud, nil)
	// Agents heartbeat at cfg.AgentHeartbeatInterval, which the welcome tells
	// them, and a session silent for cfg.AgentOfflineAfter is closed (D-41).
	// Heartbeats stay in memory; last_seen_at is written at most once a minute.
	// Refused handshakes are anonymous and cost an audit row each, so they go
	// through a throttle of their own: 20 rows at once per address, then one every
	// 6 seconds, with the rest counted into one summary row a minute (RESEARCH
	// Pitfall 8, D-13). It is separate from anonAudit so that agents behind one
	// address and people signing in do not spend each other's budget.
	agentAudit := auth.NewAuditThrottle(aud, auth.NewIPLimiter(6*time.Second, 20, 10*time.Minute, nil), log, nil)
	// The pin an install command carries, and the hint a refused agent gets, are
	// both read when asked: the pin changes only if the self-signed key is
	// replaced, and the endpoint follows Settings.
	tlsPin := func() string { return firstSelfSignedPin(cfg, tlsCfg) }
	// The health broker is told what the tunnel hub sees, and recomputes every
	// environment's health on a tick, so one stream per tab can say what changed.
	broker := envs.NewBroker(envSvc, nil)
	// The proxy observer notices a reverse proxy that is misconfigured from the
	// requests it carries and from agent endpoint requests that are not upgrades,
	// remembers it for an hour and tells the open bells when a kind appears (D-53).
	proxyObs := httpx.NewProxyObserver(inst.PublicURL, nil).OnNew(broker.PublishProxyWarning)
	tunnelHub := tunnel.NewHub(agentSvc, envSvc, log, nil).
		WithNotifier(broker).
		OnUpgradeFailure(proxyObs.RecordUpgradeFailure).
		// The hub checks each handshake against the address agents are told to dial:
		// the agent URL when one is set, else the public URL (D-18).
		WithPublicURL(inst.AgentBase).
		// Every welcome, and every push when an address changes, carries the
		// endpoint agents are told to dial (D-19).
		WithEndpoint(inst.AgentEndpoint).
		WithPins(func() []string { return selfSignedPins(cfg, tlsCfg) }).
		WithTiming(cfg.AgentHeartbeatInterval, cfg.AgentOfflineAfter).
		WithLastSeen(agentSvc).
		WithServerVersion(buildinfo.Get().Version).
		// An agent outside the version window is told the manual command that
		// updates it, for its kind of environment (D-25).
		WithUpdateHint(func(tooOld bool, envType string) string {
			return agents.UpdateHint(tooOld, envType, agents.InstallParams{
				Endpoint: inst.AgentEndpoint(), Pin: tlsPin(), ServerVersion: buildinfo.Get().Version,
				AllowInsecure: cfg.AgentAllowInsecure,
			})
		}).
		WithAllowInsecure(cfg.AgentAllowInsecure).
		WithConnectLimit(cfg.AgentConnectBurst).
		WithRefusalAudit(agentAudit)
	// Agents refuse a plain ws:// address for another host, and so does the hub,
	// unless the development switch is on. Say so at start, because nothing could
	// connect. The address is read at start only; the hub's own refusal line covers
	// a later change in Settings (G-02-R4-3).
	if base := inst.AgentBase(); tunnel.PlainAddressRefused(base, cfg.AgentAllowInsecure) {
		log.Warn("agents cannot connect: the address they are given is plain http for a host that is not this machine, so agents refuse it and so does Bayplan. Set an https BAYPLAN_PUBLIC_URL or BAYPLAN_AGENT_URL (or change it in Settings); BAYPLAN_AGENT_ALLOW_INSECURE=true allows it, for development only",
			"agent_base", base)
	}
	// A revoke commits first and then closes the agent's live session through the hub.
	agentSvc.WithKicker(tunnelHub)
	// A re-initialised swarm that is recorded, or adopted, reaches every open
	// overview at once instead of at the next tick.
	agentSvc.WithNotifier(broker)

	// The registry rolls an environment's health up from its agents, so it reads
	// the live ones from the hub and uses the configured windows (D-41); every
	// Docker read of an environment goes through the aggregator over the registry,
	// the snapshot cache and the events hub (D-47).
	envSvc.WithAgents(tunnelHub).WithHealth(envs.HealthConfig{
		OfflineAfter: cfg.AgentOfflineAfter, Grace: cfg.AgentReconnectGrace,
		// An agent older than this server shows an update badge in the overview.
		ServerVersion: buildinfo.Get().Version,
	})
	// The cluster reader keeps what a swarm's managers say about it: which manager
	// leads, the nodes, the failing services. It retries a read once on another
	// manager and says so, and it feeds the rollup and the overview (D-43).
	// Each node list it reads is shown to the reaper, which revokes the agent of a
	// node that has left the swarm (D-10).
	clusters := envs.NewClusters(envSvc, snapshots, envs.ClusterOptions{
		Publisher: broker, Events: hub, Reaper: agents.NewReaper(agentSvc),
		Grace: cfg.FailingServiceGrace, Log: log,
	})
	envSvc.WithClusters(clusters)
	// After a server upgrade the updater brings agents of an older release up to date
	// through the tunnel, one message per swarm through its leading manager, one per
	// standalone host, unless the environment or the host switched it off (D-24, D-28).
	// It hears of each agent's control stream and each update_result from the hub.
	updater := agents.NewUpdater(agentSvc, agents.HubSource(tunnelHub), func(envID string) (string, bool) {
		id := clusters.LeaderAgent(envID)
		return id, id != ""
	}, buildinfo.Get().Version, nil).WithLogger(log)
	tunnelHub.WithUpdateObserver(updater)
	// Removing an environment can first have its online agents uninstall themselves,
	// through the same control streams, and hears their answers the same way (D-30).
	envSvc.WithUninstaller(updater)
	aggregator := envs.NewAggregator(envSvc, snapshots, hub, time.Now)

	spa := webui.Handler(web.FS(), inst.BasePath)
	router := NewRouter(httpx.Options{
		Fallback:       spa,
		Checker:        checker,
		Authenticator:  auth.NewAuthenticator(st, cfg.SessionIdleTimeout, inst.CookieScope, nil),
		EnvResolver:    envSvc,
		Auditor:        anonAudit,
		MutationGuard:  auth.NewCSRFGuard(sessions),
		TrustedProxies: cfg.TrustedProxies,
		Proxy:          proxyObs,
		PublicOrigin:   inst.PublicOrigin,
		Logger:         log,
		// Strict-Transport-Security when the request came over TLS or the
		// public URL is https, which also covers a TLS-terminating proxy (D-37).
		// Not for Bayplan's own self-signed certificate: a browser pinned to it
		// could not click through a regenerated one (WR-09).
		HSTS: func(r *http.Request) bool {
			if r.TLS != nil {
				return !cfg.TLSSelfSigned
			}
			return inst.Secure()
		},
	}, api.SystemDeps{Ready: ready}, api.SetupDeps{
		Gate: gate,
		// 5 attempts at once, then one every 12 seconds, per client address (D-05).
		Limiter:   auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
		Audit:     aud,
		AnonAudit: anonAudit,
		Store:     st,
		Config:    cfg,
		Instance:  inst,
		Sessions:  sessions,
		SPA:       spa,
		Log:       log,
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
		Proxy:    proxyObs,
		// Five password-change attempts per user at once, then one every 12
		// seconds (T-01-60): ample for a person, slow for a guesser.
		Limiter: auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
		Log:     log,
	}, api.LocalHostDeps{DockerHost: cfg.DockerHost, Log: log, Envs: envSvc},
		api.EnvironmentsDeps{Envs: envSvc, Agents: agentSvc, Instance: inst, TLSPin: tlsPin, AgentAllowInsecure: cfg.AgentAllowInsecure, Checker: checker, Updater: updater, Log: log},
		api.AgentsDeps{Agents: agentSvc, Local: envSvc, Live: tunnelHub, Cluster: clusters, Instance: inst, TLSPin: tlsPin, AgentAllowInsecure: cfg.AgentAllowInsecure, Updater: updater, Log: log},
		api.DashboardDeps{Aggregator: aggregator, SelfContainerID: selfID, Log: log},
		api.EventsDeps{Aggregator: aggregator, Log: log},
		api.EnvStreamDeps{Broker: broker, Checker: checker, Log: log},
		api.RecentDeps{Store: st, Envs: envSvc, Checker: checker, Log: log},
		api.AuditDeps{Store: st, RetentionDays: cfg.AuditRetentionDays, Log: log},
		api.SettingsDeps{Instance: inst, Store: st, Audit: aud, Agents: settingsAgents{svc: agentSvc, hub: tunnelHub}, AgentAllowInsecure: cfg.AgentAllowInsecure, Log: log}, tunnelHub.Handler())
	// A route that skipped the policy seam stops startup, before anything listens (D-44).
	if err := httpx.VerifyAllGuarded(router); err != nil {
		tunnelHub.Close()
		aggregator.Close()
		hub.Close()
		monitor.StopAll()
		_ = st.Close()
		return nil, err
	}
	srv := &Server{
		cfg:        cfg,
		log:        log,
		dataDir:    info,
		store:      st,
		vault:      vlt,
		audit:      aud,
		setup:      gate,
		inst:       inst,
		session:    sessions,
		envs:       envSvc,
		monitor:    monitor,
		snapshots:  snapshots,
		hub:        hub,
		clusters:   clusters,
		aggregator: aggregator,
		broker:     broker,
		tunnel:     tunnelHub,
		agents:     agentSvc,
		updater:    updater,
		router:     router,
		tlsConfig:  tlsCfg,
	}

	// The nightly audit.Prune runs in this process for as long as the server does.
	bg, stop := context.WithCancel(ctx)
	srv.stopBackground = stop
	srv.background.Go(func() { audit.RunNightly(bg, srv.audit, cfg.AuditRetentionDays, log) })
	// Expired sessions are refused on lookup; this hourly sweep only keeps the table small.
	srv.background.Go(func() { auth.RunSessionCleanup(bg, st, cfg.SessionIdleTimeout, nil) })
	// Suppressed anonymous refusals are written every minute, and once more when
	// bg ends, which is before Close shuts the database.
	srv.background.Go(func() { anonAudit.Run(bg, time.Minute) })
	srv.background.Go(func() { agentAudit.Run(bg, time.Minute) })
	srv.background.Go(func() { broker.Run(bg) })
	srv.background.Go(func() { clusters.Run(bg) })
	// Monitoring resumes for the environments an admin already added. The
	// monitors live as long as bg, and Close waits for them.
	if err := envSvc.StartAll(bg); err != nil {
		stop()
		srv.background.Wait()
		tunnelHub.Close()
		aggregator.Close()
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

// Clusters is the cluster reader of swarm environments. The server reads every
// environment itself every 15 seconds; a test that needs a read at a given moment
// calls Refresh.
func (s *Server) Clusters() *envs.Clusters { return s.clusters }

// TLSPin is the SPKI pin ("sha256/" and base64) of the certificate Bayplan
// generated for itself, which an install command carries so the agent can trust
// it (D-06). It is empty when Bayplan serves a supplied certificate or plain
// HTTP: the first is the operator's to pin, the second has nothing to pin.
func (s *Server) TLSPin() string { return firstSelfSignedPin(s.cfg, s.tlsConfig) }

// firstSelfSignedPin is the pin an install command carries: the generated
// certificate's, and "" when there is none.
func firstSelfSignedPin(cfg config.Config, tlsCfg *tls.Config) string {
	if p := selfSignedPins(cfg, tlsCfg); len(p) > 0 {
		return p[0]
	}
	return ""
}

// selfSignedPins is the pin list a welcome carries: the generated certificate's
// pin, and nothing for a supplied certificate or plain HTTP.
func selfSignedPins(cfg config.Config, tlsCfg *tls.Config) []string {
	if !cfg.TLSSelfSigned {
		return nil
	}
	if p := spkiPin(tlsCfg); p != "" {
		return []string{p}
	}
	return nil
}

// Tunnel holds the live agent connections.
func (s *Server) Tunnel() *tunnel.Hub { return s.tunnel }

// Agents creates agent environments and enrols agents.
func (s *Server) Agents() *agents.Service { return s.agents }

// Close stops background work, including the events streams and the
// environment monitors, and releases the database. Run calls it on exit; it is
// safe to call again.
func (s *Server) Close() error {
	s.stopBackground()
	s.background.Wait()
	s.broker.Close()
	s.updater.Close()
	s.tunnel.Close()
	s.aggregator.Close()
	s.hub.Close()
	s.monitor.StopAll()
	return s.store.Close()
}

// DataDir reports the prepared data directory, for the admin banner (plan 14).
func (s *Server) DataDir() datadir.Info { return s.dataDir }

// Handler is the root HTTP handler: the router behind StripBasePath, so a
// request under the public URL's path reaches the same routes whether or not a
// proxy stripped the prefix. It runs before chi and every middleware (D-50).
func (s *Server) Handler() http.Handler {
	return httpx.StripBasePath(s.inst.BasePath)(s.router.Handler())
}

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
	srv := newHTTPServer(s.Handler(), s.tlsConfig)
	// Shutdown waits for open requests, and an events stream never ends by
	// itself. Closing the hub ends every stream, so a restart is not held up for
	// the whole shutdown timeout by a browser that has the feed open.
	srv.RegisterOnShutdown(s.aggregator.Close)
	srv.RegisterOnShutdown(s.hub.Close)
	// The health stream is open as long as a tab is, so it ends the same way.
	srv.RegisterOnShutdown(s.broker.Close)
	// http.Server.Shutdown ignores hijacked connections, and every agent
	// tunnel is one. Closing the hub sends each a close frame so the agents
	// reconnect with backoff instead of waiting on a dead session (Pitfall 5).
	srv.RegisterOnShutdown(s.tunnel.Close)
	errc := make(chan error, 1)
	// With a TLS config the certificate is already in it, so the file names are empty.
	scheme := "http"
	if s.tlsConfig != nil {
		scheme = "https"
		go func() { errc <- srv.ServeTLS(ln, "", "") }()
	} else {
		go func() { errc <- srv.Serve(ln) }()
	}
	s.log.Info("bayplan listening", "addr", ln.Addr().String(), "scheme", scheme)

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

// settingsAgents gives the settings routes what they know of agents: the agents
// service counts the enrolled ones, and the hub says which are connected and tells
// them a new address (D-19).
type settingsAgents struct {
	svc *agents.Service
	hub *tunnel.Hub
}

func (a settingsAgents) EnrolledAgentIDs(ctx context.Context) ([]string, error) {
	return a.svc.EnrolledAgentIDs(ctx)
}
func (a settingsAgents) Connected(agentID string) bool    { return a.hub.Session(agentID) != nil }
func (a settingsAgents) PushURLs(urls, pins []string) int { return a.hub.PushURLs(urls, pins) }
func (a settingsAgents) Pins() []string                   { return a.hub.Pins() }

var _ api.SettingsAgents = settingsAgents{}
