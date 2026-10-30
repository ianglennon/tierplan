// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/engine"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
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
	// Coaming's own container is. Nil means neither is known.
	Envs LocalHostEnvs
}

// LocalHostEnvs is what GET /api/local-host needs from the environment
// registry. *envs.Service implements it.
type LocalHostEnvs interface {
	HasLocal(ctx context.Context) (bool, error)
	SelfContainerID() string
}

// LocalHostBody says whether this server's own Docker socket exists, and what
// engine is behind it.
type LocalHostBody struct {
	SocketPath string `json:"socketPath" doc:"The Docker socket path Coaming was configured with"`
	Detected   bool   `json:"detected" doc:"Whether a unix socket exists at that path"`
	// Managed is true once an admin has added this host as an environment.
	Managed bool `json:"managed" doc:"Whether this host is already an environment (D-19)"`
	// OwnContainerID is Coaming's own container, so the UI can badge it "Coaming (this instance)".
	OwnContainerID string `json:"ownContainerId,omitempty" doc:"The ID of the container Coaming runs in, when it can tell (D-27)"`
	// Probe is only present when a socket was detected.
	Probe *dockerx.ProbeResult `json:"probe,omitempty" doc:"What the engine behind the socket reported; absent when no socket exists"`
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
			"worker or standalone. Read-only. Needs environments.create because it is the context for adding the host.",
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
			return out, nil
		})
}
