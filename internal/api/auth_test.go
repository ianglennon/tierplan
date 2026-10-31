// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/coaming/internal/access"
	"github.com/ianglennon/coaming/internal/agents"
	"github.com/ianglennon/coaming/internal/api"
	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/auth"
	"github.com/ianglennon/coaming/internal/config"
	"github.com/ianglennon/coaming/internal/datadir"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/gitconn"
	"github.com/ianglennon/coaming/internal/gitsync"
	"github.com/ianglennon/coaming/internal/gitx"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/instance"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/registry"
	"github.com/ianglennon/coaming/internal/shells"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
	"github.com/ianglennon/coaming/internal/store/storetest"
	"github.com/ianglennon/coaming/internal/vault"
)

// Sign-in, sign-out and /api/me (plan 12, AUTH-02, AUTH-03, AUTH-08). Negative
// cases are mandatory (D-81).

const (
	authUA     = "auth-test/1.0"
	authIP     = "203.0.113.9"
	authIdle   = 12 * time.Hour
	authRemain = 30 * 24 * time.Hour
)

var authHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(goodPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// syncBuffer is the server's log as a test reads it: handlers write it from their own
// goroutines while the test looks, so it takes a lock.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type authFixture struct {
	h       http.Handler
	st      *store.Store
	v       *vault.Vault
	aud     *audit.Writer
	sess    *auth.Sessions
	inst    *instance.Instance
	clk     *fakeClock
	logs    *syncBuffer
	limiter *auth.IPLimiter
	// dataInfo and acctVault feed the account routes (plan 14). A test sets them
	// before calling build; the defaults are a persisted directory and the
	// environment-keyed vault.
	dataInfo datadir.Info
	// backupDir is where the scheduled backups are listed from (plan 03-27); it is
	// not made until a test writes into it.
	backupDir string
	// scheduler is told when a test saves the backup schedule.
	scheduler interface{ Notify() }
	acctVault *vault.Vault
	// proxy feeds the proxy items of GET /api/warnings (plan 02-25); nil means none.
	proxy *httpx.ProxyObserver
	// refusals feeds the refusal_burst items of GET /api/warnings (plan 03-10); nil means none.
	refusals *auth.RefusalCoalescer
	// pwLimiter is the per-user throttle on POST /api/me/password; nil means none.
	pwLimiter *auth.IPLimiter
	// localHost feeds GET /api/local-host (plan 15).
	localHost api.LocalHostDeps
	// registryEgress is the policy the credential tests dial through (plan 05-11); a test
	// that talks to a registry of its own sets one that trusts that registry's certificate.
	registryEgress registry.EgressPolicy
	// gitClient is the client behind the git test routes (plan 09-01); nil builds a go-git one that
	// dials through registryEgress.
	gitClient gitx.Client
	// gitOnCheck is told when a sync check a request started has ended (plan 09-14); gitSync is the service behind
	// the git routes, set when the routes are built.
	gitOnCheck func(stackID string, outcome gitsync.Outcome, err error)
	gitSync    *gitsync.Service
	// gitTriggered is told after each check a webhook delivery started, and whether another follows (plan 09-22).
	gitTriggered func(stackID string, outcome gitsync.Outcome, err error, more bool)
	// gitJoined is told when a caller starts or joins the check of a stack (plan 09-14, 09-22).
	gitJoined func(stackID string, waiting int)
	// envSvc, when set, is the environment registry behind the environment
	// routes and the {envId} resolver (plan 16). Without it, no environment
	// exists and the environment routes are never called.
	envSvc *envs.Service
	// snaps, when set with envSvc, is the snapshot cache behind the summary and
	// list routes (plan 18); agg is the aggregator over it that the routes read
	// through (plan 02-11).
	snaps *envs.Snapshots
	agg   *envs.Aggregator
	// hub, when set with envSvc, is the events hub behind the events routes
	// (plan 21); eventsHeartbeat and eventsMaxStreams tune them for a test.
	hub              *envs.Hub
	eventsHeartbeat  time.Duration
	eventsMaxStreams int
	// eventsMaxLifetime caps how long one stream may live; zero is the default.
	eventsMaxLifetime time.Duration
	// jobHub holds the pulls (plan 05-01); it is made when a test builds the routes.
	// stackCheckDeadline bounds one check of a stack and stackOnCheck is called as a check starts
	// (plan 07-06); zero and nil are the defaults.
	stackCheckDeadline time.Duration
	stackOnCheck       func(context.Context)
	// stackOnChanges is called as a read of a revision's changes holds its slot (plan 08-04).
	stackOnChanges func(context.Context)
	// stackOnAdoptCheck is called as an adopt check holds its slot (plan 08-14).
	stackOnAdoptCheck func(context.Context)
	// stackOnDrift is called as a read of a stack for drift holds its slot (plan 08-19).
	stackOnDrift func(context.Context)
	// stackDriftRecheck is called when an operation on a stack ends, as the background drift check is asked (plan 08-21).
	stackDriftRecheck func(envID, stackID string)
	// stackLocks is the operation lock the stack routes and a StackDeployer share; stackDeps is what the routes
	// were registered with (set by build), for a test that deploys without a request (plan 09-03).
	stackLocks *stackops.Locks
	stackDeps  api.StacksDeps
	// stackBeforeJob is called in a deploy just before its job starts (plan 07-18).
	stackBeforeJob func(context.Context)
	// stackReadEnv is called before the stacks of an environment are read for the All stacks list (plan 07-24).
	stackReadEnv func(context.Context, string) error
	// stackUp replaces the Compose SDK's up in a standalone deploy (plan 07-20).
	stackUp func(context.Context, *dockerx.Client, *types.Project, dockerx.ComposeUpOptions, func(dockerx.ComposeEvent)) error
	// stackStop and stackStart replace the Compose SDK's stop and start of a standalone stack (plan 07-26).
	stackStop, stackStart func(context.Context, *dockerx.Client, *types.Project) error
	// stackDown replaces the Compose SDK's down in the removal of a standalone stack (plan 07-27).
	stackDown func(context.Context, *dockerx.Client, string) error
	// pullsHeartbeat, pullsMaxStreams and pullsMaxLifetime tune the pull stream, and
	// pullsNodeTimeout the longest one node may pull; zero is the default.
	jobHub           *jobs.Hub
	pullsHeartbeat   time.Duration
	pullsMaxStreams  int
	pullsMaxLifetime time.Duration
	pullsNodeTimeout time.Duration
	// logsHeartbeat, logsMaxStreams and logsMaxLifetime tune the container log stream
	// (plan 04-19); zero is the default. logsWrap, when set, wraps the aggregator the
	// stream reads through, so a test can make the node refuse as a busy agent does.
	logsHeartbeat   time.Duration
	logsMaxStreams  int
	logsMaxLifetime time.Duration
	logsWrap        func(api.LogsSource) api.LogsSource
	// serviceLogsHeartbeat, serviceLogsMaxStreams and serviceLogsMaxLifetime tune the service
	// log stream (plan 06-29); zero is the default.
	serviceLogsHeartbeat   time.Duration
	serviceLogsRefresh     time.Duration
	serviceLogsMaxStreams  int
	serviceLogsMaxLifetime time.Duration
	// serviceLogsBurstEnded is called when a service log stream has taken its opening burst as
	// complete (newSvcLogsFix sets it, and tests await it through svcLogsFix.awaitBurstEnd).
	serviceLogsBurstEnded func()
	// stackLogs tunes the log stream and the download of a stack (plan 07-29); zero is the default.
	// Its Now is the fixture's clock unless a test sets one.
	stackLogs api.StackLogsTuning
	// serviceLogsMaxPageBytes, serviceLogsMaxDownloadBytes and serviceLogsMaxDownloads tune the
	// page and the download of a service's log (plan 06-31).
	serviceLogsMaxPageBytes     int
	serviceLogsMaxDownloadBytes int
	serviceLogsMaxDownloads     int
	// statsHub, when set, is the sampler behind the container stats stream (plan 06-32);
	// statsHeartbeat, statsMaxStreams and statsMaxLifetime tune the stream, zero is the default.
	statsHub         *api.StatsHub
	statsHeartbeat   time.Duration
	statsMaxStreams  int
	statsMaxLifetime time.Duration
	// logsMaxPageBytes shortens how much one page of earlier lines may hold (plan 04-20).
	logsMaxPageBytes int
	// volumeSizesTimeout shortens the per-node time limit of the volume sizes route.
	volumeSizesTimeout time.Duration
	// shellsReg is the registry behind the shell routes (plan 04-22); build makes it on
	// first use and keeps it across builds. shellsPending and shellsControls shorten the
	// time a session waits for its browser and tune the byte bridge.
	shellsReg      *shells.Registry
	shellsPending  time.Duration
	shellsControls shells.Controls
	// brk, when set with envSvc, is the health broker behind GET
	// /api/environments/stream (plan 02-12); envStreamHeartbeat, envStreamMaxStreams
	// and envStreamMaxLifetime tune that stream for a test.
	brk                  *envs.Broker
	envStreamHeartbeat   time.Duration
	envStreamMaxStreams  int
	envStreamMaxLifetime time.Duration
	// auditRetention feeds GET /api/audit (plan 24).
	auditRetention int
	// tlsPin is the pin Coaming's own self-signed certificate would have; empty
	// means it serves none (plan 02-09).
	tlsPin string
	// agentAllowInsecure is the server's COAMING_AGENT_ALLOW_INSECURE as the
	// environment, agent and settings routes see it; false unless a test sets it
	// before build (plans 02-42, 02-45).
	agentAllowInsecure bool
	// agentSvc and liveAgents, when set, replace the agents service the agent routes
	// use and say which tunnel hub the node list reads live state from (plan 02-15).
	// A test with a real hub sets them to the tunneltest server's, so a revoke closes
	// a real session.
	agentSvc   *agents.Service
	liveAgents api.AgentSessions
	// clusters is the cluster reader of a swarm fixture; nil elsewhere.
	clusters *envs.Clusters
	// agentCluster says which agent leads a swarm, for the role in the node list.
	agentCluster api.AgentCluster
	// updater and serverVersion are what the agent update routes use, when set.
	updater       *agents.Updater
	serverVersion string
	// settingsAgents, when set, is what the settings routes count agents with and
	// push new addresses through (plan 02-26).
	settingsAgents api.SettingsAgents
	// envWarningsNow is the clock of GET /api/environments/warnings; nil means the wall clock.
	envWarningsNow func() time.Time
	// inviteTTL is COAMING_INVITE_TTL as the access service sees it; zero is the 72 hour
	// default. inviteLimiter throttles the public invite routes per address and
	// inviteAudit records their refusals; nil means a generous limiter and the writer
	// itself (plan 03-11).
	inviteTTL     time.Duration
	inviteLimiter *auth.IPLimiter
	inviteAudit   httpx.Auditor
	// access is the service behind the user and invite routes, built with the router.
	access *access.Service
	// restore is what the restore routes (plan 03-25) restart through and keep
	// their upload in; build creates it on first use and keeps it across builds.
	restore *restoreHook
	// restoreBurst narrows the restore routes' per-user limiter for this fixture
	// when set before a build; 0 keeps restoreLimiterBurst.
	restoreBurst int
	// unknown is the registry of agents refused unknown_agent that the unknown-agents
	// route and the warnings read (plan 03-28); newAuthFixture makes one.
	unknown *agents.UnknownAgents
}

func newAuthFixture(t *testing.T, cfg config.Config) *authFixture {
	t.Helper()
	f := &authFixture{st: storetest.New(t), clk: &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, logs: &syncBuffer{}, unknown: agents.NewUnknownAgents()}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	f.acctVault = v
	f.dataInfo = datadir.Info{Path: t.TempDir(), Persisted: true}
	f.backupDir = filepath.Join(t.TempDir(), "backups")
	f.aud = audit.NewWriter(f.st, v, log, f.clk.now)
	if f.inst, err = instance.Load(context.Background(), f.st, cfg); err != nil {
		t.Fatal(err)
	}
	f.sess = auth.NewSessions(f.st, v, authIdle, authRemain, f.clk.now)
	f.limiter = auth.NewIPLimiter(time.Second, 1000, 10*time.Minute, f.clk.now)
	f.addUser(t, "admin", true)
	f.h = f.build(t)
	return f
}

// build assembles the router from the fixture's long-lived parts, the way
// server.Build does. Calling it again is a restart: everything in memory is
// new, everything in the database is kept.
func (f *authFixture) build(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	login := auth.NewLogin(f.st, f.sess, f.aud, 5, 30*time.Minute, f.limiter, log, f.clk.now)
	checker := rbac.NewChecker(f.st)
	opts := httpx.Options{
		Checker:       checker,
		Authenticator: auth.NewAuthenticator(f.st, authIdle, f.inst.CookieScope, f.clk.now),
		Auditor:       f.aud,
		MutationGuard: auth.NewCSRFGuard(f.sess),
		PublicOrigin:  f.inst.PublicOrigin,
		Logger:        log,
	}
	if f.envSvc != nil {
		opts.EnvResolver = f.envSvc
	}
	r := httpx.New(opts)
	api.RegisterAuth(r, api.AuthDeps{Login: login, Sessions: f.sess, Instance: f.inst, Store: f.st, Checker: checker, Log: log})
	api.RegisterAccount(r, api.AccountDeps{
		Sessions: f.sess, Instance: f.inst, Store: f.st, Audit: f.aud,
		DataDir: f.dataInfo, Vault: f.acctVault, Limiter: f.pwLimiter, Proxy: f.proxy, Log: log,
		Checker: checker, Refusals: refusalSource(f.refusals), Now: f.clk.now, Unknown: f.unknown,
	})
	f.access = access.New(f.st, f.aud, checker, f.inviteTTL)
	f.access.Now = f.clk.now
	inviteLimiter := f.inviteLimiter
	if inviteLimiter == nil {
		inviteLimiter = auth.NewIPLimiter(time.Second, 1000, 10*time.Minute, f.clk.now)
	}
	api.RegisterAccess(r, api.AccessDeps{
		Access: f.access, Store: f.st, Checker: checker, Instance: f.inst, Sessions: f.sess,
		Limiter: inviteLimiter, AnonAudit: f.inviteAudit, Now: f.clk.now, Log: log,
	})
	api.RegisterSettings(r, api.SettingsDeps{Instance: f.inst, Store: f.st, Audit: f.aud, Agents: f.settingsAgents, AgentAllowInsecure: f.agentAllowInsecure, Log: log})
	api.RegisterLocalHost(r, f.localHost)
	api.RegisterEnvironments(r, api.EnvironmentsDeps{
		Envs: f.envSvc, Agents: agents.NewService(f.st, f.v, f.aud, f.clk.now), Instance: f.inst,
		TLSPin: func() string { return f.tlsPin }, AgentAllowInsecure: f.agentAllowInsecure, Checker: checker, Updater: f.updater, Log: log,
	})
	agentSvc := f.agentSvc
	if agentSvc == nil {
		agentSvc = agents.NewService(f.st, f.v, f.aud, f.clk.now)
	}
	var foldedNodes api.FoldedNodes
	if f.envSvc != nil {
		foldedNodes = f.envSvc
	}
	api.RegisterAgents(r, api.AgentsDeps{
		Agents: agentSvc, Local: foldedNodes, Live: f.liveAgents, Cluster: f.agentCluster, Instance: f.inst, TLSPin: func() string { return f.tlsPin },
		AgentAllowInsecure: f.agentAllowInsecure,
		Updater:            f.updater, ServerVersion: f.serverVersion, Log: log,
	})
	gitx.InstallTransport(registry.NewEgressPolicy(nil))
	gitClient := f.gitClient
	if gitClient == nil {
		gitClient = gitx.NewGoGit(gitx.Options{Policy: f.registryEgress, Log: log, Root: t.TempDir()})
	}
	gitConns := gitconn.NewStore(f.st, f.v, f.clk.now)
	gitDeps := api.GitDeps{
		Git: gitClient, Conns: gitConns, Envs: f.envSvc, Audit: f.aud, Checker: checker, Log: log, Now: f.clk.now, OnCheck: f.gitOnCheck,
		Access: f.access, Instance: f.inst,
		Sync: gitsync.New(gitsync.Deps{
			Git: gitClient, Conns: gitConns, Repo: stackops.Repo{Store: f.st, Vault: f.v}, Audit: f.aud, Log: log, Now: f.clk.now,
			Access: f.access, Triggered: f.gitTriggered, Joined: f.gitJoined,
		}),
	}
	gitsync.RegisterOwnedKinds()
	f.gitSync = gitDeps.Sync
	if f.envSvc != nil {
		api.RegisterEnvWarnings(r, api.EnvWarningsDeps{
			Envs: f.envSvc, Checker: checker, Clusters: f.clusters, Repo: &stackops.Repo{Store: f.st, Vault: f.v}, Now: f.envWarningsNow, Log: log,
			Agents: api.AgentsDeps{
				Agents: agentSvc, Live: f.liveAgents, Instance: f.inst, TLSPin: func() string { return f.tlsPin },
				AgentAllowInsecure: f.agentAllowInsecure,
				Updater:            f.updater, ServerVersion: f.serverVersion,
			},
		})
	}
	if f.envSvc != nil && f.agg != nil {
		api.RegisterDashboard(r, api.DashboardDeps{Aggregator: f.agg, Clusters: f.clusters, Checker: checker, SelfContainerID: f.envSvc.SelfContainerID(), Store: f.st, Log: log})
		api.RegisterContainers(r, api.ContainersDeps{Aggregator: f.agg, Audit: f.aud, Checker: checker, SelfContainerID: f.envSvc.SelfContainerID(), Clusters: f.clusters, Store: f.st, Log: log})
		// The address reader of a real server. A fixture of one standalone engine has no cluster
		// reader; the reader needs one to tell a swarm from a host, so it gets a bare one.
		addrClusters := f.clusters
		if addrClusters == nil {
			addrClusters = envs.NewClusters(f.envSvc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
		}
		addressSpaces := envs.NewAddressSpaces(f.envSvc, f.agg, addrClusters, envs.AddressSpaceOptions{Now: f.clk.now, Log: log})
		api.RegisterAddressSpace(r, api.AddressSpaceDeps{
			AddressSpaces: addressSpaces, Aggregator: f.agg, Clusters: addrClusters, Store: f.st, Audit: f.aud, Checker: checker, Now: f.clk.now, Log: log,
		})
		api.RegisterNetworks(r, api.NetworksDeps{Aggregator: f.agg, Clusters: f.clusters, AddressSpaces: addressSpaces, Audit: f.aud, SelfContainerID: f.envSvc.SelfContainerID(), Log: log})
		api.RegisterServices(r, api.ServicesDeps{
			Aggregator: f.agg, Clusters: f.clusters, Resolver: registry.NewResolver(registry.NewStore(f.st, f.v, f.clk.now)),
			Audit: f.aud, Checker: checker, Store: f.st, Log: log,
		})
		api.RegisterRestartLoop(r, api.RestartLoopDeps{Store: f.st, Clusters: f.clusters, Aggregator: f.agg, Audit: f.aud, Log: log})
		rotateResolver := registry.NewResolver(registry.NewStore(f.st, f.v, f.clk.now))
		api.RegisterSecrets(r, api.SecretsDeps{Aggregator: f.agg, Clusters: f.clusters, Resolver: rotateResolver, Audit: f.aud, Checker: checker, Store: f.st, Log: log})
		api.RegisterConfigs(r, api.ConfigsDeps{Aggregator: f.agg, Clusters: f.clusters, Resolver: rotateResolver, Audit: f.aud, Checker: checker, Store: f.st, Log: log})
		api.RegisterNodes(r, api.NodesDeps{
			Aggregator: f.agg, Clusters: f.clusters, Audit: f.aud, Checker: checker, Agents: agentSvc, Live: f.liveAgents,
			SelfContainerID: f.envSvc.SelfContainerID(), Log: log,
		})
		api.RegisterVolumes(r, api.VolumesDeps{Aggregator: f.agg, Clusters: f.clusters, Audit: f.aud, Checker: checker, SizesTimeout: f.volumeSizesTimeout, Log: log})
		api.RegisterImages(r, api.ImagesDeps{Aggregator: f.agg, Clusters: f.clusters, Audit: f.aud, Checker: checker, SelfContainerID: f.envSvc.SelfContainerID(), Log: log})
		if f.jobHub == nil {
			f.jobHub = jobs.New(jobs.Options{})
			t.Cleanup(f.jobHub.Close)
		}
		api.RegisterPulls(r, api.PullsDeps{
			Aggregator: f.agg, Jobs: f.jobHub, Audit: f.aud, Checker: checker, Heartbeat: f.pullsHeartbeat,
			Resolver:          registry.NewResolver(registry.NewStore(f.st, f.v, f.clk.now)),
			MaxStreamsPerUser: f.pullsMaxStreams, MaxLifetime: f.pullsMaxLifetime, NodeTimeout: f.pullsNodeTimeout, Log: log,
		})
		stackLogs := f.stackLogs
		if stackLogs.Now == nil {
			stackLogs.Now = f.clk.now
		}
		stackDeps := api.StacksDeps{
			Logs:       stackLogs,
			Aggregator: f.agg, Clusters: f.clusters, Resolver: registry.NewResolver(registry.NewStore(f.st, f.v, f.clk.now)),
			// One memo for every stack route, as the server builds it: a deploy that follows a review asks no registry for platforms.
			Platforms: stackops.NewPlatformMemo(),
			Audit:     f.aud, Checker: checker, Jobs: f.jobHub, Store: f.st, Vault: f.v, Log: log, RevisionsKept: f.inst.StackRevisionsKept,
			GitConns: gitConns, GitSync: gitDeps.Sync, GitDeployed: gitDeps.Sync.Deployed, GitRolledBack: gitDeps.Sync.RolledBack, GitDeployFailed: gitDeps.Sync.DeployFailed,
			CheckDeadline: f.stackCheckDeadline, OnCheck: f.stackOnCheck, OnChanges: f.stackOnChanges, OnAdoptCheck: f.stackOnAdoptCheck, OnDrift: f.stackOnDrift, DriftRecheck: f.stackDriftRecheck, BeforeJob: f.stackBeforeJob, Up: f.stackUp, StopProject: f.stackStop, StartProject: f.stackStart, DownProject: f.stackDown, ReadEnv: f.stackReadEnv,
		}
		// The deployer of a background service shares the locks and the environment lookup of the routes.
		if f.stackLocks == nil {
			f.stackLocks = stackops.NewLocks()
		}
		stackDeps.Locks, stackDeps.Envs = f.stackLocks, f.envSvc
		f.stackDeps = stackDeps
		// A git stack with auto-deploy on deploys through the same deployer a background service would use.
		gitDeps.Sync.SetDeployer(api.NewStackDeployer(stackDeps).GitDeployer())
		api.RegisterStacks(r, stackDeps)
		var logsSrc api.LogsSource = f.agg
		if f.logsWrap != nil {
			logsSrc = f.logsWrap(logsSrc)
		}
		api.RegisterLogs(r, api.LogsDeps{
			Aggregator: logsSrc, Audit: f.aud, Checker: checker, Heartbeat: f.logsHeartbeat, MaxStreamsPerUser: f.logsMaxStreams,
			MaxLifetime: f.logsMaxLifetime, MaxPageBytes: f.logsMaxPageBytes, Now: f.clk.now, Log: log,
		})
		if f.statsHub != nil {
			api.RegisterStats(r, api.StatsDeps{
				Aggregator: f.agg, Clusters: f.clusters, Hub: f.statsHub, Heartbeat: f.statsHeartbeat,
				MaxStreamsPerUser: f.statsMaxStreams, MaxLifetime: f.statsMaxLifetime, Log: log,
			})
		}
		if f.clusters != nil {
			api.RegisterServiceLogs(r, api.ServiceLogsDeps{
				Clusters: f.clusters, Audit: f.aud, Checker: checker, Heartbeat: f.serviceLogsHeartbeat,
				TaskRefresh: f.serviceLogsRefresh, MaxStreamsPerUser: f.serviceLogsMaxStreams, MaxLifetime: f.serviceLogsMaxLifetime,
				MaxPageBytes: f.serviceLogsMaxPageBytes, MaxDownloadBytes: f.serviceLogsMaxDownloadBytes, MaxServerDownloads: f.serviceLogsMaxDownloads,
				Now: f.clk.now, BurstEnded: f.serviceLogsBurstEnded, Log: log,
			})
		}
		if f.shellsReg == nil {
			f.shellsReg = shells.NewRegistry(nil)
		}
		api.RegisterShells(r, api.ShellsDeps{
			Aggregator: f.agg, Registry: f.shellsReg, Audit: f.aud, Checker: checker, Instance: f.inst, SelfContainerID: f.envSvc.SelfContainerID(),
			PendingTTL: f.shellsPending, Controls: f.shellsControls, Store: f.st, Now: f.clk.now, Log: log,
		})
	}
	if f.envSvc != nil && f.agg != nil && f.hub != nil {
		api.RegisterEvents(r, api.EventsDeps{Aggregator: f.agg, Heartbeat: f.eventsHeartbeat, MaxStreamsPerUser: f.eventsMaxStreams, MaxLifetime: f.eventsMaxLifetime, Log: log})
	}
	if f.envSvc != nil && f.brk != nil {
		api.RegisterEnvStream(r, api.EnvStreamDeps{
			Broker: f.brk, Checker: checker, Heartbeat: f.envStreamHeartbeat,
			MaxStreamsPerUser: f.envStreamMaxStreams, MaxLifetime: f.envStreamMaxLifetime, Log: log,
		})
	}
	api.RegisterRegistries(r, api.RegistriesDeps{Store: registry.NewStore(f.st, f.v, f.clk.now), Now: f.clk.now, Envs: f.envSvc, Aggregator: f.agg, Audit: f.aud, Checker: checker, Egress: f.registryEgress, Version: "test", Log: log})
	api.RegisterGit(r, gitDeps)
	api.RegisterGitHooks(r, api.GitHookDeps{
		Repo: stackops.Repo{Store: f.st, Vault: f.v, Now: f.clk.now}, Sync: gitDeps.Sync, Access: f.access, Audit: f.aud, Log: log, Now: f.clk.now,
	})
	if f.envSvc != nil && f.agg != nil {
		api.RegisterGitStacks(r, f.stackDeps, gitDeps)
	}
	regStore := registry.NewStore(f.st, f.v, f.clk.now)
	api.RegisterTags(r, api.TagsDeps{
		Resolver: registry.NewResolver(regStore), Tags: registry.NewTagService(nil, f.registryEgress, "test", log),
		Checker: checker, Aggregator: f.agg, Envs: f.envSvc, Audit: f.aud, Now: f.clk.now, Log: log,
	})
	api.RegisterUnknownAgents(r, api.UnknownAgentsDeps{Unknown: f.unknown, Store: f.st, Now: f.clk.now, Log: log})
	api.RegisterRecent(r, api.RecentDeps{Store: f.st, Envs: f.envSvc, Checker: checker, Refusals: f.aud, Now: f.clk.now, Log: log})
	api.RegisterAudit(r, api.AuditDeps{Store: f.st, RetentionDays: f.auditRetention, Now: f.clk.now, Checker: checker, Refusals: f.aud, Log: log})
	api.RegisterBackup(r, api.BackupDeps{Store: f.st, Vault: f.v, Audit: f.aud, Instance: f.inst, DataDir: f.dataInfo.Path, Dir: f.backupDir, Scheduler: f.scheduler, AppVersion: "v9.9.9", Now: f.clk.now, Log: log})
	api.RegisterRestore(r, f.restoreDeps(log))
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Fatal(err)
	}
	return r.Handler()
}

func (f *authFixture) addUser(t *testing.T, name string, admin bool) string {
	t.Helper()
	ctx := context.Background()
	id := "id-" + name
	n := f.clk.now().UnixNano()
	tx, err := f.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.st.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: id, Username: name, PasswordHash: authHash(), CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	if admin {
		if err := rbac.GrantInstanceAdminTx(ctx, tx, f.st.Q, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

type authReq struct {
	method, path, body string
	cookie, csrf       string // cookie is the raw session ID
	ip                 string
	// headers are added to the request as sent.
	headers map[string]string
	// ctx is the request's context when set, so a test can end the request while it runs.
	ctx context.Context
}

func (f *authFixture) do(t *testing.T, h http.Handler, r authReq) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(r.method, r.path, body)
	if r.ctx != nil {
		req = req.WithContext(r.ctx)
	}
	req.Header.Set("User-Agent", authUA)
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = authIP + ":40000"
	if r.ip != "" {
		req.RemoteAddr = r.ip + ":40000"
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: r.cookie}) //nolint:gosec // a request cookie as a browser sends it
	}
	if r.csrf != "" {
		req.Header.Set("X-CSRF-Token", r.csrf)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	if r.method != http.MethodGet {
		if origin := f.inst.PublicOrigin(); origin != "" {
			req.Header.Set("Origin", origin)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) call(t *testing.T, r authReq) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, f.h, r)
}

func loginBody(user, password string, remember bool) string {
	b, _ := json.Marshal(map[string]any{"username": user, "password": password, "remember": remember})
	return string(b)
}

type loginResp struct {
	User struct {
		ID            string `json:"id"`
		Username      string `json:"username"`
		DisplayName   string `json:"displayName"`
		InstanceAdmin bool   `json:"instanceAdmin"`
	} `json:"user"`
	CSRFToken string `json:"csrfToken"`
}

// signIn signs admin in and returns the raw session ID and the CSRF token.
func (f *authFixture) signIn(t *testing.T, user string, remember bool) (cookie, csrf string) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(user, goodPassword, remember)})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("sign-in set no session cookie")
	}
	var r loginResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return c.Value, r.CSRFToken
}

type auditEntry struct{ Action, ActorType, ActorID, TargetID, Result, Reason, IP, UserAgent string }

func (f *authFixture) audits(t *testing.T, action string) []auditEntry {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, actor_type, actor_id, target_id, result, reason, ip, user_agent
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.Action, &e.ActorType, &e.ActorID, &e.TargetID, &e.Result, &e.Reason, &e.IP, &e.UserAgent); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *authFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- POST /api/auth/login --------------------------------------------------

func TestLoginSignsInWithABrowserSessionCookieAndReturnsTheUserAndCSRFToken(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("Admin", goodPassword, false)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got loginResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.User.ID != "id-admin" || got.User.Username != "admin" || !got.User.InstanceAdmin || got.CSRFToken == "" {
		t.Fatalf("body = %+v", got)
	}
	if strings.Contains(rec.Body.String(), `"role"`) {
		t.Fatalf("body = %s, want no role field", rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "coaming_session" || c.Value == "" {
		t.Fatalf("cookie = %+v", c)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("a browser-session cookie must have no Max-Age or Expires: %+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie flags = %+v", c)
	}
	if want := f.sess.CSRFToken(auth.HashSessionID(c.Value)); got.CSRFToken != want {
		t.Fatalf("csrfToken = %q, want the session's token %q", got.CSRFToken, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestLoginRememberMeSetsAThirtyDayCookieAndSession(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, true)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.MaxAge != 2592000 {
		t.Fatalf("cookie = %+v, want Max-Age 2592000", c)
	}
	row, err := f.st.RQ.GetSessionByHash(context.Background(), auth.HashSessionID(c.Value))
	if err != nil || row.Remember != 1 || !row.AbsoluteExpiresAt.Valid {
		t.Fatalf("session row = %+v, %v; want remembered with an absolute expiry", row, err)
	}
}

func TestLoginOverHTTPSUsesTheHostPrefixedSecureCookie(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{PublicURL: "https://coaming.example"})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "__Host-coaming_session" || !c.Secure || !c.HttpOnly || c.Domain != "" {
		t.Fatalf("cookie = %+v", c)
	}
}

func TestLoginUnknownUserWrongPasswordLockedAndDisabledAreByteIdentical(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "locked", false)
	f.addUser(t, "off", false)
	for range 5 {
		f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("locked", "wrong wrong wrong", false)})
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'off'`); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"unknown user":   loginBody("nobody", goodPassword, false),
		"wrong password": loginBody("admin", "wrong wrong wrong", false),
		"locked account": loginBody("locked", goodPassword, false), // the right password
		"disabled":       loginBody("off", goodPassword, false),
		"malformed name": loginBody("a b", goodPassword, false),
	}
	var first *httptest.ResponseRecorder
	for name, body := range cases {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: body})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401: %s", name, rec.Code, rec.Body)
		}
		if got := problemType(t, rec); got != "invalid_credentials" {
			t.Fatalf("%s: problem type = %q, want invalid_credentials", name, got)
		}
		var p struct{ Detail string }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Detail != "Invalid username or password." {
			t.Fatalf("%s: detail = %q (%v)", name, p.Detail, err)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: a failed sign-in set a cookie", name)
		}
		if first == nil {
			first = rec
			continue
		}
		if rec.Body.String() != first.Body.String() {
			t.Fatalf("%s: body %q differs from %q (D-30)", name, rec.Body, first.Body)
		}
		if rec.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
			t.Fatalf("%s: content type differs", name)
		}
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a failed sign-in created a session")
	}
}

func TestLoginWithAnExistingSessionRotatesIt(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	oldCookie, oldCSRF := f.signIn(t, "admin", false)

	rec := f.call(t, authReq{
		method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false),
		cookie: oldCookie, csrf: oldCSRF,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	fresh := sessionCookie(rec)
	if fresh == nil || fresh.Value == oldCookie {
		t.Fatalf("the session ID was not rotated: %+v", fresh)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: oldCookie}); got.Code != http.StatusUnauthorized {
		t.Fatalf("the old session after rotation: %d, want 401", got.Code)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: fresh.Value}); got.Code != http.StatusOK {
		t.Fatalf("the new session: %d, want 200", got.Code)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestLoginWhileHoldingASessionStillNeedsItsCSRFToken(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{
		method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false), cookie: cookie,
	})
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
		t.Fatalf("status = %d, type %q: %s; want 403 csrf_failed (D-36: every session mutation carries the token)", rec.Code, problemType(t, rec), rec.Body)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("a refused sign-in changed the sessions: %d", n)
	}
}

func TestLoginIsRateLimitedPerAddressWithTheSameError(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.limiter = auth.NewIPLimiter(time.Hour, 2, time.Hour, f.clk.now)
	h := f.build(t)
	post := func(ip, pw string) *httptest.ResponseRecorder {
		return f.do(t, h, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", pw, false), ip: ip})
	}
	a := post("198.51.100.4", "wrong wrong wrong")
	post("198.51.100.4", "wrong wrong wrong")
	limited := post("198.51.100.4", goodPassword) // the right password, but the address is spent
	if limited.Code != http.StatusUnauthorized || limited.Body.String() != a.Body.String() {
		t.Fatalf("rate-limited answer = %d %s, want the same 401 body as a wrong password (%s)", limited.Code, limited.Body, a.Body)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a rate-limited attempt signed in")
	}
	if got := post("198.51.100.5", goodPassword); got.Code != http.StatusOK {
		t.Fatalf("another address: %d, want 200", got.Code)
	}
}

func TestLoginAndFailedLoginAreAuditedWithIPAndUserAgent(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", "wrong wrong wrong", false)})
	f.signIn(t, "admin", false)

	failed := f.audits(t, audit.ActionAuthLoginFailed)
	if len(failed) != 1 || failed[0].Reason != "bad_password" || failed[0].IP != authIP || failed[0].UserAgent != authUA || failed[0].ActorType != "anonymous" {
		t.Fatalf("auth.login_failed = %+v", failed)
	}
	ok := f.audits(t, audit.ActionAuthLogin)
	if len(ok) != 1 || ok[0].ActorID != "id-admin" || ok[0].IP != authIP || ok[0].UserAgent != authUA || ok[0].Result != "success" {
		t.Fatalf("auth.login = %+v", ok)
	}
}

func TestLoginFiveWrongPasswordsLockTheAccountAndAuditIt(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	for range 5 {
		f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", "wrong wrong wrong", false)})
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("right password while locked: %d, want 401", rec.Code)
	}
	if got := f.audits(t, audit.ActionAuthLocked); len(got) != 1 {
		t.Fatalf("auth.locked entries = %d, want 1", len(got))
	}
	f.clk.t = f.clk.t.Add(30*time.Minute + time.Second)
	f.signIn(t, "admin", false) // the lock expired on its own
}

func TestLoginNeverEchoesOrLogsThePassword(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	const secret = "an-attempted-secret-9876"
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", secret, false)})
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(f.logs.String(), secret) {
		t.Fatal("an attempted password was echoed or logged")
	}
	good := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if strings.Contains(good.Body.String(), goodPassword) || strings.Contains(f.logs.String(), goodPassword) {
		t.Fatal("the password was echoed or logged")
	}
}

// A password typed into the username box is stored nowhere (CR-01, D-39,
// AUTH-08): not in the table, the list, either export or the container log. The
// third string is a valid-looking name, so only "no account matched" can keep it
// out.
func TestLoginWithAPasswordTypedAsTheUsernameLeavesNoTraceInTheAuditLogItsExportsOrTheLog(t *testing.T) {
	t.Parallel()
	f := newAuditFix(t)
	typed := []string{"Tr0ub4dor&3 hunter", "correct horse battery staple!", "hunter2hunter2"}
	for _, name := range typed {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(name, "wrong wrong wrong", false)})
		if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_credentials" {
			t.Fatalf("username %q: status = %d, type %q: %s; want the one 401 invalid_credentials", name, rec.Code, problemType(t, rec), rec.Body)
		}
	}

	bodies := map[string]string{}
	for label, path := range map[string]string{
		"list":        "/api/audit?action=auth.login_failed",
		"export csv":  "/api/audit/export?format=csv",
		"export json": "/api/audit/export?format=json",
	} {
		rec := f.get(t, path, f.cookie)
		if rec.code != http.StatusOK {
			t.Fatalf("GET %s: status = %d: %s", path, rec.code, rec.body)
		}
		bodies[label] = rec.body
	}
	bodies["container log"] = f.logs.String()
	for _, name := range typed {
		for label, body := range bodies {
			if strings.Contains(body, name) {
				t.Errorf("the typed string %q appears in the %s", name, label)
			}
		}
		var hits int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log
			WHERE target_id LIKE ?1 OR details LIKE ?1 OR reason LIKE ?1 OR actor_name LIKE ?1 OR actor_id LIKE ?1`,
			"%"+name+"%").Scan(&hits); err != nil {
			t.Fatal(err)
		}
		if hits != 0 {
			t.Errorf("the typed string %q is in %d audit_log rows", name, hits)
		}
	}

	failed := f.audits(t, audit.ActionAuthLoginFailed)
	if len(failed) != len(typed) {
		t.Fatalf("auth.login_failed entries = %d, want %d", len(failed), len(typed))
	}
	for _, e := range failed {
		if e.TargetID != "(unknown user)" || e.Reason != "unknown_user" {
			t.Errorf("entry = %+v, want target (unknown user) and reason unknown_user", e)
		}
	}
}

func TestLoginRejectsAnOversizedBodyFieldsWithoutTouchingTheAccount(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(strings.Repeat("x", 65), goodPassword, false)})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 65-character username: %d, want 422", rec.Code)
	}
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", strings.Repeat("x", 1025), false)})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 1025-character password: %d, want 422", rec.Code)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("an oversized sign-in created a session")
	}
}

// --- GET /api/me -----------------------------------------------------------

type meResp struct {
	User struct {
		ID            string `json:"id"`
		Username      string `json:"username"`
		DisplayName   string `json:"displayName"`
		InstanceAdmin bool   `json:"instanceAdmin"`
	} `json:"user"`
	CSRFToken   string `json:"csrfToken"`
	Permissions struct {
		Global       []string            `json:"global"`
		Environments map[string][]string `json:"environments"`
	} `json:"permissions"`
	Access map[string]struct {
		Role    string   `json:"role"`
		Extras  []string `json:"extras"`
		Sources []struct {
			Kind     string `json:"kind"`
			TeamID   string `json:"teamId"`
			TeamName string `json:"teamName"`
		} `json:"sources"`
	} `json:"access"`
	Requirements map[string]struct {
		Kind string `json:"kind"`
		Role string `json:"role"`
	} `json:"requirements"`
	Administrators []string `json:"administrators"`
}

// getMe signs name in and returns the decoded /api/me body and the raw body.
func (f *authFixture) getMe(t *testing.T, name string) (meResp, string) {
	t.Helper()
	cookie, _ := f.signIn(t, name, false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me as %s: %d %s", name, rec.Code, rec.Body)
	}
	var got meResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got, rec.Body.String()
}

// addTeam makes a team of the named users and returns its ID.
func (f *authFixture) addTeam(t *testing.T, name string, members ...string) string {
	t.Helper()
	id := "team-" + name
	if _, err := f.st.Write.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, 0, 0)`, id, name); err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if _, err := f.st.Write.Exec(`INSERT INTO team_members (team_id, user_id, created_at) VALUES (?, ?, 0)`, id, "id-"+m); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// addTeamRole gives a team a role on one environment.
func (f *authFixture) addTeamRole(t *testing.T, teamID, envID, role string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES (?, 'team', ?, ?, ?, 0)`, "ra-"+teamID+"-"+envID, teamID, role, envID); err != nil {
		t.Fatal(err)
	}
}

func TestMeReturnsTheUserCSRFTokenAndEffectivePermissions(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got meResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.User.ID != "id-admin" || got.User.Username != "admin" || !got.User.InstanceAdmin || got.CSRFToken != csrf {
		t.Fatalf("body = %+v, want the user and the same CSRF token the sign-in returned", got)
	}

	var wantGlobal, wantEnv []string
	for _, p := range rbac.All() {
		if p.Scope == rbac.ScopeGlobal {
			wantGlobal = append(wantGlobal, string(p.Name))
		} else {
			wantEnv = append(wantEnv, string(p.Name))
		}
	}
	slices.Sort(wantGlobal)
	slices.Sort(wantEnv)
	if !slices.Equal(got.Permissions.Global, wantGlobal) {
		t.Fatalf("global permissions = %v, want %v", got.Permissions.Global, wantGlobal)
	}
	if !slices.Equal(got.Permissions.Environments["*"], wantEnv) || len(got.Permissions.Environments) != 1 {
		t.Fatalf("environment permissions = %v, want only %q: %v", got.Permissions.Environments, "*", wantEnv)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestMeForAUserWithNoRoleHasEmptyPermissionListsNotNull(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "plain", false)
	cookie, _ := f.signIn(t, "plain", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"global":[]`) || !strings.Contains(rec.Body.String(), `"environments":{}`) {
		t.Fatalf("body = %s, want empty lists, not null", rec.Body)
	}
	var got meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.User.InstanceAdmin {
		t.Fatal("a user with no grant is not an instance admin")
	}
	if !strings.Contains(rec.Body.String(), `"access":{}`) || !strings.Contains(rec.Body.String(), `"administrators":["admin"]`) {
		t.Fatalf("body = %s, want empty access and the administrators", rec.Body)
	}
}

func TestMeForTheFirstAdminIsAnInstanceAdminWithOneAllEnvironmentsEntry(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	got, raw := f.getMe(t, "admin")
	if !got.User.InstanceAdmin || got.User.DisplayName != "" {
		t.Fatalf("user = %+v", got.User)
	}
	var generic struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatal(err)
	}
	if _, has := generic.User["role"]; has {
		t.Fatalf("user = %v, want no role field", generic.User)
	}
	if len(got.Access) != 1 {
		t.Fatalf("access = %+v, want only %q", got.Access, "*")
	}
	all := got.Access["*"]
	var wantExtras []string
	for _, p := range rbac.All() {
		if p.Scope == rbac.ScopeEnvironment && p.Extra {
			wantExtras = append(wantExtras, string(p.Name))
		}
	}
	slices.Sort(wantExtras)
	if all.Role != "admin" || !slices.Equal(all.Extras, wantExtras) || len(all.Sources) != 1 || all.Sources[0].Kind != "instance" {
		t.Fatalf("access[*] = %+v, want admin, extras %v and the one source instance", all, wantExtras)
	}
	if len(got.Requirements) != len(rbac.All()) {
		t.Fatalf("%d requirements for %d permissions", len(got.Requirements), len(rbac.All()))
	}
	for perm, want := range map[string]struct{ kind, role string }{
		"containers.exec":        {"extra", "operator"},
		"environments.read":      {"role", "viewer"},
		"environments.configure": {"role", "admin"},
		"users.manage":           {"instance", ""},
	} {
		if r := got.Requirements[perm]; r.Kind != want.kind || r.Role != want.role {
			t.Errorf("requirements[%s] = %+v, want %+v", perm, r, want)
		}
	}
	if !strings.Contains(raw, `"administrators":[]`) {
		t.Fatalf("body = %s, want administrators as an empty array", raw)
	}
}

func TestMeShowsATeamAndADirectGrantAsSourcesOfTheHigherRole(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "dev", false)
	f.addEnvRole(t, "dev", "env-a", "viewer")
	team := f.addTeam(t, "ops", "dev")
	f.addTeamRole(t, team, "env-a", "operator")

	got, raw := f.getMe(t, "dev")
	if got.User.InstanceAdmin {
		t.Fatal("dev is not an instance admin")
	}
	a, ok := got.Access["env-a"]
	if !ok || len(got.Access) != 1 {
		t.Fatalf("access = %+v, want only env-a", got.Access)
	}
	if a.Role != "operator" || len(a.Extras) != 0 {
		t.Fatalf("access[env-a] = %+v, want operator with no extras", a)
	}
	if len(a.Sources) != 2 || a.Sources[0].Kind != "team" || a.Sources[0].TeamName != "ops" || a.Sources[0].TeamID != team || a.Sources[1].Kind != "direct" {
		t.Fatalf("sources = %+v, want team ops then direct", a.Sources)
	}
	if !strings.Contains(raw, `"extras":[]`) || !strings.Contains(raw, `"administrators":[]`) {
		t.Fatalf("body = %s, want empty arrays, not null", raw)
	}
}

func TestMeListsTheActiveInstanceAdminsOnlyForAUserWithNoEnvironmentAccess(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addInstanceAdmin(t, "zed")
	f.addInstanceAdmin(t, "gone")
	f.addInstanceAdmin(t, "off")
	f.addUser(t, "nobody", false)
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", "env-a", "viewer")
	if _, err := f.st.Write.Exec(`UPDATE users SET deleted_at = 1 WHERE id = 'id-gone'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE id = 'id-off'`); err != nil {
		t.Fatal(err)
	}

	got, _ := f.getMe(t, "nobody")
	if want := []string{"admin", "zed"}; !slices.Equal(got.Administrators, want) {
		t.Fatalf("administrators = %v, want %v (sorted, without the disabled and the deleted)", got.Administrators, want)
	}
	if len(got.Access) != 0 {
		t.Fatalf("access = %+v, want none", got.Access)
	}
	if got, _ := f.getMe(t, "viewer"); len(got.Administrators) != 0 {
		t.Fatalf("a user with access was told the administrators: %v", got.Administrators)
	}
	if got, _ := f.getMe(t, "zed"); len(got.Administrators) != 0 {
		t.Fatalf("an instance admin was told the administrators: %v", got.Administrators)
	}
}

func TestMeWithoutASessionIs401AndAuditedAsADeniedRead(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorType != "anonymous" || denied[0].Result != "denied" || denied[0].TargetID != "GET /api/me" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
}

func TestMeWithAGarbageOrForeignCookieIs401(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	for _, v := range []string{"garbage", strings.Repeat("A", 43), strings.Repeat("A", 5000)} {
		if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: v}); rec.Code != http.StatusUnauthorized {
			t.Errorf("cookie %.12q: %d, want 401", v, rec.Code)
		}
	}
}

func TestAnHTTPSInstanceIgnoresABareSessionCookie(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{PublicURL: "https://coaming.example"})
	raw, _ := f.signIn(t, "admin", false)
	me := func(name string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: name, Value: raw}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := me("coaming_session"); got != http.StatusUnauthorized {
		t.Fatalf("a bare cookie on https: %d, want 401 (WR-01)", got)
	}
	if got := me("__Host-coaming_session"); got != http.StatusOK {
		t.Fatalf("the __Host- cookie on https: %d, want 200", got)
	}
}

func TestAPlainHTTPInstanceIgnoresAHostPrefixedCookie(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	raw, _ := f.signIn(t, "admin", false)
	me := func(name string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: name, Value: raw}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := me("__Host-coaming_session"); got != http.StatusUnauthorized {
		t.Fatalf("a __Host- cookie on plain http: %d, want 401", got)
	}
	if got := me("coaming_session"); got != http.StatusOK {
		t.Fatalf("the bare cookie on plain http: %d, want 200", got)
	}
}

func TestMeSessionSurvivesAServerRestart(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	restarted := f.build(t) // new Authenticator and Login, same database
	rec := f.do(t, restarted, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("after a restart: %d, want 200 (D-32: sessions live in SQLite)", rec.Code)
	}
	var got meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CSRFToken != csrf {
		t.Fatal("the CSRF token changed across a restart")
	}
}

func TestMeSessionEndsAfterTwelveHoursIdleButARememberedOneDoesNot(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	plain, _ := f.signIn(t, "admin", false)
	kept, _ := f.signIn(t, "admin", true)

	f.clk.t = f.clk.t.Add(authIdle + time.Second)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: plain}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser session idle 12h+1s: %d, want 401", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: kept}); rec.Code != http.StatusOK {
		t.Fatalf("remembered session idle 12h+1s: %d, want 200 (D-31)", rec.Code)
	}
	f.clk.t = f.clk.t.Add(authRemain)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: kept}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remembered session past 30 days: %d, want 401", rec.Code)
	}
}

// --- POST /api/auth/logout -------------------------------------------------

func TestLogoutEndsOnlyThisDevicesSessionClearsTheCookieAndIsAudited(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	phone, _ := f.signIn(t, "admin", false)
	laptop, laptopCSRF := f.signIn(t, "admin", true)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: laptop, csrf: laptopCSRF})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Value != "" || c.MaxAge >= 0 || !c.HttpOnly {
		t.Fatalf("the cookie was not cleared: %+v", c)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: laptop}); got.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out session: %d, want 401", got.Code)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: phone}); got.Code != http.StatusOK {
		t.Fatalf("the other device after logout: %d, want 200 (D-33)", got.Code)
	}
	out := f.audits(t, audit.ActionAuthLogout)
	if len(out) != 1 || out[0].ActorID != "id-admin" || out[0].IP != authIP || out[0].UserAgent != authUA {
		t.Fatalf("auth.logout = %+v", out)
	}
}

func TestLogoutWithoutAValidCSRFTokenIs403AndKeepsTheSession(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token", "truncated": csrf[:8]} {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: cookie, csrf: token})
		if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
			t.Fatalf("%s token: %d %q, want 403 csrf_failed", name, rec.Code, problemType(t, rec))
		}
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}); got.Code != http.StatusOK {
		t.Fatalf("the session after refused logouts: %d, want 200", got.Code)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 3 || denied[0].Reason != "csrf" || denied[0].ActorID != "id-admin" {
		t.Fatalf("csrf denials = %+v, want 3 audited with reason csrf", denied)
	}
	if len(f.audits(t, audit.ActionAuthLogout)) != 0 {
		t.Fatal("a refused logout was audited as a logout")
	}
}

func TestLogoutAnotherSessionsTokenIsRefused(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	a, _ := f.signIn(t, "admin", false)
	_, bCSRF := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: a, csrf: bCSRF})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a token from another session: %d, want 403", rec.Code)
	}
}

func TestLogoutWithoutASessionOrWithAnExpiredOneIs401(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d, want 401", rec.Code)
	}
	cookie, csrf := f.signIn(t, "admin", false)
	f.clk.t = f.clk.t.Add(authIdle + time.Minute)
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: cookie, csrf: csrf}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired session: %d, want 401", rec.Code)
	}
}

func TestMutationFromAnotherOriginIsRefusedBeforeLogin(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{PublicURL: "https://coaming.example"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(loginBody("admin", goodPassword, false)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "origin_mismatch" {
		t.Fatalf("cross-origin login: %d %q, want 403 origin_mismatch", rec.Code, problemType(t, rec))
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a cross-origin login created a session")
	}
}

// refusalSource keeps a nil coalescer a nil interface, so the warnings handler
// sees "none" rather than a typed nil.
func refusalSource(c *auth.RefusalCoalescer) interface{ Bursts(time.Time) []auth.Burst } {
	if c == nil {
		return nil
	}
	return c
}
