// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package selfmaint is the agent's own maintenance: updating itself to a new
// image of its own repository and removing itself. It is the one
// agent package that may hold a Docker client, and it is not the passthrough proxy:
// the proxy forwards a read-only allowlist for the server, while this package does
// a handful of writes the agent decides on itself, each scoped to the agent (D-26).
//
//   - It may pull only its own image repository.
//   - It may update only its own service (swarm).
//   - It may create, start and remove only a helper container from its own image,
//     and replace its own container (standalone).
//   - It may remove its own service and the join-token secret that service names, and
//     have a helper remove its own container and state volume (uninstall).
//
// "Its own" is the label tierplan.agent=true on the agent's own container, found by
// the container's ID, plus the swarm service ID that container carries. Nothing is
// ever selected by a name or an ID the server sent. The host owner can switch all of
// it off with TIERPLAN_AGENT_SELF_UPDATE=false, which is checked first and before any
// Docker call (D-28).
package selfmaint

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/proto/wire"
)

const (
	// AgentLabel marks every container and service of the agent, including the
	// helper. Self-maintenance touches nothing without it.
	AgentLabel = "tierplan.agent"
	// HelperLabel marks the short-lived helper container of a standalone update.
	HelperLabel = "tierplan.agent.helper"
	// swarmServiceLabel is set by swarm on every task container.
	swarmServiceLabel = "com.docker.swarm.service.id"

	// updateTimeout bounds one update request, a pull included.
	updateTimeout = 10 * time.Minute
)

// Service is what the control stream hands the server's update and uninstall
// messages to. Each call answers with the update_result to send back.
type Service interface {
	Update(ctx context.Context, msg wire.Update) wire.UpdateResult
	Uninstall(ctx context.Context, msg wire.Uninstall) wire.UpdateResult
}

// Config is what New needs.
type Config struct {
	// SocketPath is the Docker socket. Empty means /var/run/docker.sock.
	SocketPath string
	// SelfID is the ID of the agent's own container, or its hostname, which Docker
	// sets to the short ID. Empty means ResolveSelfID().
	SelfID string
	// SelfUpdate is TIERPLAN_AGENT_SELF_UPDATE. False refuses update and uninstall.
	SelfUpdate bool
	// OwnRepository is the agent's image repository, for example
	// ghcr.io/ianglennon/tierplan-agent. Empty means it is read from the agent's own
	// container the first time an update arrives.
	OwnRepository string
	// StateDir is the agent's state directory, which the helper mounts.
	// Empty means /var/lib/tierplan-agent.
	StateDir string
	// Log receives one line per event. Nil means slog.Default().
	Log *slog.Logger
}

// Maint implements Service.
type Maint struct {
	cfg     Config
	log     *slog.Logger
	running atomic.Bool
	// beforeDocker, when set, runs once the update has taken the single slot.
	// Tests use it to hold an update in flight.
	beforeDocker func()
}

var _ Service = (*Maint)(nil)

// New builds a Maint. It makes no Docker call.
func New(cfg Config) *Maint {
	if cfg.SocketPath == "" {
		cfg.SocketPath = "/var/run/docker.sock"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/tierplan-agent"
	}
	if cfg.SelfID == "" {
		cfg.SelfID = ResolveSelfID()
	}
	if cfg.OwnRepository != "" {
		if n, err := reference.ParseNormalizedNamed(cfg.OwnRepository); err == nil {
			cfg.OwnRepository = n.Name()
		} else {
			cfg.OwnRepository = ""
		}
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Maint{cfg: cfg, log: log}
}

// busy reports whether an update is running.
func (m *Maint) busy() bool { return m.running.Load() }

// result starts an answer to a request with the given id.
func result(id string) wire.UpdateResult {
	return wire.UpdateResult{T: wire.TypeUpdateResult, ID: wire.CapID(id)}
}

func refuse(res wire.UpdateResult, code string) wire.UpdateResult {
	res.OK, res.Error = false, code
	return res
}

// Update replaces the agent with msg.Image. The answer says whether the update
// was accepted and started; it is not a report that the new version runs.
func (m *Maint) Update(ctx context.Context, msg wire.Update) wire.UpdateResult {
	res := result(msg.ID)
	if !m.cfg.SelfUpdate {
		m.log.Warn("the server asked this agent to update, and TIERPLAN_AGENT_SELF_UPDATE=false refuses it")
		return refuse(res, wire.UpdateErrSelfUpdateDisabled)
	}
	ref, ok := parseImage(msg.Image)
	if !ok {
		m.log.Warn("the server asked this agent to update to something that is not a full image reference")
		return refuse(res, wire.UpdateErrImageInvalid)
	}
	if m.cfg.OwnRepository != "" && ref.Name() != m.cfg.OwnRepository {
		m.log.Warn("the server asked this agent to update to another repository; refused", "repository", ref.Name())
		return refuse(res, wire.UpdateErrImageNotOwnRepository)
	}
	if !m.running.CompareAndSwap(false, true) {
		return refuse(res, wire.UpdateErrBusy)
	}
	defer m.running.Store(false)
	if m.beforeDocker != nil {
		m.beforeDocker()
	}

	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	cli, err := m.newClient()
	if err != nil {
		m.log.Error("self-update: cannot build the Docker client", "error", err)
		return refuse(res, wire.UpdateErrFailed)
	}
	defer func() { _ = cli.Close() }()

	self, code := m.inspectSelf(ctx, cli)
	if code != "" {
		return refuse(res, code)
	}
	if m.cfg.OwnRepository == "" {
		own, err := reference.ParseNormalizedNamed(self.Config.Image)
		if err != nil {
			m.log.Error("self-update: the agent's own image is not a repository reference", "image", self.Config.Image)
			return refuse(res, wire.UpdateErrOwnContainerUnknown)
		}
		if ref.Name() != own.Name() {
			m.log.Warn("the server asked this agent to update to another repository; refused", "repository", ref.Name())
			return refuse(res, wire.UpdateErrImageNotOwnRepository)
		}
	}

	m.log.Info("self-update requested", "image", msg.Image)
	if svc := self.Config.Labels[swarmServiceLabel]; svc != "" {
		if code := m.updateSwarm(ctx, cli, svc, msg.Image); code != "" {
			return refuse(res, code)
		}
	} else {
		if code := m.updateStandalone(ctx, cli, self, msg.Image); code != "" {
			return refuse(res, code)
		}
	}
	res.OK = true
	return res
}

// parseImage accepts a full reference: a repository with a tag, a digest or both.
// A bare repository would mean :latest, which an update must never silently do.
func parseImage(s string) (reference.Named, bool) {
	if s == "" || len(s) > wire.MaxImageLen {
		return nil, false
	}
	ref, err := reference.ParseNormalizedNamed(s)
	if err != nil || reference.IsNameOnly(ref) {
		return nil, false
	}
	return ref, true
}

// newClient builds a Docker client on the socket. The client negotiates the API
// version with the engine on first use (the default since client v0.5); no request
// is made here.
func (m *Maint) newClient() (*client.Client, error) {
	return client.New(client.WithHost("unix://" + m.cfg.SocketPath))
}

var hexID = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// inspectSelf reads the agent's own container and checks it is labelled as an
// agent. It returns an update_result error code when it is not, or when it cannot
// be found.
func (m *Maint) inspectSelf(ctx context.Context, cli *client.Client) (container.InspectResponse, string) {
	if m.cfg.SelfID == "" {
		m.log.Error("self-update: the agent does not know its own container ID")
		return container.InspectResponse{}, wire.UpdateErrOwnContainerUnknown
	}
	r, err := cli.ContainerInspect(ctx, m.cfg.SelfID, client.ContainerInspectOptions{})
	if err != nil {
		m.log.Error("self-update: cannot inspect the agent's own container", "id", m.cfg.SelfID, "error", err)
		return container.InspectResponse{}, wire.UpdateErrOwnContainerUnknown
	}
	c := r.Container
	if c.Config == nil || c.Config.Labels[AgentLabel] != "true" {
		m.log.Error("self-update: the container with the agent's ID is not labelled as an agent", "id", m.cfg.SelfID)
		return container.InspectResponse{}, wire.UpdateErrOwnContainerUnknown
	}
	// A hostname that is not an ID may name some other container; an ID must match.
	if hexID.MatchString(m.cfg.SelfID) && !strings.HasPrefix(c.ID, m.cfg.SelfID) {
		m.log.Error("self-update: the inspected container is not the one with the agent's ID", "id", m.cfg.SelfID)
		return container.InspectResponse{}, wire.UpdateErrOwnContainerUnknown
	}
	return c, ""
}

// ResolveSelfID finds the ID of the container the agent runs in: from the mounts
// Docker sets up under /var/lib/docker/containers/<id>/, else the hostname, which
// Docker sets to the short ID unless the operator chose one.
func ResolveSelfID() string {
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		defer func() { _ = f.Close() }()
		if id := containerIDFromMountinfo(f); id != "" {
			return id
		}
	}
	h, _ := os.Hostname()
	return h
}

var containerMount = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// containerIDFromMountinfo returns the first 64-character container ID found in
// the mount sources of a /proc/self/mountinfo, or "".
func containerIDFromMountinfo(r io.Reader) string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		if m := containerMount.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

// unixHTTPClient is a plain HTTP client on the Docker socket, for the calls that
// must keep the engine's JSON byte for byte and so cannot go through the typed client.
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		}},
	}
}

var errNoImage = errors.New("the service spec has no container image to replace")
