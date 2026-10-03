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
)

// AgentTarget is the monitor key of one agent. An agent environment has no
// monitor of its own: each connected agent is a target, and the environment's
// status is read from its targets (RESEARCH Pattern 5).
func AgentTarget(agentID string) string { return "agent:" + agentID }

// Environment is a managed environment with its live status.
type Environment struct {
	ID, Kind, Name, SocketPath string
	Enabled                    bool
	CreatedAt                  time.Time
	Status                     Status
}

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
	}
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
		if row.Enabled == 0 || row.Kind != KindLocal {
			continue
		}
		if err := s.mon.Start(ctx, row.ID, engine.LocalSocketTransport{Path: row.SocketPath}); err != nil {
			return err
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
		out = append(out, s.environment(row))
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
	return s.environment(row), nil
}

func (s *Service) environment(row sqlc.Environment) Environment {
	name := row.Name
	if name == "" {
		// An agent environment the admin did not name stays blank in the
		// database until its first agent enrols and names it (D-23).
		name = pendingName
	}
	return Environment{
		ID: row.ID, Kind: row.Kind, Name: name, SocketPath: row.SocketPath,
		Enabled:   row.Enabled != 0,
		CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
		Status:    s.status(row),
	}
}

// status is the monitor's view of a local environment, and for an agent
// environment the view of its connected agent. With no agent connected it is
// unknown; plan 11 replaces this with the D-33 rollup over every agent.
func (s *Service) status(row sqlc.Environment) Status {
	if row.Kind != KindAgent {
		return s.mon.Status(row.ID)
	}
	if id := s.firstConnectedAgent(row.ID); id != "" {
		return s.mon.Status(AgentTarget(id))
	}
	return Status{State: StateUnknown}
}

// firstConnectedAgent is the connected agent of envID with the lowest ID, or
// empty when none is connected.
func (s *Service) firstConnectedAgent(envID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := ""
	for id := range s.connected[envID] {
		if first == "" || id < first {
			first = id
		}
	}
	return first
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

// HasLocal reports whether the local host is already an environment, for the
// "managed" flag of GET /api/local-host.
func (s *Service) HasLocal(ctx context.Context) (bool, error) {
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
	if exists, err := s.HasLocal(ctx); err != nil {
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
		CreatedAt: time.Unix(0, now).UTC(), Status: st,
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
	return s.environment(row), nil
}

// Remove deletes the environment and stops its monitor. It changes nothing in
// Docker (D-20): the host's containers, networks and volumes are untouched.
func (s *Service) Remove(ctx context.Context, actor audit.Actor, id, ip, ua string) error {
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
	// An agent environment is monitored through its agents' targets.
	s.mu.Lock()
	agents := s.connected[id]
	delete(s.connected, id)
	s.mu.Unlock()
	for agentID := range agents {
		s.mon.Stop(AgentTarget(agentID))
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
	s.mon.Kick(id)
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
