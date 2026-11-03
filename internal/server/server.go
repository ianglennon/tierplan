// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package server assembles TierPlan from its configuration. Later plans add
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
	"sync/atomic"
	"time"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/backup"
	"github.com/ianglennon/tierplan/internal/buildinfo"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/datadir"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/gitconn"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/shells"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/internal/webui"
	"github.com/ianglennon/tierplan/web"
)

const (
	// readHeaderTimeout, defaultReadTimeout and idleTimeout bound slow clients
	// (T-01-17, Pitfall 8). There is deliberately no global WriteTimeout: the
	// SSE events stream (plan 21) lives for hours and clears its own write
	// deadline.
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

// defaultReadTimeout bounds reading a whole request, headers and body, so a
// client that sends the headers and then withholds the body is cut off (WR-14,
// T-01-17). Go clears the read deadline once a request with no body is being
// served, so a long-lived response such as the events stream is unaffected.
// newHTTPServer takes it as a parameter, so a test that needs a short one
// builds its own server instead of writing shared state.
const defaultReadTimeout = 30 * time.Second

// maxHeaderBytes caps the request line and headers. The net/http default is
// 1 MiB, which let a caller put megabytes into a path segment (an environment ID
// in a refusal, for one). 64 KiB is far above any real cookie or URL.
const maxHeaderBytes = 64 << 10

// newHTTPServer returns the http.Server TierPlan serves with. huma already caps
// JSON bodies (MaxBodyBytes), so no MaxBytesReader is added here. Production
// passes defaultReadTimeout.
func newHTTPServer(h http.Handler, tlsCfg *tls.Config, readTimeout time.Duration) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsCfg,
	}
}

// Server is the assembled application.
type Server struct {
	// resolver chooses the stored credential a pull or a service write is sent with.
	resolver *registry.Resolver
	// refusals folds a signed-in user's repeated refusals into one audit entry.
	refusals *auth.RefusalCoalescer
	cfg      config.Config
	log      *slog.Logger
	dataDir  datadir.Info
	store    *store.Store
	vault    *vault.Vault
	audit    *audit.Writer
	setup    *auth.SetupGate
	inst     *instance.Instance
	session  *auth.Sessions
	envs     *envs.Service
	monitor  *envs.Monitor
	// drift checks the stacks of the monitored environments for drift in the background.
	drift *stackops.DriftMonitor
	// gitSync checks the git stacks; its checks and automatic deploys run apart from the poller and are ended and
	// waited for by Close before the jobs and the database close.
	gitSync *gitsync.Service
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
	// shells holds the browser shells that are open (plan 04-22). It is memory only,
	// and the shutdown closes every one of them.
	shells *shells.Registry
	// jobs holds the image pulls in progress and the ones that finished a few minutes ago.
	jobs *jobs.Hub
	// stats samples the containers that browsers watch; closed with the server.
	stats *api.StatsHub
	// tunnel holds the live agent connections; agents creates agent
	// environments and enrols agents.
	tunnel  *tunnel.Hub
	agents  *agents.Service
	updater *agents.Updater
	router  *httpx.Router
	// tlsConfig is nil when the server speaks plain HTTP (D-12).
	tlsConfig *tls.Config

	// unlock releases the data-directory lock; Close calls it last.
	unlock func() error
	// keepLock makes Close leave the lock held. A restore sets it before it closes
	// the server: nothing may take the data directory between the database swap and
	// the re-exec, which drops the lock by closing its close-on-exec descriptor.
	keepLock atomic.Bool
	// restart carries a restore's request to restart the process.
	restart *restarter

	// listening is closed by Run once its address is bound, and never when the
	// bind fails. listeningOnce keeps a second Run from closing it twice.
	listening     chan struct{}
	listeningOnce sync.Once

	// stopBackground ends the nightly audit prune and waits for it, so Close
	// never closes the database under a running prune.
	stopBackground context.CancelFunc
	background     sync.WaitGroup
}

// RouterDeps are the dependencies of every route group. One struct, because a
// positional list of this length is where a swapped pair of arguments of the same
// type goes unnoticed (RESEARCH Pitfall 10). The zero value builds the same
// routes with no database or socket, which is what the `openapi` subcommand and
// the router-walk tests use.
type RouterDeps struct {
	Options       httpx.Options
	System        api.SystemDeps
	Setup         api.SetupDeps
	Auth          api.AuthDeps
	Account       api.AccountDeps
	Access        api.AccessDeps
	LocalHost     api.LocalHostDeps
	Environments  api.EnvironmentsDeps
	Agents        api.AgentsDeps
	UnknownAgents api.UnknownAgentsDeps
	Dashboard     api.DashboardDeps
	Containers    api.ContainersDeps
	Networks      api.NetworksDeps
	AddressSpace  api.AddressSpaceDeps
	Janitor       api.JanitorDeps
	Services      api.ServicesDeps
	Updates       api.UpdatesDeps
	RestartLoop   api.RestartLoopDeps
	Secrets       api.SecretsDeps
	Configs       api.ConfigsDeps
	Nodes         api.NodesDeps
	Volumes       api.VolumesDeps
	Images        api.ImagesDeps
	Pulls         api.PullsDeps
	Stacks        api.StacksDeps
	Registries    api.RegistriesDeps
	Git           api.GitDeps
	Tags          api.TagsDeps
	Events        api.EventsDeps
	Logs          api.LogsDeps
	ServiceLogs   api.ServiceLogsDeps
	Stats         api.StatsDeps
	Shells        api.ShellsDeps
	EnvStream     api.EnvStreamDeps
	Recent        api.RecentDeps
	Audit         api.AuditDeps
	Settings      api.SettingsDeps
	Backup        api.BackupDeps
	Restore       api.RestoreDeps
	// GitHooks serves POST /hooks/git/{token}, the address a git host calls on a push. Zero answers 404 to everything.
	GitHooks api.GitHookDeps
	// AgentConnect serves the agent WebSocket at /agent/connect. It is the one
	// route outside the policy besides the probes and the setup page, because the
	// handshake inside it is its own authentication. Nil mounts a handler that
	// answers 404: the route exists in every router, so its exemption is always
	// checked.
	AgentConnect http.Handler
}

// NewRouter wires every operation.
func NewRouter(d RouterDeps) *httpx.Router {
	r := httpx.New(d.Options)
	agentConnect := d.AgentConnect
	if agentConnect == nil {
		agentConnect = http.NotFoundHandler()
	}
	r.AgentConnect(agentConnect)
	api.RegisterSystem(r, d.System)
	api.RegisterSetup(r, d.Setup)
	api.RegisterAuth(r, d.Auth)
	api.RegisterAccount(r, d.Account)
	api.RegisterAccess(r, d.Access)
	api.RegisterSettings(r, d.Settings)
	api.RegisterLocalHost(r, d.LocalHost)
	api.RegisterEnvironments(r, d.Environments)
	api.RegisterAgents(r, d.Agents)
	api.RegisterUnknownAgents(r, d.UnknownAgents)
	api.RegisterEnvWarnings(r, api.EnvWarningsDeps{Envs: d.Environments.Envs, Checker: d.Environments.Checker, Clusters: d.Services.Clusters, Repo: &stackops.Repo{Store: d.Stacks.Store, Vault: d.Stacks.Vault}, Agents: d.Agents, Log: d.Environments.Log})
	api.RegisterDashboard(r, d.Dashboard)
	api.RegisterContainers(r, d.Containers)
	api.RegisterNetworks(r, d.Networks)
	api.RegisterAddressSpace(r, d.AddressSpace)
	api.RegisterJanitor(r, d.Janitor)
	api.RegisterJanitorTaskHistory(r, d.Janitor)
	api.RegisterServices(r, d.Services)
	api.RegisterUpdates(r, d.Updates)
	api.RegisterRestartLoop(r, d.RestartLoop)
	api.RegisterSecrets(r, d.Secrets)
	api.RegisterConfigs(r, d.Configs)
	api.RegisterNodes(r, d.Nodes)
	api.RegisterVolumes(r, d.Volumes)
	api.RegisterImages(r, d.Images)
	api.RegisterPulls(r, d.Pulls)
	api.RegisterStacks(r, d.Stacks)
	api.RegisterRegistries(r, d.Registries)
	api.RegisterGit(r, d.Git)
	api.RegisterGitStacks(r, d.Stacks, d.Git)
	api.RegisterGitHooks(r, d.GitHooks)
	api.RegisterTags(r, d.Tags)
	api.RegisterEvents(r, d.Events)
	api.RegisterLogs(r, d.Logs)
	api.RegisterServiceLogs(r, d.ServiceLogs)
	api.RegisterStats(r, d.Stats)
	api.RegisterShells(r, d.Shells)
	api.RegisterEnvStream(r, d.EnvStream)
	api.RegisterRecent(r, d.Recent)
	api.RegisterAudit(r, d.Audit)
	api.RegisterBackup(r, d.Backup)
	api.RegisterRestore(r, d.Restore)
	return r
}

// Option adjusts Build.
type Option func(*buildOptions)

type buildOptions struct {
	selfContainerID string
	refusalClock    func() time.Time
	addressClock    func() time.Time
	janitorClock    stackops.Clock
	noJanitorSched  bool
}

// WithRefusalClock replaces the clock of the refusal coalescer, which decides
// when a minute of repeated refusals has closed. It exists for tests.
func WithRefusalClock(now func() time.Time) Option {
	return func(o *buildOptions) { o.refusalClock = now }
}

// WithAddressSpaceClock replaces the clock of the address-space reader, which decides
// when a network has been idle long enough to be listed as orphaned or unused. It exists
// for tests, which move it past the grace period instead of waiting for it. It is a Go
// option only: no setting, flag or environment variable reaches it, so a production build,
// which calls Build without it, always reads the real time. Only the reader's clock moves;
// sessions, tokens and every other clock keep real time. A nil clock keeps time.Now.
func WithAddressSpaceClock(now func() time.Time) Option {
	return func(o *buildOptions) { o.addressClock = now }
}

// WithJanitorClock replaces the clock a janitor run judges the age of what it removes by. It exists
// for tests that need containers to be past the grace period at once instead of waiting for it. It
// is a Go option only, like WithAddressSpaceClock: a production build calls Build without it and
// always reads the real time. A nil clock keeps the wall clock.
func WithJanitorClock(c stackops.Clock) Option {
	return func(o *buildOptions) { o.janitorClock = c }
}

// WithoutJanitorScheduler keeps the janitor's scheduler from running, so that no environment is cleaned
// by itself at 03:00. It exists for tests, which must not depend on the time of day they run at. It is
// a Go option only: a production build calls Build without it and every environment's janitor runs on
// its schedule.
func WithoutJanitorScheduler() Option {
	return func(o *buildOptions) { o.noJanitorSched = true }
}

// WithSelfContainerID supplies the ID of the container TierPlan runs in. The
// command line reads it from /proc/self/mountinfo before the privilege drop
// (RESEARCH A9, D-27) and passes it in, so Build never probes /proc itself.
// Without it nothing is badged "this instance".
func WithSelfContainerID(id string) Option {
	return func(o *buildOptions) { o.selfContainerID = id }
}

// checkActiveKey refuses to start on a master key other than the one the
// database was last rotated to (app_meta active_key_id). A database that was
// never rotated names none and is not checked. When the key comes from
// TIERPLAN_ENCRYPTION_KEY the usual cause is a variable that still holds the old
// key, and tierplan.key.new holds the one to use.
func checkActiveKey(ctx context.Context, st *store.Store, vlt *vault.Vault, dataDir string) error {
	active, err := backup.ActiveKeyID(ctx, st)
	if err != nil {
		return fmt.Errorf("read the active key: %w", err)
	}
	if active == "" || active == vlt.KeyID() {
		return nil
	}
	if vlt.KeySource() == "env" {
		newFile := filepath.Join(dataDir, vault.NewKeyFile)
		if key, rerr := vault.ReadKeyFile(dataDir, vault.NewKeyFile); rerr == nil && vault.KeyIDOf(key) == active {
			return fmt.Errorf("the master key was rotated and TIERPLAN_ENCRYPTION_KEY still holds the old one (%s). "+
				"Set TIERPLAN_ENCRYPTION_KEY to the contents of %s, restart TierPlan, then delete the file", vlt.KeyID(), newFile)
		}
		return fmt.Errorf("the master key was rotated to %s but TIERPLAN_ENCRYPTION_KEY holds %s: set it to the key that "+
			"`tierplan key rotate` wrote to %s", active, vlt.KeyID(), newFile)
	}
	return fmt.Errorf("the database was rotated to the master key %s but %s holds %s: restore the key file that "+
		"matches the database (the previous one is kept as %s after a rotation)",
		active, filepath.Join(dataDir, vault.KeyFile), vlt.KeyID(), vault.OldKeyFile)
}

// Build prepares the data directory, loads (or on first start generates) the
// master key, opens and migrates the database and assembles the server. It logs the effective configuration with secrets
// redacted. A migration failure (or a database newer than this binary) is
// returned as an error so `serve` exits non-zero before it listens (D-82, D-84).
func Build(ctx context.Context, cfg config.Config, log *slog.Logger, opts ...Option) (srv *Server, err error) {
	var bo buildOptions
	for _, opt := range opts {
		opt(&bo)
	}
	if log == nil {
		log = slog.Default()
	}
	log.Info("starting tierplan",
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

	// One server per data directory, and no offline `tierplan restore` beside it:
	// the lock is held until Close, and by every error path below it. It comes
	// before the key and the database, so a refused start writes nothing.
	unlock, err := datadir.Lock(info.Path)
	if err != nil {
		return nil, fmt.Errorf("another TierPlan is using the data directory %s: %w", info.Path, err)
	}
	defer func() {
		if err != nil {
			_ = unlock()
		}
	}()

	// A rotation that a crash cut short is finished or discarded before the key
	// is loaded: LoadOrCreate would otherwise mint a fresh key beside a
	// half-swapped pair of key files (D-33).
	recovered, err := vault.RecoverRotation(ctx, info.Path, cfg.EncryptionKey != "", func(ctx context.Context) (string, error) {
		return backup.StoredActiveKeyID(ctx, info.Path)
	})
	if err != nil {
		return nil, fmt.Errorf("recover an interrupted key rotation: %w", err)
	}
	if recovered {
		log.Warn("finished or discarded a master-key rotation that was interrupted")
	}

	// A restore from the web UI leaves its marker when it swaps the database. The
	// restored database gets one start: if it cannot start, the snapshot taken
	// before the swap goes back and the old state starts instead (T-03-105).
	marker, pending, merr := backup.ReadMarker(info.Path)
	if merr != nil {
		log.Error("a restore was pending but its marker cannot be read; starting without the safety net", "error", merr)
		_ = backup.RemoveMarker(info.Path)
		pending = false
	}
	// Nothing staged or uploaded before this start is of any use to it.
	if err := backup.ClearStaging(info.Path); err != nil {
		log.Warn("could not clear the restore staging directory", "error", err)
	}

	srv, err = assemble(ctx, cfg, log, bo, info, unlock, socketPath)
	// Only a failure caused by the restored data goes back to the snapshot. A bad
	// TLS pair, a mistyped key variable or a cancelled start says nothing about
	// the restored database, so the restore keeps its one try for the next start.
	if err != nil && pending && !errors.Is(err, errRestoredState) {
		log.Error("the start failed for a reason unrelated to the restored data; the restore is kept and tried again at the next start", "error", err)
	} else if err != nil && pending {
		log.Error("the restored database could not start; going back to the pre-restore snapshot", "error", err)
		if rbErr := rollbackRestore(ctx, cfg, log, info.Path, marker, err); rbErr != nil {
			return nil, fmt.Errorf("%w (and going back to the snapshot failed: %w)", err, rbErr)
		}
		pending = false
		srv, err = assemble(ctx, cfg, log, bo, info, unlock, socketPath)
	}
	if err != nil {
		return nil, err
	}
	if pending {
		if err := backup.RemoveMarker(info.Path); err != nil {
			log.Warn("restore completed but its marker could not be removed", "error", err)
		} else {
			log.Info("restore completed", "snapshot", marker.Snapshot)
		}
	}
	return srv, nil
}

// errRestoredState marks a start failure that comes from the data a restore put
// in place: the database cannot be opened, is on a key this instance does not
// hold, cannot be migrated or does not load.
var errRestoredState = errors.New("the restored data cannot start")

// restoredStateError wraps err so that errors.Is(err, errRestoredState) holds
// while the message stays exactly err's own.
type restoredStateError struct{ err error }

func (e restoredStateError) Error() string      { return e.err.Error() }
func (e restoredStateError) Unwrap() error      { return e.err }
func (restoredStateError) Is(target error) bool { return target == errRestoredState }

// rollbackRestore puts the pre-restore snapshot back as the database and records
// backup.failed (stage restore_boot) in its audit chain. The audit entry is best
// effort: a rollback that worked is not undone because the note about it did not.
func rollbackRestore(ctx context.Context, cfg config.Config, log *slog.Logger, dir string, m backup.Marker, cause error) error {
	if err := backup.RollbackRestore(dir, m); err != nil {
		return err
	}
	vlt, err := vault.LoadOrCreate(cfg.EncryptionKey, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		log.Error("could not record the failed restore: load the encryption key", "error", err)
		return nil
	}
	st, err := store.Open(ctx, dir)
	if err != nil {
		log.Error("could not record the failed restore: open the database", "error", err)
		return nil
	}
	defer func() { _ = st.Close() }()
	if err := audit.NewWriter(st, vlt, log, nil).Record(ctx, audit.Entry{
		Actor:      audit.SystemRestore,
		Action:     audit.ActionBackupFailed,
		TargetType: "backup",
		Result:     audit.ResultFailure,
		Reason:     "restore_boot",
		Details:    map[string]string{"stage": "restore_boot", "error": cause.Error()},
	}); err != nil {
		log.Error("could not record the failed restore", "error", err)
	}
	return nil
}

// assemble loads the master key, opens and migrates the database and builds the
// server on the prepared, locked data directory. It closes what it opened when it
// fails, and leaves the lock to its caller.
func assemble(ctx context.Context, cfg config.Config, log *slog.Logger, bo buildOptions, info datadir.Info, unlock func() error, socketPath string) (srv *Server, err error) {
	// The master key comes before the database: a bad TIERPLAN_ENCRYPTION_KEY
	// should stop startup before anything is written (D-13).
	vlt, err := vault.LoadOrCreate(cfg.EncryptionKey, info.Path, log)
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}
	log.Info("encryption key loaded", "key_id", vlt.KeyID(), "source", vlt.KeySource())
	log.Info("docker socket", "path", socketPath, "present", engine.SocketExists(socketPath))

	st, err := store.Open(ctx, info.Path)
	if err != nil {
		return nil, restoredStateError{fmt.Errorf("open database: %w", err)}
	}
	// A database rotated to another key cannot be opened with this one, so say
	// so before anything is migrated or written (D-33).
	if err := checkActiveKey(ctx, st, vlt, info.Path); err != nil {
		_ = st.Close()
		return nil, restoredStateError{err}
	}
	res, err := store.Migrate(ctx, st, store.MigrateOptions{
		AppVersion: buildinfo.Get().Version,
		DataDir:    info.Path,
		SkipBackup: cfg.SkipPreMigrateBackup,
	})
	if err != nil {
		_ = st.Close()
		return nil, restoredStateError{fmt.Errorf("migration failed: %w", err)}
	}
	attrs := []any{"from", res.From, "to", res.To}
	if res.SnapshotPath != "" {
		attrs = append(attrs, "snapshot", res.SnapshotPath)
	}
	log.Info("database ready", attrs...)

	// The jobs and the operation lock are in memory, so a deploy that was running when the last process
	// stopped is not running now. It ends interrupted, its revision failed, and its stack reads Deploy
	// failed and offers a roll back (Open Q5).
	interrupted, err := stackops.Repo{Store: st, Vault: vlt}.MarkInterrupted(ctx)
	if err != nil {
		_ = st.Close()
		return nil, restoredStateError{fmt.Errorf("mark interrupted stack deploys: %w", err)}
	}
	if interrupted > 0 {
		log.Warn("stack operations that were running when the server stopped were marked interrupted", "count", interrupted)
	}

	// The Compose SDK that deploys a standalone stack keeps process-wide state (its Docker CLI
	// configuration directory and its logging), set once here and never per deploy. The directory is
	// private to the SDK and always empty, under the data directory.
	if err = dockerx.InitCompose(info.Path, log); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("prepare the Compose SDK: %w", err)
	}

	// /readyz needs a usable data directory and a database at this binary's
	// schema version (D-86).
	ready := func(ctx context.Context) error {
		if err := datadir.CheckWritable(info.Path); err != nil {
			return err
		}
		return st.Ready(ctx)
	}

	aud := audit.NewWriter(st, vlt, log, nil)

	// The janitor's runs are in memory as well as on record: one still waiting or running belongs to a
	// process that is gone. Before anything can start a run, each is ended as failed with the reason
	// server_restarted and audited, so none reads "running" for ever, and then the run history is pruned to
	// the retention, before the scheduler starts (D-16, RESEARCH F12).
	janitorRuns := &janitor.SQLStore{St: st}
	sweepAt := time.Now()
	if bo.janitorClock != nil {
		sweepAt = bo.janitorClock.Now()
	}
	swept, err := janitorRuns.MarkInterruptedRuns(ctx, sweepAt)
	if err != nil {
		_ = st.Close()
		return nil, restoredStateError{fmt.Errorf("mark interrupted janitor runs: %w", err)}
	}
	if len(swept) > 0 {
		log.Warn("janitor runs that were working when the server stopped were marked failed", "count", len(swept))
		janitor.AuditInterrupted(ctx, aud, log, swept)
	}
	// The retention is the one the instance defaults store (D-16), so a limit saved on the settings page
	// holds across a restart. One that cannot be read prunes nothing rather than guessing.
	if keep, rerr := (janitor.PolicyStore{St: st}).Retention(ctx); rerr != nil {
		log.Warn("could not read the janitor run history retention, so the history was not pruned", "error", rerr)
	} else if pruned, perr := janitorRuns.PruneAll(ctx, keep, sweepAt); perr != nil {
		log.Warn("could not prune the janitor run history beyond the retention", "error", perr)
	} else if pruned > 0 {
		log.Info("pruned janitor runs beyond the retention", "count", pruned, "days", keep.Days, "runs", keep.Runs)
	}

	// While no user exists every start mints a fresh setup token, writes it to
	// the data directory and logs it (D-01). There is no setting that skips or
	// prefills it (D-79).
	gate := auth.NewSetupGate(st, info.Path, log)
	if err := gate.Init(ctx); err != nil {
		_ = st.Close()
		return nil, restoredStateError{fmt.Errorf("initialise the setup gate: %w", err)}
	}

	// The instance name and public URL come from the settings table, with
	// TIERPLAN_PUBLIC_URL winning (D-04). The Origin guard reads the cached
	// origin on every mutation, so a change made at setup applies at once.
	inst, err := instance.Load(ctx, st, cfg)
	if err != nil {
		_ = st.Close()
		return nil, restoredStateError{fmt.Errorf("load instance settings: %w", err)}
	}

	// Revisions beyond the retention limit go once at startup, after the deploys that were running are ended
	// (their revisions are failed now, none is pending): a lowered limit, a restored database and a stack
	// that last pruned before this version all reach the limit here (D-01). A failure is logged: a stack
	// with too many revisions still works.
	if pruned, perr := (stackops.Repo{Store: st, Vault: vlt}).PruneAll(ctx, inst.StackRevisionsKept()); perr != nil {
		log.Warn("could not prune the stack revisions beyond the retention limit", "error", perr)
	} else if pruned > 0 {
		log.Info("pruned stack revisions beyond the retention limit", "count", pruned, "kept", inst.StackRevisionsKept())
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
		log.Info("serving plain HTTP; set TIERPLAN_TLS_CERT_FILE and TIERPLAN_TLS_KEY_FILE, TIERPLAN_TLS_SELF_SIGNED=true, or put a TLS-terminating proxy in front")
	case cfg.TLSSelfSigned:
		log.Info("serving HTTPS with a self-signed certificate; compare this fingerprint in your browser",
			"sha256", fingerprint(tlsCfg), "tls_pin", spkiPin(tlsCfg),
			"certificate", filepath.Join(info.Path, selfSignedDir, selfSignedCertFile))
	default:
		log.Info("serving HTTPS", "certificate_file", cfg.TLSCertFile, "sha256", fingerprint(tlsCfg))
	}

	sessions := auth.NewSessions(st, vlt, cfg.SessionIdleTimeout, cfg.SessionRememberLifetime, nil)
	checker := rbac.NewChecker(st)
	accessSvc := access.New(st, aud, checker, cfg.InviteTTL)

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

	// A signed-in user's refusals are different: the first of each kind is written
	// in full before the answer goes out, and identical repeats inside a minute
	// become one "refused xN" entry, so a refreshing tab cannot flood the single
	// write connection or the log (D-20). Everything else, anonymous refusals
	// included, passes through to the anonymous throttle.
	refusals := auth.NewRefusalCoalescer(anonAudit, log, bo.refusalClock)

	// The environment registry and its health monitors. Nothing is added here:
	// the local host becomes an environment only when an admin asks (D-19).
	// TierPlan's own container was identified before the privilege drop and is
	// handed in with WithSelfContainerID (RESEARCH A9).
	monitor := envs.NewMonitor(log, nil, nil)
	selfID := bo.selfContainerID
	selfNamespace := readSelfNamespace(ctx, socketPath, selfID, log)
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
		log.Warn("agents cannot connect: the address they are given is plain http for a host that is not this machine, so agents refuse it and so does TierPlan. Set an https TIERPLAN_PUBLIC_URL or TIERPLAN_AGENT_URL (or change it in Settings); TIERPLAN_AGENT_ALLOW_INSECURE=true allows it, for development only",
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
	// The address reader tells how full the address pools of an environment are. The networks,
	// stacks and dashboard routes read it as well as its own route, so it is built once.
	addressSpaces := envs.NewAddressSpaces(envSvc, aggregator, clusters, envs.AddressSpaceOptions{Now: bo.addressClock, Log: log})
	// The cluster views carry each environment's address space summary, so a swarm that is out of
	// address space is degraded wherever health is shown (D-06).
	clusters.WithAddressSpaces(addressSpaces)
	// The subnet check reads the routes of each node's host: through the agents' tunnels, and for
	// TierPlan's own host from the route file mounted into its container (TIERPLAN_HOST_ROUTE_FILE).
	hostRoutes := envs.NewHostRoutes(envSvc, tunnelHub, envs.HostRoutesOptions{LocalRouteFile: cfg.HostRouteFile, Log: log})
	// A standalone host's pools count the blocks its routes overlap, which its daemon skips.
	addressSpaces.WithHostRoutes(hostRoutes)

	spa := webui.Handler(web.FS(), inst.BasePath)
	// The wizard restore routes share the setup routes' gate, limiter and audit.
	setupDeps := api.SetupDeps{
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
	}
	restart := newRestarter()

	// Scheduled backups go to TIERPLAN_BACKUP_DIR, or the backups folder of the data
	// directory, which the pre-migration and pre-restore snapshots share (D-32).
	backupDir := cfg.BackupDir
	if backupDir == "" {
		backupDir = filepath.Join(info.Path, "backups")
	}
	// A process killed in the middle of a backup leaves the unencrypted copy of the
	// database it was working on; the start removes the old ones.
	backup.SweepWorkFiles(info.Path, backupDir, time.Now(), log)
	backups := &backup.Scheduler{
		Store: st, Vault: vlt, Audit: aud, Dir: backupDir, DataDir: info.Path, Log: log,
		AppVersion:   func() string { return buildinfo.Get().Version },
		InstanceName: inst.Name, PublicURL: inst.PublicURL,
	}

	shellRegistry := shells.NewRegistry(nil)
	// The job hub holds the pulls in progress. It is memory only; the audit log is the
	// record, and Close ends every running job before the database closes (D-26).
	jobHub := jobs.New(jobs.Options{})
	// One sampler hub serves every viewer of any container's stats (D-29 to D-31).
	statsHub := api.NewStatsHub(aggregator, api.StatsHubOptions{Log: log})
	registryStore := registry.NewStore(st, vlt, nil)
	resolver := registry.NewResolver(registryStore)
	// The stack operation lock and the drift results are shared by the stack routes and the background drift check,
	// which looks at each stack of a monitored environment on a timer, on the Docker events that name it and when an
	// operation on it ends, and never while an operation holds the lock (D-22).
	stackLocks, driftStore := stackops.NewLocks(), stackops.NewDriftStore()
	// The orphan check asks the stacks which networks they declare external and whether one holds an operation, so a
	// network a stack uses or is deploying is never listed (NET-04). It opens the stacks with the vault, as every
	// other reader of a stack does.
	netFacts := stackops.NetFacts{Repo: stackops.Repo{Store: st, Vault: vlt}, Locks: stackLocks}
	addressSpaces.WithStackFacts(netFacts)
	// The janitor reads what each stack's own file says (x-tierplan: {keep: true}) with the same loader and deadline.
	janitorFacts := stackops.JanitorFacts{Repo: stackops.Repo{Store: st, Vault: vlt}}
	driftMonitor := stackops.NewDriftMonitor(stackops.DriftMonitorDeps{
		Ops:  stackops.Deps{Aggregator: aggregator, Clusters: clusters, Resolver: resolver, Checker: checker, Log: log},
		Repo: stackops.Repo{Store: st, Vault: vlt}, Store: driftStore, Audit: st, Locks: stackLocks,
		Subscribe: aggregator.EventRows, Log: log,
	})
	// The worker follows an environment by its ID: the local host is announced by the monitor, an agent environment (a
	// swarm, or a host with an agent) by the service when its first agent connects, and its events are the environment's
	// merged feed. The monitor's own keys for an agent's target are not environment IDs (STK-14).
	monitor.Watch(driftMonitor.EnvStarted, driftMonitor.EnvStopped)
	envSvc.WatchAgentEnvironments(driftMonitor.EnvStarted, driftMonitor.EnvStopped)
	// Every call the server makes to a registry dials through one egress policy (T-05-38).
	registryEgress := registry.NewEgressPolicy(cfg.RegistryBlockCIDRs)
	registryClients := registry.NewClientCache(registryEgress, buildinfo.Get().Version, log)
	// Git over HTTPS dials through the same policy. go-git keeps its protocols in a package variable, so the
	// transport is installed once, here, before any route can run a git call (Phase 9, T-09-01).
	gitx.InstallTransport(registryEgress)
	gitClient := gitx.NewGoGit(gitx.Options{Policy: registryEgress, Log: log, Root: filepath.Join(info.Path, "git")})
	gitConns := gitconn.NewStore(st, vlt, nil)
	// Image update checks: the registries are asked through the same clients and egress policy as every other
	// registry call, and the checker and the service share one memory of the indexes workloads run so each is
	// read once (D-02). The service reads the nodes through the aggregator for the platforms a service runs on.
	runIndex := updates.NewRunIndex()
	updateService := &updates.Service{
		Store: updates.SQLStore{Q: st.Q, W: st.Write}, Resolver: resolver, Clusters: clusters, Aggregator: aggregator, Runs: runIndex,
		Checker: &updates.Checker{Heads: updates.NewHeads(resolver, registryClients), Store: updates.SQLStore{Q: st.Q, W: st.Write}, Runs: runIndex, Log: log},
	}
	gitDeps := api.GitDeps{
		Git: gitClient, Conns: gitConns, Envs: envSvc, Audit: aud, Checker: checker, Log: log, Access: accessSvc, Instance: inst,
		Sync: gitsync.New(gitsync.Deps{Git: gitClient, Conns: gitConns, Repo: stackops.Repo{Store: st, Vault: vlt}, Audit: aud, Log: log, Access: accessSvc}),
	}
	// A git stack's webhook and auto-deploy act on their owner's authority while the owner is away (P3 D-14): the
	// kinds are registered before any route can create or run one.
	gitsync.RegisterOwnedKinds()
	// The deploys a git stack makes by itself (auto-deploy) go through the very code the Deploy button does, on the same
	// operation lock and environment lookup as the stack routes. The service that starts them is made before the
	// routes' dependencies (which hold its hooks), so it is given the deployer once both exist.
	// The janitor's Clean now: a run takes the lock the stack operations share and is a job in the same hub.
	// The policy is the stored one: the instance defaults with each environment's overrides over them.
	// Networks are judged by the orphan check the Address space page lists with and removed by the code its delete route
	// calls, so the janitor has one way to judge a network and one way to remove it.
	// A preview judges ages by the same clock a run does, so a test that moves the janitor's clock
	// sees the preview and the run agree. Production builds have none and read the real time.
	janitorNow := time.Now
	if bo.janitorClock != nil {
		janitorNow = bo.janitorClock.Now
	}
	addressSpaceDeps := api.AddressSpaceDeps{AddressSpaces: addressSpaces, Aggregator: aggregator, Clusters: clusters, Store: st, Audit: aud, Checker: checker, HostRoutes: hostRoutes, Now: time.Now, Log: log}
	janitorDeps := api.JanitorDeps{
		Aggregator: aggregator, Clusters: clusters, Store: st, Audit: aud, Checker: checker, Locks: stackLocks, Jobs: jobHub,
		AddressSpaces: addressSpaces, StackFacts: netFacts, JanitorFacts: janitorFacts, Clock: bo.janitorClock, Now: janitorNow, Log: log,
	}
	// One store keeps the record of the runs: the Runner writes it and the run route reads it.
	janitorDeps.Runs = janitorRuns
	janitorPolicies := janitor.PolicyStore{St: st}
	janitorDeps.Policies = janitorPolicies
	// One grace per environment, for everything (D-20): the Address space orphan list judges by the grace the
	// janitor's policy gives the environment, the instance default of 24 hours until someone sets it. It is set
	// before the reader serves its first request.
	addressSpaces.WithGrace(func(ctx context.Context, envID string) (time.Duration, error) {
		p, err := janitorPolicies.Effective(ctx, envID)
		return p.Grace, err
	})
	janitorDeps.Runner = &janitor.Runner{
		Engine: api.NewJanitorEngine(aggregator, clusters, netFacts, janitor.KeepStore{St: st}, log, api.WithStackDeclarations(janitorFacts), api.WithEngineClock(janitorNow)), Locks: stackLocks, Jobs: jobHub, Clock: bo.janitorClock,
		Policy: janitorPolicies.Effective, Nodes: janitorPolicies.Nodes, Retention: janitorPolicies.Retention, Store: janitorRuns, Audit: aud, Log: log,
		Networks: api.NewJanitorNetworks(addressSpaceDeps),
	}
	// The scheduler cleans each enabled environment at its slot, as the system, through the Runner Clean now uses. It
	// reads a policy through the Runner and starts after the sweep of interrupted runs above (RESEARCH F11).
	janitorSched := &janitor.Scheduler{
		Envs: func(ctx context.Context) ([]string, error) {
			rows, err := st.RQ.ListEnvironments(ctx)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(rows))
			for _, row := range rows {
				if row.Enabled != 0 {
					ids = append(ids, row.ID)
				}
			}
			return ids, nil
		},
		Runner: janitorDeps.Runner, Clock: bo.janitorClock, Loc: time.Local, Log: log,
	}
	janitorDeps.Scheduler = janitorSched
	stacksDeps := api.StacksDeps{Aggregator: aggregator, Clusters: clusters, Resolver: resolver, Platforms: stackops.NewPlatformMemo(), Audit: aud, Checker: checker, Jobs: jobHub, Store: st, Vault: vlt, ConvergeTimeout: cfg.StackConvergeTimeout, Locks: stackLocks, AddressSpaces: addressSpaces, Drift: driftStore, DriftRecheck: driftMonitor.Recheck, RevisionsKept: inst.StackRevisionsKept, SelfContainerID: selfID, SelfNamespace: selfNamespace, GitConns: gitConns, GitSync: gitDeps.Sync, GitDeployed: gitDeps.Sync.Deployed, GitRolledBack: gitDeps.Sync.RolledBack, GitDeployFailed: gitDeps.Sync.DeployFailed, Envs: envSvc, Log: log}
	gitDeps.Sync.SetDeployer(api.NewStackDeployer(stacksDeps).GitDeployer())
	router := NewRouter(RouterDeps{
		Options: httpx.Options{
			Fallback:       spa,
			Checker:        checker,
			Authenticator:  auth.NewAuthenticator(st, cfg.SessionIdleTimeout, inst.CookieScope, nil),
			EnvResolver:    envSvc,
			Auditor:        refusals,
			MutationGuard:  auth.NewCSRFGuard(sessions),
			TrustedProxies: cfg.TrustedProxies,
			Proxy:          proxyObs,
			PublicOrigin:   inst.PublicOrigin,
			Logger:         log,
			// Strict-Transport-Security when the request came over TLS or the
			// public URL is https, which also covers a TLS-terminating proxy (D-37).
			// Not for TierPlan's own self-signed certificate: a browser pinned to it
			// could not click through a regenerated one (WR-09).
			HSTS: func(r *http.Request) bool {
				if r.TLS != nil {
					return !cfg.TLSSelfSigned
				}
				return inst.Secure()
			},
		},
		System: api.SystemDeps{Ready: ready},
		Setup:  setupDeps,
		Auth: api.AuthDeps{
			Login:    login,
			Sessions: sessions,
			Instance: inst,
			Store:    st,
			Checker:  checker,
			Log:      log,
		},
		Account: api.AccountDeps{
			Sessions: sessions,
			Instance: inst,
			Store:    st,
			Audit:    aud,
			DataDir:  info,
			Vault:    vlt,
			Proxy:    proxyObs,
			Checker:  checker,
			Refusals: refusals,
			Unknown:  agentSvc.Unknown(),
			// Five password-change attempts per user at once, then one every 12
			// seconds (T-01-60): ample for a person, slow for a guesser.
			Limiter: auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
			Log:     log,
		},
		Access: api.AccessDeps{
			Access: accessSvc, Store: st, Checker: checker, Instance: inst, Sessions: sessions,
			// Opening or using a link: 5 attempts at once, then one every 12 seconds,
			// per client address, like the setup token (T-03-40).
			Limiter:   auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
			AnonAudit: anonAudit,
			Log:       log,
		},
		LocalHost:     api.LocalHostDeps{DockerHost: cfg.DockerHost, Log: log, Envs: envSvc},
		Environments:  api.EnvironmentsDeps{Envs: envSvc, Agents: agentSvc, Instance: inst, TLSPin: tlsPin, AgentAllowInsecure: cfg.AgentAllowInsecure, Checker: checker, Updater: updater, Log: log},
		UnknownAgents: api.UnknownAgentsDeps{Unknown: agentSvc.Unknown(), Store: st, Log: log},
		Agents:        api.AgentsDeps{Agents: agentSvc, Local: envSvc, Live: tunnelHub, Cluster: clusters, Instance: inst, TLSPin: tlsPin, AgentAllowInsecure: cfg.AgentAllowInsecure, Updater: updater, Log: log},
		Dashboard:     api.DashboardDeps{Aggregator: aggregator, Clusters: clusters, Checker: checker, SelfContainerID: selfID, Store: st, AddressSpaces: addressSpaces, Log: log},
		Networks:      api.NetworksDeps{Aggregator: aggregator, Clusters: clusters, AddressSpaces: addressSpaces, Audit: aud, SelfContainerID: selfID, Log: log},
		AddressSpace:  addressSpaceDeps,
		Janitor:       janitorDeps,
		Services:      api.ServicesDeps{Aggregator: aggregator, Clusters: clusters, Resolver: resolver, AddressSpaces: addressSpaces, Audit: aud, Checker: checker, Store: st, Updates: updateService, Log: log},
		Updates:       api.UpdatesDeps{Updates: updateService, Checker: checker, Log: log},
		RestartLoop:   api.RestartLoopDeps{Store: st, Clusters: clusters, Aggregator: aggregator, Audit: aud, Log: log},
		Secrets:       api.SecretsDeps{Aggregator: aggregator, Clusters: clusters, Resolver: resolver, Audit: aud, Checker: checker, Store: st, Log: log},
		Configs:       api.ConfigsDeps{Aggregator: aggregator, Clusters: clusters, Resolver: resolver, Audit: aud, Checker: checker, Store: st, Log: log},
		Nodes:         api.NodesDeps{Aggregator: aggregator, Clusters: clusters, Audit: aud, Checker: checker, Agents: agentSvc, Live: tunnelHub, SelfContainerID: selfID, Log: log},
		Volumes:       api.VolumesDeps{Aggregator: aggregator, Clusters: clusters, Audit: aud, Checker: checker, Log: log},
		Images:        api.ImagesDeps{Aggregator: aggregator, Clusters: clusters, Audit: aud, Checker: checker, SelfContainerID: selfID, Log: log},
		Pulls:         api.PullsDeps{Aggregator: aggregator, Jobs: jobHub, Audit: aud, Checker: checker, Resolver: resolver, Log: log},
		Stacks:        stacksDeps,
		Registries:    api.RegistriesDeps{Store: registryStore, Envs: envSvc, Aggregator: aggregator, Audit: aud, Checker: checker, Egress: registryEgress, Clients: registryClients, Version: buildinfo.Get().Version, Log: log},
		Git:           gitDeps,
		GitHooks:      api.GitHookDeps{Repo: stackops.Repo{Store: st, Vault: vlt}, Sync: gitDeps.Sync, Access: accessSvc, Audit: aud, Log: log},
		Tags:          api.TagsDeps{Resolver: resolver, Tags: registry.NewTagService(registryClients, registryEgress, buildinfo.Get().Version, log), Checker: checker, Aggregator: aggregator, Envs: envSvc, Audit: aud, Log: log},
		Containers:    api.ContainersDeps{Aggregator: aggregator, Audit: aud, Checker: checker, SelfContainerID: selfID, Clusters: clusters, Store: st, Log: log},
		Events:        api.EventsDeps{Aggregator: aggregator, Log: log},
		Logs:          api.LogsDeps{Aggregator: aggregator, Audit: aud, Checker: checker, Log: log},
		ServiceLogs:   api.ServiceLogsDeps{Clusters: clusters, Audit: aud, Checker: checker, Log: log},
		Stats:         api.StatsDeps{Aggregator: aggregator, Clusters: clusters, Hub: statsHub, Log: log},
		Shells:        api.ShellsDeps{Aggregator: aggregator, Registry: shellRegistry, Audit: aud, Checker: checker, Instance: inst, SelfContainerID: selfID, Store: st, Log: log},
		EnvStream:     api.EnvStreamDeps{Broker: broker, Checker: checker, Log: log},
		Recent:        api.RecentDeps{Store: st, Envs: envSvc, Checker: checker, Refusals: refusals, Log: log},
		Audit:         api.AuditDeps{Store: st, RetentionDays: cfg.AuditRetentionDays, Checker: checker, Refusals: refusals, Log: log},
		Settings:      api.SettingsDeps{Instance: inst, Store: st, Audit: aud, Agents: settingsAgents{svc: agentSvc, hub: tunnelHub}, AgentAllowInsecure: cfg.AgentAllowInsecure, Log: log},
		Backup:        api.BackupDeps{Store: st, Vault: vlt, Audit: aud, Instance: inst, DataDir: info.Path, Dir: backupDir, Scheduler: backups, AppVersion: buildinfo.Get().Version, MaxUploadBytes: cfg.BackupMaxUpload, Log: log},
		Restore: api.RestoreDeps{
			Uploads: backup.NewUploads(info.Path, cfg.BackupMaxUpload), Vault: vlt, Audit: aud, Instance: inst,
			Setup: setupDeps, DataDir: info.Path, AppVersion: buildinfo.Get().Version, Log: log,
			// Five tries at once, then one every 12 seconds, per signed-in user.
			Limiter: auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, nil),
			Restart: func(swap func(context.Context) error) bool { return restart.request(RestartRequest{Swap: swap}) },
		},
		AgentConnect: tunnelHub.Handler(),
	})
	// A route that skipped the policy seam stops startup, before anything listens (D-44).
	if err := httpx.VerifyAllGuarded(router); err != nil {
		jobHub.Close()
		statsHub.Close()
		tunnelHub.Close()
		aggregator.Close()
		hub.Close()
		monitor.StopAll()
		_ = st.Close()
		return nil, err
	}
	srv = &Server{
		unlock:     unlock,
		restart:    restart,
		listening:  make(chan struct{}),
		refusals:   refusals,
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
		drift:      driftMonitor,
		gitSync:    gitDeps.Sync,
		snapshots:  snapshots,
		hub:        hub,
		clusters:   clusters,
		aggregator: aggregator,
		broker:     broker,
		shells:     shellRegistry,
		jobs:       jobHub,
		stats:      statsHub,
		resolver:   resolver,
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
	// Scheduled backups wait for the next instant the saved schedule names.
	srv.background.Go(func() { backups.Run(bg) })
	// Expired sessions are refused on lookup; this hourly sweep only keeps the table small.
	srv.background.Go(func() { auth.RunSessionCleanup(bg, st, cfg.SessionIdleTimeout, nil) })
	// Suppressed anonymous refusals are written every minute, and once more when
	// bg ends, which is before Close shuts the database.
	srv.background.Go(func() { anonAudit.Run(bg, time.Minute) })
	srv.background.Go(func() { refusals.Run(bg, 10*time.Second) })
	srv.background.Go(func() { agentAudit.Run(bg, time.Minute) })
	srv.background.Go(func() { broker.Run(bg) })
	srv.background.Go(func() { clusters.Run(bg) })
	// Every environment's address space and orphan list are read every minute and after network, container
	// and service events, so the overview, the health and the last-seen times of networks stay current
	// when nobody has the page open (D-09).
	srv.background.Go(func() { addressSpaces.Run(bg) })
	// Every environment's janitor runs at its slot, daily at 03:00 server time by default (JAN-01).
	if !bo.noJanitorSched {
		srv.background.Go(func() { janitorSched.Run(bg) })
	}
	// Git stacks are checked against their repositories on their interval, for as long as the server runs (D-14).
	srv.background.Go(func() { gitDeps.Sync.RunPoller(bg, gitsync.PollerDeps{Clock: stackops.RealClock()}) })
	// The mirrors of those repositories are a cache: the ones whose repository is gone, that nothing has used for
	// 14 days or that grew past 2 GiB are removed once a day, and made again on their next use (D-08).
	srv.background.Go(func() { gitDeps.Sync.RunMirrorGC(bg, gitsync.GCDeps{Clock: stackops.RealClock()}) })
	// The drift check starts before the environments do, so it hears of each of them.
	driftMonitor.Start(bg)
	// Monitoring resumes for the environments an admin already added. The
	// monitors live as long as bg, and Close waits for them.
	if err := envSvc.StartAll(bg); err != nil {
		stop()
		srv.background.Wait()
		srv.shutdownGitSync()
		driftMonitor.Stop()
		jobHub.Close()
		statsHub.Close()
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

// Refusals is the coalescer of signed-in users' refusals. Tests sweep it to close
// a window without waiting a minute.
func (s *Server) Refusals() *auth.RefusalCoalescer { return s.refusals }

// Vault is the loaded master key, for the dependencies later plans add.
func (s *Server) Vault() *vault.Vault { return s.vault }

// Resolver is the registry credential resolver: AuthFor gives the header for an image.
func (s *Server) Resolver() *registry.Resolver { return s.resolver }

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

// Aggregator is the read and write path to one node's engine. Tests drive
// node-targeted calls through it.
func (s *Server) Aggregator() *envs.Aggregator { return s.aggregator }

// Snapshots is the per-environment resource snapshot cache; the events feed
// invalidates it when Docker reports a change.
func (s *Server) Snapshots() *envs.Snapshots { return s.snapshots }

// Hub is the per-environment Docker events hub.
func (s *Server) Hub() *envs.Hub { return s.hub }

// Clusters is the cluster reader of swarm environments. The server reads every
// environment itself every 15 seconds; a test that needs a read at a given moment
// calls Refresh.
func (s *Server) Clusters() *envs.Clusters { return s.clusters }

// TLSPin is the SPKI pin ("sha256/" and base64) of the certificate TierPlan
// generated for itself, which an install command carries so the agent can trust
// it (D-06). It is empty when TierPlan serves a supplied certificate or plain
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

// gitSyncGrace is how long Close waits for the checks of git stacks to end once they are told to. A fetch that is
// cancelled ends within a moment; the wait only runs out for a call that does not look at its context.
const gitSyncGrace = 15 * time.Second

// shutdownGitSync ends the detached git checks and waits for them for gitSyncGrace; a wait that runs out is logged
// and Close goes on.
func (s *Server) shutdownGitSync() {
	if s.gitSync == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitSyncGrace)
	defer cancel()
	if err := s.gitSync.Shutdown(ctx); err != nil {
		s.log.Warn("a git check was still running when the server closed", "error", err)
	}
}

// Close stops background work, including the events streams and the
// environment monitors, and releases the database. Run calls it on exit; it is
// safe to call again.
func (s *Server) Close() error {
	s.stopBackground()
	s.background.Wait()
	// The checks of git stacks, and the automatic deploys that follow them, run apart from the poller (and a webhook
	// starts them too). They are ended and waited for before the jobs and the database close, so none writes to either
	// afterwards.
	s.shutdownGitSync()
	// Before the database closes: a check in flight reads it.
	s.drift.Stop()
	// Before the database closes, so the end of each shell is still audited.
	s.shells.CloseAll("server_shutdown")
	// Pulls in flight are cancelled and waited for, so their audit rows are written.
	s.jobs.Close()
	// Before the aggregator, so no sample is asked of an engine that is going away.
	s.stats.Close()
	s.broker.Close()
	s.updater.Close()
	s.tunnel.Close()
	s.aggregator.Close()
	s.hub.Close()
	s.monitor.StopAll()
	// The lock goes last: until the database is closed, an offline restore must
	// still be kept out. A restore that is about to swap the database and re-exec
	// keeps it, so no one can take the directory in between (T-03-106).
	err := s.store.Close()
	if s.keepLock.Load() {
		return err
	}
	return errors.Join(err, s.unlock())
}

// DataDir reports the prepared data directory, for the admin banner (plan 14).
func (s *Server) DataDir() datadir.Info { return s.dataDir }

// Routes lists every route the server registered behind the policy, with the
// requirement each one declares. The enforcement test calls every one of them as
// every principal archetype, so a route cannot be added without it being proven
// refused for whoever lacks its requirement.
func (s *Server) Routes() []httpx.RouteRecord { return s.router.Routes() }

// Handler is the root HTTP handler: the router behind StripBasePath, so a
// request under the public URL's path reaches the same routes whether or not a
// proxy stripped the prefix. It runs before chi and every middleware (D-50).
func (s *Server) Handler() http.Handler {
	return httpx.StripBasePath(s.inst.BasePath)(s.router.Handler())
}

// Run serves until ctx is cancelled, then shuts down gracefully within 10s. When
// a restore asks for a restart it stops serving the same way, closes the database
// but not the data-directory lock, and returns a *RestartError (errors.Is
// ErrRestart); the caller then runs Restart.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s (TIERPLAN_LISTEN): %w", s.cfg.Listen, err)
	}
	s.listeningOnce.Do(func() { close(s.listening) })
	return s.run(ctx, ln)
}

// Listening returns a channel that Run closes once it has bound its address, so
// a caller that started Run in a goroutine learns that the server is accepting
// connections from the server itself rather than from a timer. It stays open
// when the address cannot be bound; Run's error says why.
func (s *Server) Listening() <-chan struct{} { return s.listening }

func (s *Server) run(ctx context.Context, ln net.Listener) error {
	err := s.serve(ctx, ln)
	req, restart := s.restart.take()
	if !restart {
		return err
	}
	if ctx.Err() != nil {
		// A stop signal arrived as well. A process that was told to stop does not
		// start another one; the staged restore is cleared at the next start.
		s.log.Warn("a restore asked for a restart but TierPlan is stopping; the restore was not applied")
		return err
	}
	if err != nil {
		s.log.Warn("the server stopped with an error while a restore was waiting for it", "error", err)
	}
	s.keepLock.Store(true)
	if cerr := s.Close(); cerr != nil {
		s.log.Warn("closing the server for a restore", "error", cerr)
	}
	return &RestartError{Request: req}
}

// httpServer is the http.Server the process serves with: production's read
// timeout, the TLS settings the configuration chose.
func (s *Server) httpServer() *http.Server {
	return newHTTPServer(s.Handler(), s.tlsConfig, defaultReadTimeout)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	srv := s.httpServer()
	// Shutdown waits for open requests, and an events stream never ends by
	// itself. Closing the hub ends every stream, so a restart is not held up for
	// the whole shutdown timeout by a browser that has the feed open.
	srv.RegisterOnShutdown(s.aggregator.Close)
	srv.RegisterOnShutdown(s.hub.Close)
	// The health stream is open as long as a tab is, so it ends the same way.
	srv.RegisterOnShutdown(s.broker.Close)
	// http.Server.Shutdown ignores hijacked connections, and every browser shell is
	// one. Closing the registry tells each attached browser why and ends its process
	// attach, so a shell does not outlive the shutdown.
	srv.RegisterOnShutdown(func() { s.shells.CloseAll("server_shutdown") })
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
	// The message and the "addr" key are a contract: scripts/smoke.sh binds port 0
	// and reads the port the kernel assigned from this line (WR-07). ln.Addr is the
	// bound address, so ":0" shows as the real port.
	s.log.Info("tierplan listening", "addr", ln.Addr().String(), "scheme", scheme)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return s.shutdown(srv, false)
	case <-s.restart.requested():
		// The request that asked for the restart is still being answered; Shutdown
		// waits for it, so the browser gets its 202 before the listener goes.
		return s.shutdown(srv, true)
	}
}

// shutdown stops the HTTP server gracefully. For a restart a connection that does
// not finish in time is cut instead of failing the restart: the process is about
// to be replaced anyway.
func (s *Server) shutdown(srv *http.Server, restarting bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	// Shutdown runs the shell hook on a goroutine it does not wait for. CloseAll
	// waits for every shell's end to be audited, so nothing is lost when the
	// database closes after serve returns.
	s.shells.CloseAll("server_shutdown")
	if err != nil {
		if restarting {
			s.log.Warn("some connections were still open at the restart and were closed", "error", err)
			_ = srv.Close()
			return nil
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
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
