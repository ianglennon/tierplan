// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// AgentsDeps are the dependencies of the agent routes.
type AgentsDeps struct {
	// Agents rotates and disables join tokens. Nil means the routes answer 409
	// agents_unavailable.
	Agents *agents.Service
	// Instance supplies the address agents dial, as for adding an environment.
	Instance *instance.Instance
	// TLSPin returns the SPKI pin of the self-signed certificate Bayplan serves, or
	// "" when it serves none. Nil means none.
	TLSPin func() string
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RotateTokenBody is the answer to rotating a join token.
type RotateTokenBody struct {
	Install   InstallBody `json:"install" doc:"The new token and every command and file built from it. Shown only in this answer: the server keeps the token's hash and cannot show it again. install.generation is one more than the token it replaced, and the swarm secret is named for it"`
	EnvType   string      `json:"envType,omitempty" enum:"swarm,standalone" doc:"What the environment's active agents run on. Absent while it has none"`
	HasAgents bool        `json:"hasAgents" doc:"Whether the environment has an active agent. A swarm that has agents is moved to the new token with install.swarm.rotateCommand, which is present only then; otherwise a first install needs the new token's command"`
}

// RotateTokenOutput wraps RotateTokenBody for huma.
type RotateTokenOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RotateTokenBody
}

// TokenStatusBody is what may be shown of an environment's current join token.
type TokenStatusBody struct {
	Generation int    `json:"generation" doc:"Which token this is for the environment, from 1"`
	CreatedAt  string `json:"createdAt" format:"date-time" doc:"When the token was made"`
	CreatedBy  string `json:"createdBy" doc:"The username of the admin who made it, or the principal as recorded when that is not an account"`
	Disabled   bool   `json:"disabled" doc:"True while the token enrols nobody, until the next rotation"`
}

// TokenStatusOutput wraps TokenStatusBody for huma.
type TokenStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TokenStatusBody
}

// TokenMutationInput carries {envId} and the client's User-Agent for the routes
// that change a join token.
type TokenMutationInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RegisterAgents registers the join token routes: rotate, disable and inspect.
// All three need environments.token, a dangerous global permission, because a
// token is what lets a new host in (D-02).
func RegisterAgents(r *httpx.Router, d AgentsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	fail := func(what string, err error) error {
		if errors.Is(err, agents.ErrEnvironmentNotFound) {
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		d.Log.Error("join token request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	usable := func() error {
		if d.Agents == nil {
			return httpx.Problem(http.StatusConflict, problemAgentsUnavailable, "This server cannot manage agent environments.")
		}
		return nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "rotateEnvironmentToken",
		Method:      http.MethodPost,
		Path:        "/api/environments/{envId}/token",
		Summary:     "Rotate an environment's join token",
		Description: "Makes a new join token and retires the current one in the same transaction. The old install command stops " +
			"enrolling hosts at once; agents that are already enrolled keep their certificates and are not disconnected. Answers the " +
			"install object for the new token, once, with Cache-Control no-store; when the environment is a swarm that already has agents " +
			"it carries the one-line rotate command. 404 environment_not_found; 409 " + problemPublicURLRequired + " when Bayplan has no " +
			"address to give agents. Needs environments.token. Audited as environment.token_rotated with the new generation.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsToken, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *TokenMutationInput) (*RotateTokenOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := usable(); err != nil {
			return nil, err
		}
		// The address is checked before anything changes: a rotation whose commands
		// cannot be built would retire a token nobody has seen.
		endpoint := ""
		if d.Instance != nil {
			endpoint = d.Instance.AgentEndpoint()
		}
		if endpoint == "" {
			return nil, httpx.Problem(http.StatusConflict, problemPublicURLRequired,
				"Bayplan has no public URL or agent URL, so it cannot tell an agent where to connect. Set one in Settings, or with BAYPLAN_PUBLIC_URL or BAYPLAN_AGENT_URL.")
		}
		rot, err := d.Agents.RotateToken(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("rotate token", err)
		}
		pin := ""
		if d.TLSPin != nil {
			pin = d.TLSPin()
		}
		install, err := agents.BuildInstall(agents.InstallParams{
			Token: rot.JoinToken, Endpoint: endpoint, Pin: pin, ServerVersion: buildinfo.Get().Version, Generation: rot.Generation,
		})
		if err != nil {
			// The token is retired and cannot be shown again. A value that breaks the
			// quoting is a server bug, so it is logged and not hidden.
			return nil, fail("build the install", err)
		}
		body := installBody(install)
		// The rotate command swaps a running service over to the new secret. Without a
		// swarm that has agents there is no service to swap, so it is left out.
		if rot.EnvType != agents.EnvTypeSwarm || !rot.HasAgents {
			body.Swarm.RotateCommand = ""
		}
		return &RotateTokenOutput{CacheControl: "no-store", Body: RotateTokenBody{
			Install: *body, EnvType: rot.EnvType, HasAgents: rot.HasAgents,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "disableEnvironmentToken",
		Method:        http.MethodDelete,
		Path:          "/api/environments/{envId}/token",
		Summary:       "Disable an environment's join token",
		DefaultStatus: http.StatusNoContent,
		Description: "The current join token enrols nobody from now on. Agents that are already enrolled keep working. Rotating the token " +
			"makes a new one and enables enrolment again. Disabling a disabled token changes nothing. 404 environment_not_found. " +
			"Needs environments.token. Audited as environment.token_disabled.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsToken, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *TokenMutationInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := usable(); err != nil {
			return nil, err
		}
		if err := d.Agents.DisableToken(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail("disable token", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getEnvironmentToken",
		Method:      http.MethodGet,
		Path:        "/api/environments/{envId}/token",
		Summary:     "Show which join token an environment has",
		Description: "The generation, when and by whom it was made, and whether it is disabled. Never the token: it is stored only as a hash. " +
			"404 environment_not_found. Needs environments.token.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsToken, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *EnvironmentPathInput) (*TokenStatusOutput, error) {
		if err := usable(); err != nil {
			return nil, err
		}
		st, err := d.Agents.TokenStatus(ctx, in.EnvID)
		if err != nil {
			return nil, fail("token status", err)
		}
		return &TokenStatusOutput{CacheControl: "no-store", Body: TokenStatusBody{
			Generation: st.Generation, CreatedAt: rfc3339(st.CreatedAt), CreatedBy: st.CreatedBy, Disabled: st.Disabled,
		}}, nil
	})
}
