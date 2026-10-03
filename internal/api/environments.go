// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Stable problem types of the environment routes. The SPA maps each to copy.
const (
	problemLocalEnvironmentExists = "local_environment_exists"
	problemSocketNotFound         = "socket_not_found"
	problemEngineUnreachable      = "engine_unreachable"
	problemUnsupportedEngine      = "unsupported_engine"
	problemInvalidName            = "invalid_name"
	problemEnvironmentNotFound    = "environment_not_found"
	problemPublicURLMissing       = "public_url_missing"
	problemAgentsUnavailable      = "agents_unavailable"
)

// EnvironmentsDeps are the dependencies of the environment routes.
type EnvironmentsDeps struct {
	Envs *envs.Service
	// Agents creates agent environments and their join tokens. Nil means the
	// server cannot add one.
	Agents *agents.Service
	// PublicURL returns Bayplan's public URL, from which the address agents dial
	// is built. Nil or empty means no agent environment can be added.
	PublicURL func() string
	// Checker filters the list to the environments the caller may read.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EnvironmentStatusBody is what the health monitor currently believes.
type EnvironmentStatusBody struct {
	State       string `json:"state" enum:"unknown,online,offline,unsupported" doc:"online: the engine answers and is supported. offline: it does not answer. unsupported: it answers but Bayplan does not manage it. unknown: not probed yet"`
	Since       string `json:"since,omitempty" format:"date-time" doc:"When the environment entered this state; for offline, the first failed probe"`
	Reason      string `json:"reason,omitempty" doc:"Why it is offline or unsupported, as a stable code such as unreachable, api_too_old, podman, rootless or docker_desktop"`
	LastAttempt string `json:"lastAttempt,omitempty" format:"date-time" doc:"When the most recent probe finished"`
}

// EnvironmentEngineBody identifies the engine behind an environment.
type EnvironmentEngineBody struct {
	Name          string `json:"name,omitempty" doc:"For example Docker Engine"`
	Version       string `json:"version,omitempty" doc:"The engine version"`
	APIVersion    string `json:"apiVersion,omitempty" doc:"The newest Engine API version the daemon speaks"`
	NegotiatedAPI string `json:"negotiatedApi,omitempty" doc:"The API version Bayplan uses with it"`
}

// EnvironmentBody is one managed environment.
type EnvironmentBody struct {
	ID         string                `json:"id" doc:"The environment ID, used in /api/env/{envId}/..."`
	Name       string                `json:"name" doc:"The display name; defaults to the daemon's hostname"`
	Kind       string                `json:"kind" enum:"local,agent" doc:"local: the Docker socket on the host Bayplan runs on. agent: one or more hosts that dial in through an agent"`
	Type       string                `json:"type,omitempty" enum:"swarm-manager,swarm-worker,standalone" doc:"What the engine is; absent until it has answered"`
	IsSelfHost bool                  `json:"isSelfHost" doc:"True for the Docker host Bayplan itself runs on"`
	Enabled    bool                  `json:"enabled"`
	CreatedAt  string                `json:"createdAt" format:"date-time"`
	Status     EnvironmentStatusBody `json:"status"`
	Engine     EnvironmentEngineBody `json:"engine"`
}

// EnvironmentInstallBody is what an admin needs to connect a host to a new agent
// environment. It is in the answer to adding one and nowhere else: the join token
// is stored only as a hash and cannot be shown again.
type EnvironmentInstallBody struct {
	JoinToken string `json:"joinToken" doc:"The join token the agent presents once to enrol. Shown only in this answer"`
	AgentURL  string `json:"agentUrl" doc:"The address the agent dials, for example wss://bayplan.example.com/agent/connect"`
}

// AddEnvironmentBody is the answer to adding an environment: the environment, and
// for an agent environment the install details.
type AddEnvironmentBody struct {
	EnvironmentBody
	Install *EnvironmentInstallBody `json:"install,omitempty" doc:"Present for an agent environment only: the join token and the address the agent dials"`
}

// AddEnvironmentOutput wraps AddEnvironmentBody for huma.
type AddEnvironmentOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AddEnvironmentBody
}

// EnvironmentListBody lists the environments the caller may see.
type EnvironmentListBody struct {
	Items []EnvironmentBody `json:"items"`
}

// EnvironmentListOutput wraps EnvironmentListBody for huma.
type EnvironmentListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvironmentListBody
}

// EnvironmentOutput wraps one EnvironmentBody for huma.
type EnvironmentOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvironmentBody
}

// AddEnvironmentInput opts a Docker host in as an environment.
type AddEnvironmentInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Kind             string `json:"kind" enum:"local,agent" doc:"local: the Docker socket on this server's own host. agent: a new environment whose hosts run the Bayplan agent and dial in; the answer carries the join token"`
		Name             string `json:"name,omitempty" maxLength:"1024" doc:"1 to 64 characters; defaults to the hostname the daemon reports, or for an agent environment the hostname of its first agent"`
		AllowUnsupported bool   `json:"allowUnsupported,omitempty" doc:"Local only: add the host even though its engine is not supported; it is stored and shown as unsupported"`
	}
}

// EnvironmentPathInput carries {envId} for the routes that act on one environment.
type EnvironmentPathInput struct {
	EnvID string `path:"envId" doc:"The environment ID"`
}

// RenameEnvironmentInput renames an environment.
type RenameEnvironmentInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Name string `json:"name" maxLength:"1024" doc:"The new name, 1 to 64 characters"`
	}
}

// RemoveEnvironmentInput removes an environment.
type RemoveEnvironmentInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func environmentBody(e envs.Environment) EnvironmentBody {
	p := e.Status.Probe
	return EnvironmentBody{
		ID: e.ID, Name: e.Name, Kind: e.Kind,
		Type:       p.Type,
		IsSelfHost: e.Kind == envs.KindLocal,
		Enabled:    e.Enabled,
		CreatedAt:  rfc3339(e.CreatedAt),
		Status: EnvironmentStatusBody{
			State:       string(e.Status.State),
			Since:       rfc3339(e.Status.Since),
			Reason:      e.Status.Reason,
			LastAttempt: rfc3339(e.Status.LastAttempt),
		},
		Engine: EnvironmentEngineBody{
			Name: p.EngineName, Version: p.EngineVersion, APIVersion: p.APIVersion, NegotiatedAPI: p.NegotiatedAPI,
		},
	}
}

// canRead reports whether the grants listed for each environment include
// environments.read for id, directly or through an assignment that covers every
// environment (the "*" key).
func canRead(perEnv map[string][]rbac.Perm, id string) bool {
	return slices.Contains(perEnv[id], rbac.PermEnvironmentsRead) ||
		slices.Contains(perEnv[rbac.AllEnvironments], rbac.PermEnvironmentsRead)
}

// RegisterEnvironments registers the environment routes: list, add, rename,
// remove, and the retry-now action under /api/env/{envId}. Adding the local
// host is the admin's opt-in to managing a root-equivalent socket (D-19), so it
// needs environments.write and nothing is added automatically.
func RegisterEnvironments(r *httpx.Router, d EnvironmentsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	fail := func(what string, err error) error {
		switch {
		case errors.Is(err, envs.ErrLocalExists):
			return httpx.Problem(http.StatusConflict, problemLocalEnvironmentExists, "The local Docker host is already an environment.")
		case errors.Is(err, envs.ErrSocketNotFound):
			return httpx.Problem(http.StatusConflict, problemSocketNotFound, "No Docker socket was found on this host.")
		case errors.Is(err, envs.ErrEngineUnreachable):
			return httpx.Problem(http.StatusConflict, problemEngineUnreachable, "A Docker socket exists but the engine did not answer. Check that Bayplan may open it.")
		case errors.Is(err, envs.ErrUnsupportedEngine):
			return httpx.Problem(http.StatusConflict, problemUnsupportedEngine, "Bayplan does not support the Docker engine on this host. Add it anyway to see it as unsupported.")
		case errors.Is(err, envs.ErrInvalidName), errors.Is(err, agents.ErrInvalidName):
			return httpx.Problem(http.StatusUnprocessableEntity, problemInvalidName, "The name must be 1 to 64 characters.")
		case errors.Is(err, envs.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		d.Log.Error("environment request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	one := func(e envs.Environment) *EnvironmentOutput {
		return &EnvironmentOutput{CacheControl: "no-store", Body: environmentBody(e)}
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listEnvironments",
		Method:      http.MethodGet,
		Path:        "/api/environments",
		Summary:     "List the environments the signed-in user may see",
		Description: "Each environment comes with the health monitor's status. An environment the caller does not hold " +
			"environments.read for is left out, so the list never reveals what the caller cannot open.",
		Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*EnvironmentListOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			return nil, fail("effective permissions", err)
		}
		all, err := d.Envs.List(ctx)
		if err != nil {
			return nil, fail("list environments", err)
		}
		items := make([]EnvironmentBody, 0, len(all))
		for _, e := range all {
			if canRead(perEnv, e.ID) {
				items = append(items, environmentBody(e))
			}
		}
		return &EnvironmentListOutput{CacheControl: "no-store", Body: EnvironmentListBody{Items: items}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "addEnvironment",
		Method:        http.MethodPost,
		Path:          "/api/environments",
		Summary:       "Add an environment",
		DefaultStatus: http.StatusCreated,
		Description: "kind local is the admin's explicit opt-in to managing the Docker host Bayplan runs on (D-19): nothing is " +
			"added at startup. Needs a Docker socket that answers. 409 socket_not_found, engine_unreachable, unsupported_engine " +
			"(unless allowUnsupported) or local_environment_exists. kind agent creates an environment for hosts that run the " +
			"Bayplan agent, with its own certificate authority and a join token, and answers install with the token (shown once) " +
			"and the address the agent dials; 409 public_url_missing when Bayplan has no public URL. 422 invalid_name. " +
			"The name defaults to the hostname the daemon, or the first agent, reports. Audited as environment.added.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *AddEnvironmentInput) (*AddEnvironmentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if in.Body.Kind == agents.KindAgent {
			return addAgentEnvironment(ctx, d, fail, p, in)
		}
		e, err := d.Envs.AddLocal(ctx, p.AuditActor(), in.Body.Name, in.Body.AllowUnsupported, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("add environment", err)
		}
		return &AddEnvironmentOutput{CacheControl: "no-store", Body: AddEnvironmentBody{EnvironmentBody: environmentBody(e)}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "renameEnvironment",
		Method:      http.MethodPatch,
		Path:        "/api/environments/{envId}",
		Summary:     "Rename an environment",
		Description: "422 invalid_name unless the name is 1 to 64 characters. Audited as environment.renamed with the old and new name.",
		Tags:        []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RenameEnvironmentInput) (*EnvironmentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		e, err := d.Envs.Rename(ctx, p.AuditActor(), in.EnvID, in.Body.Name, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("rename environment", err)
		}
		return one(e), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "removeEnvironment",
		Method:        http.MethodDelete,
		Path:          "/api/environments/{envId}",
		Summary:       "Remove an environment",
		DefaultStatus: http.StatusNoContent,
		Description: "Stops managing the host. Changes nothing in Docker: its containers, networks and volumes are untouched " +
			"(D-20). Audited as environment.removed.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsWrite, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RemoveEnvironmentInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := d.Envs.Remove(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail("remove environment", err)
		}
		return nil, nil
	})

	// Reconnect is deliberately not audited: it changes no Bayplan or Docker
	// state, it only asks the monitor to probe now. The environment is resolved
	// by the policy before this runs, so an unknown or disabled one never gets here.
	httpx.Register(r, huma.Operation{
		OperationID:   "reconnectEnvironment",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/reconnect",
		Summary:       "Retry the connection to an environment now",
		DefaultStatus: http.StatusAccepted,
		Description: "Asks the health monitor to probe the engine immediately instead of waiting for its next retry, and " +
			"returns the status as it is now; poll the environment for the result. The probe renegotiates the API version.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment}, func(ctx context.Context, in *EnvironmentPathInput) (*EnvironmentOutput, error) {
		e, err := d.Envs.Reconnect(ctx, in.EnvID)
		if err != nil {
			return nil, fail("reconnect environment", err)
		}
		return one(e), nil
	})
}

// addAgentEnvironment creates an agent environment and answers with its join
// token. The token is in this answer only: the server keeps its hash.
func addAgentEnvironment(ctx context.Context, d EnvironmentsDeps, fail func(string, error) error, p *rbac.Principal, in *AddEnvironmentInput) (*AddEnvironmentOutput, error) {
	if d.Agents == nil {
		return nil, httpx.Problem(http.StatusConflict, problemAgentsUnavailable, "This server cannot add agent environments.")
	}
	publicURL := ""
	if d.PublicURL != nil {
		publicURL = d.PublicURL()
	}
	agentURL, err := agents.AgentURL(publicURL)
	if publicURL == "" || err != nil {
		return nil, httpx.Problem(http.StatusConflict, problemPublicURLMissing,
			"Bayplan has no public URL, so it cannot tell an agent where to connect. Set one in Settings, or with BAYPLAN_PUBLIC_URL.")
	}
	created, err := d.Agents.CreateEnvironment(ctx, p.AuditActor(), in.Body.Name, httpx.ClientIP(ctx), in.UserAgent)
	if err != nil {
		return nil, fail("add agent environment", err)
	}
	e, err := d.Envs.Get(ctx, created.EnvID)
	if err != nil {
		return nil, fail("read the new environment", err)
	}
	return &AddEnvironmentOutput{CacheControl: "no-store", Body: AddEnvironmentBody{
		EnvironmentBody: environmentBody(e),
		Install:         &EnvironmentInstallBody{JoinToken: created.JoinToken, AgentURL: agentURL},
	}}, nil
}
