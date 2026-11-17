// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/engine"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// defaultProbeTimeout bounds one look at the local engine, so a daemon that
// accepts the connection and then hangs cannot hang the setup wizard (D-26).
const defaultProbeTimeout = 3 * time.Second

// LocalHostDeps are the dependencies of GET /api/local-host.
type LocalHostDeps struct {
	// DockerHost is the raw DOCKER_HOST setting. Empty means the default
	// socket. server.Build rejects anything but unix:// before it gets here.
	DockerHost string
	// ProbeTimeout bounds the probe. Zero means 3 seconds.
	ProbeTimeout time.Duration
	// Log receives probe failures. Nil means slog.Default().
	Log *slog.Logger
	// Envs says whether the local host is already an environment and what
	// TierPlan's own container is. Nil means neither is known.
	Envs LocalHostEnvs
}

// LocalHostEnvs is what GET /api/local-host needs from the environment
// registry. *envs.Service implements it.
type LocalHostEnvs interface {
	HasLocal(ctx context.Context) (bool, error)
	SelfContainerID() string
	// LocalSwarm names the swarm environment the host behind p is a node of, and whether the
	// fold into it is offered. It makes no Docker call: p is the probe the handler already made.
	LocalSwarm(ctx context.Context, p dockerx.ProbeResult) (envs.LocalSwarm, bool)
}

// LocalHostSwarmBody is the swarm environment this host's own swarm node belongs to (D-01, D-05).
type LocalHostSwarmBody struct {
	EnvironmentID   string `json:"environmentId" doc:"The swarm environment, added through its agents, that this host is a node of"`
	EnvironmentName string `json:"environmentName" doc:"That environment's name"`
	FoldOffered     bool   `json:"foldOffered" doc:"Whether the fold card of that environment offers to fold this host into it now. False means its fold card is not ready: the swarm's node list does not hold this host yet, or the host is not in a state to be folded"`
}

// LocalHostBody says whether this server's own Docker socket exists, and what
// engine is behind it.
type LocalHostBody struct {
	SocketPath string `json:"socketPath" doc:"The Docker socket path TierPlan was configured with"`
	Detected   bool   `json:"detected" doc:"Whether a unix socket exists at that path"`
	// Managed is true once an admin has added this host as an environment.
	Managed bool `json:"managed" doc:"Whether this host is already an environment (D-19)"`
	// OwnContainerID is TierPlan's own container, so the UI can badge it "TierPlan (this instance)".
	OwnContainerID string `json:"ownContainerId,omitempty" doc:"The ID of the container TierPlan runs in, when it can tell (D-27)"`
	// Probe is only present when a socket was detected.
	Probe *dockerx.ProbeResult `json:"probe,omitempty" doc:"What the engine behind the socket reported; absent when no socket exists"`
	// Swarm is only present for a host that is a swarm node and is not managed yet.
	Swarm *LocalHostSwarmBody `json:"swarm,omitempty" doc:"The swarm environment this host's node belongs to; present only when the host is a node of a swarm, is not managed yet, and an unfolded swarm environment of that swarm exists. Absent otherwise, and then the setup panel offers to add the swarm"`
}

// LocalHostOutput wraps LocalHostBody for huma.
type LocalHostOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         LocalHostBody
}

// RegisterLocalHost registers GET /api/local-host. It is the context for the
// admin's opt-in to manage this host (D-19): it only reads. Adding the host as
// an environment is POST /api/environments.
func RegisterLocalHost(r *httpx.Router, d LocalHostDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.ProbeTimeout <= 0 {
		d.ProbeTimeout = defaultProbeTimeout
	}
	path, parseErr := engine.ParseDockerHost(d.DockerHost)

	httpx.Register(r, huma.Operation{
		OperationID: "getLocalHost",
		Method:      http.MethodGet,
		Path:        "/api/local-host",
		Summary:     "Detect the Docker engine on this server's own host",
		Description: "Reports whether a Docker socket exists at the configured path and, if so, what engine answers on it: " +
			"supported or not (and why), its version and API version, and whether the host is a Swarm manager, a Swarm " +
			"worker or standalone. The probe carries the manager hosts a swarm node reports (swarmManagerHosts, host parts only), and for a node " +
			"of a swarm that is already added through its agents, swarm names that environment and says whether its fold card offers the fold " +
			"(worked out from this one probe; no second Docker call). Read-only. Needs environments.create because it is the context for adding the host.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsCreate, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, _ *struct{}) (*LocalHostOutput, error) {
			if parseErr != nil {
				// Startup validates DOCKER_HOST, so this is unreachable in a
				// running server; answer plainly rather than panic.
				d.Log.Error("local host check refused", "error", parseErr)
				return nil, huma.Error500InternalServerError("DOCKER_HOST is not a unix socket path")
			}
			out := &LocalHostOutput{CacheControl: "no-store", Body: LocalHostBody{SocketPath: path}}
			if d.Envs != nil {
				managed, err := d.Envs.HasLocal(ctx)
				if err != nil {
					d.Log.Error("look up the local environment", "error", err)
					return nil, huma.Error500InternalServerError("internal error")
				}
				out.Body.Managed = managed
				out.Body.OwnContainerID = d.Envs.SelfContainerID()
			}
			if !engine.SocketExists(path) {
				return out, nil
			}
			out.Body.Detected = true

			cli, err := dockerx.New(engine.LocalSocketTransport{Path: path})
			if err != nil {
				d.Log.Error("create the local docker client", "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			defer func() { _ = cli.Close() }()

			pctx, cancel := context.WithTimeout(ctx, d.ProbeTimeout)
			defer cancel()
			// An unreachable engine is an answer, not a failure: the result
			// says reachable=false and why, and the wizard shows it.
			res, err := cli.Probe(pctx)
			if err != nil {
				d.Log.Debug("local docker engine probe", "socket", path, "error", err)
			}
			out.Body.Probe = &res
			if d.Envs != nil && !out.Body.Managed && (res.Type == dockerx.TypeSwarmManager || res.Type == dockerx.TypeSwarmWorker) {
				if ls, ok := d.Envs.LocalSwarm(ctx, res); ok {
					out.Body.Swarm = &LocalHostSwarmBody{EnvironmentID: ls.EnvID, EnvironmentName: ls.EnvName, FoldOffered: ls.FoldOffered}
				}
			}
			return out, nil
		})
}
