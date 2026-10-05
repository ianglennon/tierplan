// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/engine"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Errors of the registry. The API maps each to a stable problem type.
var (
	// ErrSocketNotFound means there is no Docker socket at the configured path.
	ErrSocketNotFound = errors.New("envs: no Docker socket was found")
	// ErrEngineUnreachable means a socket exists but nothing Docker answers on
	// it, usually a permission problem.
	ErrEngineUnreachable = errors.New("envs: the Docker engine did not answer")
	// ErrUnsupportedEngine means the engine is one Bayplan does not manage and
	// the caller did not ask to add it anyway.
	ErrUnsupportedEngine = errors.New("envs: the Docker engine is not supported")
	// ErrLocalExists means the local host is already an environment.
	ErrLocalExists = errors.New("envs: the local host is already an environment")
	// ErrInvalidName means the name is empty or longer than 64 characters.
	ErrInvalidName = errors.New("envs: the name must be 1 to 64 characters")
	// ErrNotFound means there is no environment with that ID.
	ErrNotFound = errors.New("envs: no such environment")
)

const (
	// KindLocal is the Docker socket on the server's own host.
	KindLocal = "local"
	// KindAgent is an environment reached through one or more agents that dial
	// in (D-18). Its monitors are keyed by agent, not by environment.
	KindAgent = "agent"

	maxNameLen = 64
	// fallbackName names an environment whose engine reported no hostname,
	// which only an unsupported engine can do.
	fallbackName = "Local Docker"
	// pendingName is shown for an environment that has no name yet.
	pendingName = "New environment"

	targetEnvironment = "environment"

	// statusRevoked is the status of an agent an admin revoked, and the roles an
	// agent row carries.
	statusRevoked     = "revoked"
	roleManager       = "manager"
	roleWorker        = "worker"
	envTypeStandalone = "standalone"
)

// AgentTarget is the monitor key of one agent. An agent environment has no
// monitor of its own: each connected agent is a target, and the environment's
// status is read from its targets (RESEARCH Pattern 5).
func AgentTarget(agentID string) string { return "agent:" + agentID }

// SocketTarget is the monitor key of the Docker socket of a host folded into a swarm
// environment (D-48). It is keyed by the swarm environment, which owns it from the fold.
func SocketTarget(envID string) string { return "socket:" + envID }

// Environment is a managed environment with its live status.
type Environment struct {
	ID, Kind, Name, SocketPath string
	Enabled                    bool
	CreatedAt                  time.Time
	// Status is the monitor's view of the environment's one engine: the local
	// socket, or for an agent environment its primary target. It carries the probe
	// data (engine, version, type) and is unknown while no target is monitored.
	Status Status
	// Health is the environment's rolled-up state (D-33) with its D-40 reason and
	// issues. It is what the API reports as the environment's status.
	Health Health
	// Overview is what the environments table shows beside the health (D-32).
	Overview Overview
	// PendingClusterID is the cluster a manager reported after the swarm was
	// re-initialised, while that change waits for an admin to adopt it (D-14).
	// Empty when no change is recorded.
	PendingClusterID string
	// AutoUpdate is whether Bayplan updates the environment's agents itself. It is
	// meaningful for an agent environment only.
	AutoUpdate bool
	// FoldOffered is true on the local environment and on the swarm environment when
	// the local host manages the swarm and could be folded into it (D-48).
	FoldOffered bool
}

// DefaultReconnectGrace is how long after the server starts an environment with
// no agent present shows as reconnecting (D-41).
const DefaultReconnectGrace = 60 * time.Second

// HealthConfig is the timing the health rollup uses. A zero field takes its
// default: the moment the service was built, the 30 s offline window and the 60 s
// reconnect grace.
type HealthConfig struct {
	ServerStart  time.Time
	OfflineAfter time.Duration
	Grace        time.Duration
	// ServerVersion is what an agent's version is measured against to say an update
	// is available; empty or not a release version never says so.
	ServerVersion string
}

// AgentSource is what the registry reads of the live agents: the tunnel hub.
type AgentSource interface {
	// Views is what is known of the agents of an environment that have connected.
	Views(envID string) []tunnel.AgentLive
	// Forget drops what is known of the agents of a removed environment.
	Forget(envID string)
	// KickEnv closes every live session of the environment's agents, recording code
	// as why.
	KickEnv(envID, code string)
}

// AgentUninstaller tells the agents of an environment to remove themselves from their
// hosts (D-30). agents.Updater is one.
type AgentUninstaller interface {
	// UninstallAll asks every online agent of the environment to uninstall, waits up to
	// timeout for their answers, audits each confirmation under actor and returns the
	// IDs of the agents that confirmed.
	UninstallAll(ctx context.Context, actor audit.Actor, envID, ip, ua string, timeout time.Duration) []string
}

// UninstallWait is how long removing an environment waits for its agents to confirm
// that they are uninstalling (D-30).
const UninstallWait = 30 * time.Second

// Service is the environment registry: it stores the environments an admin
// opted in, audits every change, starts and stops their monitors, and resolves
// {envId} for the policy.
type Service struct {
	st         *store.Store
	aud        *audit.Writer
	mon        *Monitor
	dockerHost string
	selfID     string
	now        func() time.Time
	log        *slog.Logger

	// agents, health and clusters are set before the service is used (WithAgents,
	// WithHealth, WithClusters).
	agents      AgentSource
	health      HealthConfig
	clusters    *Clusters
	uninstaller AgentUninstaller
	// uninstallWait is how long Remove waits for the agents; UninstallWait unless a
	// test changed it.
	uninstallWait time.Duration

	mu     sync.Mutex
	runCtx context.Context
	// connected holds the live agent sessions by environment and agent ID, the
	// map the status of an agent environment is read through.
	connected map[string]map[string]*tunnel.Session
}

// NewService returns the registry. dockerHost is the raw DOCKER_HOST setting
// (empty means the default socket). selfID is Bayplan's own container ID from
// ReadSelfContainerID, empty when it does not run in a container.
func NewService(s *store.Store, w *audit.Writer, m *Monitor, dockerHost string, selfID string, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		st: s, aud: w, mon: m, dockerHost: dockerHost, selfID: selfID, now: now,
		log: m.log, runCtx: context.Background(),
		connected: map[string]map[string]*tunnel.Session{},
		health: HealthConfig{
			ServerStart: now(), OfflineAfter: tunnel.DefaultOfflineAfter, Grace: DefaultReconnectGrace,
		},
	}
}

// WithAgents tells the service where to read the live agents from. Call it before
// the service is used.
func (s *Service) WithAgents(src AgentSource) *Service {
	s.agents = src
	return s
}

// WithUninstaller tells the service who removes the agents from their hosts when an
// admin removes an environment and asks for that. Without it Remove only revokes. Call
// it before the service is used.
func (s *Service) WithUninstaller(u AgentUninstaller) *Service {
	s.uninstaller = u
	return s
}

// WithUninstallWait changes how long Remove waits for agents to confirm an uninstall,
// which is UninstallWait unless this is called. A value of zero or less keeps the
// default. It exists for tests, which cannot wait 30 seconds.
func (s *Service) WithUninstallWait(d time.Duration) *Service {
	if d > 0 {
		s.uninstallWait = d
	}
	return s
}

// WithHealth sets the timing of the health rollup; a zero field keeps the value it
// has. Call it before the service is used.
func (s *Service) WithHealth(c HealthConfig) *Service {
	if !c.ServerStart.IsZero() {
		s.health.ServerStart = c.ServerStart
	}
	if c.OfflineAfter > 0 {
		s.health.OfflineAfter = c.OfflineAfter
	}
	if c.Grace > 0 {
		s.health.Grace = c.Grace
	}
	if c.ServerVersion != "" {
		s.health.ServerVersion = c.ServerVersion
	}
	return s
}

// WithClusters tells the service where the cluster view of swarm environments
// comes from. Without it the rollup decides from the agents alone. Call it before
// the service is used.
func (s *Service) WithClusters(c *Clusters) *Service {
	s.clusters = c
	return s
}

// primary is the target a one-engine read of the environment goes to: for a swarm
// the manager a cluster read would choose, so the dashboard follows the leader.
func (s *Service) primary(in RollupInput) (string, bool) {
	if s.clusters != nil {
		return s.clusters.Manager(in)
	}
	return Primary(in)
}

// Input is everything Rollup decides an environment's health from, read now: the
// environment row, its agent rows, the hub's live view of each agent and the
// monitor's status of each target. It returns ErrNotFound for an unknown
// environment.
func (s *Service) Input(ctx context.Context, envID string, now time.Time) (RollupInput, error) {
	row, err := s.st.RQ.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return RollupInput{}, ErrNotFound
	}
	if err != nil {
		return RollupInput{}, fmt.Errorf("get environment: %w", err)
	}
	return s.input(ctx, row, now)
}

func (s *Service) input(ctx context.Context, row sqlc.Environment, now time.Time) (RollupInput, error) {
	in := RollupInput{
		EnvID: row.ID, Kind: row.Kind, Now: now,
		ServerStart: s.health.ServerStart, OfflineAfter: s.health.OfflineAfter, Grace: s.health.Grace,
	}
	if row.Kind != KindAgent {
		in.Local = s.mon.Status(row.ID)
		if s.clusters != nil {
			in.Cluster = s.clusters.View(in)
		}
		return in, nil
	}
	rows, err := s.st.RQ.ListAgentsByEnv(ctx, row.ID)
	if err != nil {
		return RollupInput{}, fmt.Errorf("list agents: %w", err)
	}
	live := map[string]tunnel.AgentLive{}
	if s.agents != nil {
		for _, v := range s.agents.Views(row.ID) {
			live[v.AgentID] = v
		}
	}
	swarm := false
	folded := ""
	if row.LocalNodeID.Valid {
		folded = row.LocalNodeID.String
	}
	for _, a := range rows {
		if folded != "" && a.SwarmNodeID.String == folded {
			// The host is folded in: its node is reached through the socket, whose target
			// wins, so an agent that runs on it is not a second way to the same node.
			continue
		}
		v := AgentView{
			ID: a.ID, Node: a.Name, Role: a.Role, SwarmNodeID: a.SwarmNodeID.String, Revoked: a.Status == statusRevoked,
			CertNotAfter: time.Unix(0, a.CertNotAfter).UTC(),
			// Presence never reads the stored last-seen: after a restart it would make
			// an agent that has not reconnected look present for up to a minute.
			EngineReachable: true, Version: a.AgentVersion,
		}
		if a.RevokedAt.Valid {
			v.RevokedAt = time.Unix(0, a.RevokedAt.Int64).UTC()
		}
		if a.LastSeenAt.Valid {
			v.StoredSeen = time.Unix(0, a.LastSeenAt.Int64).UTC()
		}
		if l, ok := live[a.ID]; ok {
			v.Connected, v.ConnectedAt, v.LastSeen, v.Ended, v.LastCode = l.Connected, l.ConnectedAt, l.LastSeen, l.Ended, l.LastCode
			v.EngineReachable, v.ClockOffset = l.EngineReachable, l.ClockOffset
			if l.Version != "" {
				v.Version = l.Version
			}
			if l.Connected {
				v.Target = s.mon.Status(AgentTarget(a.ID))
			}
		}
		if !v.Revoked {
			in.Expected++
			swarm = swarm || a.Role == roleManager || a.Role == roleWorker
		}
		in.Agents = append(in.Agents, v)
	}
	if folded != "" {
		in.Agents = append(in.Agents, s.socketView(row, now))
		in.Expected++
		swarm = true
	}
	switch {
	case row.EnvType.Valid:
		in.EnvType = row.EnvType.String
	case swarm:
		in.EnvType = envTypeSwarm
	case in.Expected > 0:
		in.EnvType = envTypeStandalone
	}
	if row.ClusterChangedAt.Valid {
		in.ClusterChanged, in.ClusterChangedAt = true, time.Unix(0, row.ClusterChangedAt.Int64).UTC()
	}
	if s.clusters != nil {
		in.Cluster = s.clusters.View(in)
		if v := in.Cluster; v != nil && v.Known && in.EnvType == envTypeSwarm {
			// The cluster knows which nodes should run an agent, which is better than
			// counting the agents that enrolled.
			in.Expected = v.ExpectedAgents
		}
	}
	return in, nil
}

// socketView is the folded host as a node of the swarm environment: always connected,
// with no certificate and no clock to report, and the monitor's status of its socket as
// its target. It is a manager until the socket's own probe says it is not.
func (s *Service) socketView(row sqlc.Environment, now time.Time) AgentView {
	key := SocketTarget(row.ID)
	st := s.mon.Status(key)
	role := roleManager
	if t := st.Probe.Type; t == dockerx.TypeSwarmWorker || t == dockerx.TypeStandalone {
		role = roleWorker
	}
	node := cleanText(st.Probe.Hostname, maxNodeLen)
	if node == "" {
		node = "local"
	}
	return AgentView{
		ID: key, Node: node, Role: role, SwarmNodeID: row.LocalNodeID.String, Socket: true,
		Connected: true, ConnectedAt: firstSet(st.Since, now), LastSeen: now, EngineReachable: true, Target: st,
	}
}

// Known reports whether an environment with this ID exists. A lookup that fails
// counts as not known.
func (s *Service) Known(ctx context.Context, id string) bool {
	_, err := s.st.RQ.GetEnvironment(ctx, id)
	return err == nil
}

// SelfContainerID is the ID of Bayplan's own container, or empty (D-27).
func (s *Service) SelfContainerID() string { return s.selfID }

func (s *Service) ctx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runCtx
}

// StartAll starts the monitor of every stored environment and uses ctx as the
// life of every monitor started from now on, including those AddLocal starts.
// With no environments it does nothing and never contacts Docker (D-19).
func (s *Service) StartAll(ctx context.Context) error {
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return fmt.Errorf("list environments: %w", err)
	}
	for _, row := range rows {
		// An agent environment has no socket here: its agents connect after a
		// start and become monitor targets then (Connected).
		if row.Enabled == 0 {
			continue
		}
		switch {
		case row.Kind == KindLocal:
			if err := s.mon.Start(ctx, row.ID, engine.LocalSocketTransport{Path: row.SocketPath}); err != nil {
				return err
			}
		case row.Kind == KindAgent && row.LocalNodeID.Valid && row.LocalSocketPath.String != "":
			// A folded host has no agent: its socket is a target of the swarm environment.
			if err := s.mon.Start(ctx, SocketTarget(row.ID), engine.LocalSocketTransport{Path: row.LocalSocketPath.String}); err != nil {
				return err
			}
		}
	}
	return nil
}

// List returns every environment, oldest first, with its live status.
func (s *Service) List(ctx context.Context) ([]Environment, error) {
	rows, err := s.st.RQ.ListEnvironments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	out := make([]Environment, 0, len(rows))
	for _, row := range rows {
		e, err := s.environment(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// Get returns one environment, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (Environment, error) {
	row, err := s.st.RQ.GetEnvironment(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, fmt.Errorf("get environment: %w", err)
	}
	return s.environment(ctx, row)
}

// environment builds the API view of a row: its rolled-up health (D-33) and the
// status of the one target its engine data is read through. An agent environment
// is never monitored under its own ID: its health is the rollup of its agents,
// and its engine data are those of its primary target.
func (s *Service) environment(ctx context.Context, row sqlc.Environment) (Environment, error) {
	name := row.Name
	if name == "" {
		// An agent environment the admin did not name stays blank in the
		// database until its first agent enrols and names it (D-23).
		name = pendingName
	}
	now := s.now()
	in, err := s.input(ctx, row, now)
	if err != nil {
		return Environment{}, err
	}
	status := Status{State: StateUnknown}
	if key, ok := s.primary(in); ok {
		status = s.mon.Status(key)
	}
	e := Environment{
		ID: row.ID, Kind: row.Kind, Name: name, SocketPath: row.SocketPath,
		Enabled:    row.Enabled != 0,
		CreatedAt:  time.Unix(0, row.CreatedAt).UTC(),
		Status:     status,
		Health:     Rollup(in),
		Overview:   overview(in, status, s.health.ServerVersion),
		AutoUpdate: row.AutoUpdate != 0,
	}
	e.FoldOffered = s.foldOfferedFor(ctx, row)
	if row.ClusterChangedAt.Valid {
		e.PendingClusterID = row.PendingClusterID.String
	}
	return e, nil
}

// Connected implements tunnel.Observer: a session that is ready becomes a
// monitor target, probed through its own tunnel exactly as the local socket is.
func (s *Service) Connected(sess *tunnel.Session) {
	s.mu.Lock()
	byAgent := s.connected[sess.EnvID]
	if byAgent == nil {
		byAgent = map[string]*tunnel.Session{}
		s.connected[sess.EnvID] = byAgent
	}
	byAgent[sess.AgentID] = sess
	ctx := s.runCtx
	s.mu.Unlock()

	key := AgentTarget(sess.AgentID)
	// A session that replaces an older one of the same agent must not keep the
	// old transport: Start does nothing for a key that is already monitored.
	s.mon.Stop(key)
	if err := s.mon.Start(ctx, key, sess.Transport()); err != nil {
		s.log.Error("monitor an agent", "agent", sess.AgentID, "environment", sess.EnvID, "error", err)
	}
}

// Disconnected implements tunnel.Observer.
func (s *Service) Disconnected(sess *tunnel.Session, code string) {
	s.mu.Lock()
	if byAgent := s.connected[sess.EnvID]; byAgent != nil && byAgent[sess.AgentID] == sess {
		delete(byAgent, sess.AgentID)
		if len(byAgent) == 0 {
			delete(s.connected, sess.EnvID)
		}
	} else {
		// A newer session of the same agent has already taken its place, and
		// its monitor must keep running.
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.mon.Stop(AgentTarget(sess.AgentID))
	s.log.Info("agent target stopped", "agent", sess.AgentID, "environment", sess.EnvID, "reason", code)
}

var _ tunnel.Observer = (*Service)(nil)

// HasLocal reports whether the local host is already managed, for the "managed" flag of
// GET /api/local-host: it is its own environment, or it was folded into a swarm
// environment (D-48), which reaches it through its socket.
func (s *Service) HasLocal(ctx context.Context) (bool, error) {
	if has, err := s.hasLocalEnvironment(ctx); err != nil || has {
		return has, err
	}
	n, err := s.st.RQ.CountFoldedEnvironments(ctx)
	if err != nil {
		return false, fmt.Errorf("count folded environments: %w", err)
	}
	return n > 0, nil
}

// hasLocalEnvironment reports whether the local host is an environment of its own.
// Adding it again after a fold is allowed, and undoes the fold's effect on the list.
func (s *Service) hasLocalEnvironment(ctx context.Context) (bool, error) {
	n, err := s.st.RQ.CountEnvironmentsOfKind(ctx, KindLocal)
	if err != nil {
		return false, fmt.Errorf("count local environments: %w", err)
	}
	return n > 0, nil
}

// Resolve implements httpx.EnvResolver: an unknown ID is ErrEnvNotFound and a
// disabled environment is ErrEnvDisabled (D-48).
func (s *Service) Resolve(ctx context.Context, envID string) (httpx.Env, error) {
	row, err := s.st.RQ.GetEnvironment(ctx, envID)
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Env{}, httpx.ErrEnvNotFound
	}
	if err != nil {
		return httpx.Env{}, fmt.Errorf("resolve environment: %w", err)
	}
	env := httpx.Env{ID: row.ID, Name: row.Name, Enabled: row.Enabled != 0}
	if !env.Enabled {
		return env, httpx.ErrEnvDisabled
	}
	return env, nil
}

// AddLocal opts the local Docker host in as an environment (D-19). It needs a
// socket that answers. An unsupported engine is refused unless allowUnsupported
// is true, in which case it is stored and shown as unsupported (D-25). The name
// defaults to the hostname the daemon reports (D-23). Two concurrent calls
// create one environment: the other gets ErrLocalExists.
func (s *Service) AddLocal(ctx context.Context, actor audit.Actor, name string, allowUnsupported bool, ip, ua string) (Environment, error) {
	name = strings.TrimSpace(name)
	if name != "" && !validName(name) {
		return Environment{}, ErrInvalidName
	}
	if exists, err := s.hasLocalEnvironment(ctx); err != nil {
		return Environment{}, err
	} else if exists {
		return Environment{}, ErrLocalExists
	}

	path, err := engine.ParseDockerHost(s.dockerHost)
	if err != nil {
		return Environment{}, err
	}
	if !engine.SocketExists(path) {
		return Environment{}, fmt.Errorf("%w at %s", ErrSocketNotFound, path)
	}
	transport := engine.LocalSocketTransport{Path: path}
	res, err := s.probe(ctx, transport)
	if err != nil {
		return Environment{}, fmt.Errorf("%w at %s: %s", ErrEngineUnreachable, path, detail(res, err))
	}
	if !res.Supported && !allowUnsupported {
		return Environment{}, fmt.Errorf("%w: %s (%s %s, API %s)", ErrUnsupportedEngine, res.Reason, res.EngineName, res.EngineVersion, res.APIVersion)
	}
	if name == "" {
		name = defaultName(res.Hostname)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Environment{}, fmt.Errorf("generate environment id: %w", err)
	}
	now := s.now().UTC().UnixNano()

	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Environment{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	details := map[string]string{"name": name, "kind": KindLocal, "socket": path}
	if allowUnsupported && !res.Supported {
		details["unsupported"] = res.Reason
	}
	if err := s.st.Q.WithTx(tx).InsertEnvironment(ctx, sqlc.InsertEnvironmentParams{
		ID: id.String(), Kind: KindLocal, Name: name, SocketPath: path, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Environment{}, ErrLocalExists
		}
		return Environment{}, fmt.Errorf("insert environment: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentAdded, TargetType: targetEnvironment, TargetID: id.String(),
		EnvID: id.String(), Result: audit.ResultSuccess, IP: ip, UserAgent: ua, Details: details,
	}); err != nil {
		return Environment{}, fmt.Errorf("audit environment added: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Environment{}, fmt.Errorf("commit: %w", err)
	}

	if err := s.mon.Start(s.ctx(), id.String(), transport); err != nil {
		return Environment{}, err
	}
	st, _ := statusFromProbe(res, nil, s.now())
	return Environment{
		ID: id.String(), Kind: KindLocal, Name: name, SocketPath: path, Enabled: true,
		CreatedAt: time.Unix(0, now).UTC(), Status: st, Health: Health{State: st.State, Reason: st.Reason, Since: st.Since},
		Overview: overview(RollupInput{Kind: KindLocal}, st, s.health.ServerVersion),
	}, nil
}

// probe looks at the engine once with a throw-away client, so adding does not
// depend on a monitor that does not exist yet. An error means the engine did
// not answer; an unsupported engine is an answer.
func (s *Service) probe(ctx context.Context, t engine.Transport) (dockerx.ProbeResult, error) {
	cli, err := dockerx.New(t)
	if err != nil {
		return dockerx.ProbeResult{}, err
	}
	defer func() { _ = cli.Close() }()
	pctx, cancel := context.WithTimeout(ctx, s.mon.probeTimeout)
	defer cancel()
	return cli.Probe(pctx)
}

func detail(res dockerx.ProbeResult, err error) string {
	if res.Detail != "" {
		return res.Detail
	}
	return err.Error()
}

// Rename changes an environment's name and audits the old and new one.
func (s *Service) Rename(ctx context.Context, actor audit.Actor, id, name, ip, ua string) (Environment, error) {
	name = strings.TrimSpace(name)
	if !validName(name) {
		return Environment{}, ErrInvalidName
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Environment{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := q.GetEnvironment(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, fmt.Errorf("get environment: %w", err)
	}
	now := s.now().UTC().UnixNano()
	if _, err := q.RenameEnvironment(ctx, sqlc.RenameEnvironmentParams{Name: name, UpdatedAt: now, ID: id}); err != nil {
		return Environment{}, fmt.Errorf("rename environment: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentRenamed, TargetType: targetEnvironment, TargetID: id,
		EnvID: id, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"from": row.Name, "to": name},
	}); err != nil {
		return Environment{}, fmt.Errorf("audit environment renamed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Environment{}, fmt.Errorf("commit: %w", err)
	}
	row.Name, row.UpdatedAt = name, now
	return s.environment(ctx, row)
}

// removedReason is the reason recorded for each agent that removing its environment
// revokes.
const removedReason = "environment_removed"

// Remove deletes the environment and stops its monitor. It changes nothing in
// Docker (D-20): the host's containers, networks and volumes are untouched.
//
// An agent environment's agents are revoked with it (D-08, D-30): each agent that
// was still active is audited as revoked, the join token is disabled, and once the
// deletion has committed every live session is closed. The environment's CA, tokens,
// agents and blocks go with the row through the foreign keys, so a later hello from
// any of them finds no environment and is refused auth_rejected.
//
// With uninstall set, an agent environment's online agents are first told to remove
// themselves from their hosts and given UninstallWait to confirm (D-30). Whatever they
// answer, or do not, the revoking and the deletion then happen as usual: an agent that
// was offline, refused or never answered is left for the admin's manual cleanup command.
func (s *Service) Remove(ctx context.Context, actor audit.Actor, id, ip, ua string, uninstall bool) error {
	if uninstall && s.uninstaller != nil {
		row, err := s.st.RQ.GetEnvironment(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get environment: %w", err)
		}
		if row.Kind == KindAgent {
			wait := s.uninstallWait
			if wait <= 0 {
				wait = UninstallWait
			}
			s.uninstaller.UninstallAll(ctx, actor, id, ip, ua, wait)
		}
	}
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err := q.GetEnvironment(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get environment: %w", err)
	}
	if row.Kind == KindAgent {
		if err := s.revokeAgentsOf(ctx, tx, q, actor, id, ip, ua); err != nil {
			return err
		}
	}
	if _, err := q.DeleteEnvironment(ctx, id); err != nil {
		return fmt.Errorf("delete environment: %w", err)
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionEnvironmentRemoved, TargetType: targetEnvironment, TargetID: id,
		EnvID: id, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"name": row.Name, "kind": row.Kind},
	}); err != nil {
		return fmt.Errorf("audit environment removed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.mon.Stop(id)
	s.mon.Stop(SocketTarget(id))
	// The rows are gone, so a session that is still open would be talking for an
	// environment that does not exist.
	if s.agents != nil && row.Kind == KindAgent {
		s.agents.KickEnv(id, tunnel.DisconnectRemoved)
	}
	// An agent environment is monitored through its agents' targets.
	s.mu.Lock()
	agents := s.connected[id]
	delete(s.connected, id)
	s.mu.Unlock()
	for agentID := range agents {
		s.mon.Stop(AgentTarget(agentID))
	}
	if s.agents != nil {
		s.agents.Forget(id)
	}
	if s.clusters != nil {
		s.clusters.Forget(id)
	}
	return nil
}

// revokeAgentsOf disables an environment's join token and audits agent.revoked for
// each of its agents that is still active, in the transaction that deletes the
// environment. The rows themselves are deleted by the cascade, so only the audit
// log remembers who was revoked and why.
func (s *Service) revokeAgentsOf(ctx context.Context, tx *sql.Tx, q *sqlc.Queries, actor audit.Actor, envID, ip, ua string) error {
	if _, err := q.DisableCurrentToken(ctx, sqlc.DisableCurrentTokenParams{
		DisabledAt: sql.NullInt64{Int64: s.now().UTC().UnixNano(), Valid: true}, EnvID: envID,
	}); err != nil {
		return fmt.Errorf("disable the join token: %w", err)
	}
	rows, err := q.ListAgentsByEnv(ctx, envID)
	if err != nil {
		return fmt.Errorf("list the agents: %w", err)
	}
	for _, a := range rows {
		if a.Status != "active" {
			continue
		}
		if err := s.aud.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionAgentRevoked, TargetType: "agent", TargetID: a.ID,
			EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
			Details: map[string]string{"node": a.Name, "fingerprint": a.CertFingerprint, "reason": removedReason},
		}); err != nil {
			return fmt.Errorf("audit agent revoked: %w", err)
		}
	}
	return nil
}

// Reconnect asks the monitor for an immediate probe and returns the status as
// it is now; the caller polls for the result.
func (s *Service) Reconnect(ctx context.Context, id string) (Environment, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return Environment{}, err
	}
	in, err := s.Input(ctx, id, s.now())
	if err != nil {
		return Environment{}, err
	}
	for _, key := range Targets(in) {
		s.mon.Kick(key)
	}
	return e, nil
}

func validName(name string) bool {
	n := utf8.RuneCountInString(name)
	return n >= 1 && n <= maxNameLen
}

// defaultName is the hostname the daemon reported, cut to the name limit, or a
// fixed name when it reported none (D-23).
func defaultName(hostname string) string {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fallbackName
	}
	if utf8.RuneCountInString(hostname) > maxNameLen {
		hostname = string([]rune(hostname)[:maxNameLen])
	}
	return hostname
}

var _ httpx.EnvResolver = (*Service)(nil)

// ErrAgentPolicyDenied means an agent refused a Docker request by its own policy
// (D-29). It is a refusal, not an outage: the engine is fine and the agent is
// doing what the host owner set.
var ErrAgentPolicyDenied = errors.New("envs: the agent refused the request by policy")

// AgentPolicyDeniedError is an ErrAgentPolicyDenied that names the request.
type AgentPolicyDeniedError struct {
	// Endpoint is "METHOD /path" as the agent reported it, capped and free of
	// control characters. It is text for a person to read, nothing more.
	Endpoint string
	cause    error
}

func (e *AgentPolicyDeniedError) Error() string {
	return "envs: the agent refused " + e.Endpoint + " by policy"
}

// Is makes errors.Is(err, ErrAgentPolicyDenied) true.
func (e *AgentPolicyDeniedError) Is(target error) bool { return target == ErrAgentPolicyDenied }

// Unwrap returns the error the refusal was read from.
func (e *AgentPolicyDeniedError) Unwrap() error { return e.cause }

// policyDeniedMarker starts the message of every refusal an agent gives. The
// Docker client puts the daemon's message in its error, so the marker is how a
// refusal is told from an engine error.
const policyDeniedMarker = "agent_policy_denied:"

// maxEndpoint caps the endpoint kept from a refusal message.
const maxEndpoint = 256

// ClassifyDockerError turns an error from a Docker call into an
// *AgentPolicyDeniedError when its message is an agent's refusal, and returns
// every other error, and nil, unchanged.
func ClassifyDockerError(err error) error {
	if err == nil {
		return nil
	}
	_, rest, ok := strings.Cut(err.Error(), policyDeniedMarker)
	if !ok {
		return err
	}
	endpoint, _, _ := strings.Cut(rest, "\n")
	endpoint = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(endpoint))
	if len(endpoint) > maxEndpoint {
		endpoint = strings.ToValidUTF8(endpoint[:maxEndpoint], "")
	}
	return &AgentPolicyDeniedError{Endpoint: endpoint, cause: err}
}
