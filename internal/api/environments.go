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
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
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
	problemPublicURLRequired      = "public_url_required"
	problemAgentsUnavailable      = "agents_unavailable"
)

// EnvironmentsDeps are the dependencies of the environment routes.
type EnvironmentsDeps struct {
	Envs *envs.Service
	// Agents creates agent environments and their join tokens. Nil means the
	// server cannot add one.
	Agents *agents.Service
	// Instance supplies the address agents dial: the agent URL, else the public
	// URL, with its base path (D-18). Nil, or neither URL set, means no agent
	// environment can be added.
	Instance *instance.Instance
	// TLSPin returns the SPKI pin of the self-signed certificate Bayplan serves, or
	// "" when it serves a supplied certificate or plain HTTP, and then the install
	// carries no pin (D-06). Nil means none.
	TLSPin func() string
	// Checker filters the list to the environments the caller may read.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EnvironmentIssueBody is one reason an environment is degraded: a node that is
// missing, cannot reach Docker, runs an unsupported engine, or has a certificate or
// clock that needs attention. It carries only a fixed code and a node name.
type EnvironmentIssueBody struct {
	Code  string `json:"code" doc:"A stable code: agent_offline, engine_unreachable, unsupported_engine, cert_expiring or clock_skew"`
	Node  string `json:"node,omitempty" doc:"The node's host name, at most 64 characters; absent for an agent that has not enrolled yet"`
	Since string `json:"since,omitempty" format:"date-time" doc:"When this began"`
}

// EnvironmentStatusBody is the health of an environment: the rollup of its agents
// for an agent environment (D-33), what the health monitor believes for the local
// socket.
type EnvironmentStatusBody struct {
	State       string                 `json:"state" enum:"unknown,online,offline,unsupported,pending,reconnecting,degraded,revoked,cluster_changed" doc:"online: working. degraded: working, but see issues. offline: no agent can be reached, or the host's engine does not answer. unsupported: the engine answers but Bayplan does not manage it. pending: no agent has enrolled yet. reconnecting: the server has just started and is waiting for its agents. revoked: every agent was revoked. cluster_changed: the swarm was re-initialised. unknown: the local socket has not been probed yet"`
	Since       string                 `json:"since,omitempty" format:"date-time" doc:"When the environment entered this state; for offline, when contact was lost"`
	Reason      string                 `json:"reason,omitempty" doc:"Why it is not online, as a stable code. An agent environment uses agent_offline, timeout, engine_unreachable, unsupported_engine, revoked, cluster_changed, not_a_manager, no_quorum or cert_expiring; the local socket uses unreachable, api_too_old, podman, rootless or docker_desktop"`
	LastAttempt string                 `json:"lastAttempt,omitempty" format:"date-time" doc:"When the most recent probe of the environment's engine finished"`
	Issues      []EnvironmentIssueBody `json:"issues,omitempty" doc:"What is wrong with a degraded environment, worst first, then by node"`
}

// statusBody builds the status of an environment from its rolled-up health and the
// time of the last probe of its engine.
func statusBody(h envs.Health, lastAttempt time.Time) EnvironmentStatusBody {
	body := EnvironmentStatusBody{
		State: string(h.State), Since: rfc3339(h.Since), Reason: h.Reason, LastAttempt: rfc3339(lastAttempt),
	}
	for _, is := range h.Issues {
		body.Issues = append(body.Issues, EnvironmentIssueBody{Code: is.Code, Node: is.Node, Since: rfc3339(is.Since)})
	}
	return body
}

// EnvironmentEngineBody identifies the engine behind an environment.
type EnvironmentEngineBody struct {
	Name          string `json:"name,omitempty" doc:"For example Docker Engine"`
	Version       string `json:"version,omitempty" doc:"The engine version"`
	APIVersion    string `json:"apiVersion,omitempty" doc:"The newest Engine API version the daemon speaks"`
	NegotiatedAPI string `json:"negotiatedApi,omitempty" doc:"The API version Bayplan uses with it"`
}

// EnvironmentNodesBody is how many of an environment's nodes are ready of how many it has.
type EnvironmentNodesBody struct {
	Ready int `json:"ready" doc:"Nodes whose agent is connected or was heard from within the last 30 seconds"`
	Total int `json:"total" doc:"Nodes the environment should have: one per agent that is not revoked"`
}

// EnvironmentFailingBody is how many of an environment's services are failing of how many it has.
type EnvironmentFailingBody struct {
	Failing int `json:"failing" doc:"Services with tasks that keep failing"`
	Total   int `json:"total" doc:"Services in the environment"`
}

// EnvironmentAgentsBody is how many of a swarm's expected agents are connected.
type EnvironmentAgentsBody struct {
	Connected int `json:"connected" doc:"Agents with a live connection"`
	Expected  int `json:"expected" doc:"Agents the swarm should have: one per node whose agent was not revoked"`
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

	EnvType         string                           `json:"envType,omitempty" enum:"swarm,standalone" doc:"What the environment is; absent until it is known. An agent environment gets it from its first agent"`
	IsLocalSocket   bool                             `json:"isLocalSocket" doc:"True for the Docker socket on the host Bayplan runs on, as opposed to hosts that dial in through an agent"`
	Nodes           Nullable[EnvironmentNodesBody]   `json:"nodes" doc:"Ready and total nodes; null while no agent has enrolled and for a local swarm, whose nodes are read from the cluster"`
	FailingServices Nullable[EnvironmentFailingBody] `json:"failingServices" doc:"Failing and total services; null until service data is known"`
	Agents          Nullable[EnvironmentAgentsBody]  `json:"agents" doc:"Connected and expected agents; null except on a swarm"`
	AgentVersion    *string                          `json:"agentVersion" nullable:"true" doc:"The oldest agent version in the environment; null when it has no agent, and for the local socket"`
	UpdateAvailable bool                             `json:"updateAvailable" doc:"True when an agent is older than this server by a minor or a patch version"`
	EngineVersion   *string                          `json:"engineVersion" nullable:"true" doc:"The Docker engine version, as the engine reported it; null until it has answered"`
	LastSeen        *string                          `json:"lastSeen" nullable:"true" format:"date-time" doc:"The last sign of life of any agent, or for the local socket the last probe that found the engine online; null when never seen"`
}

// InstallSwarmBody is the swarm install: one command for a manager, or a stack file.
type InstallSwarmBody struct {
	Command       string `json:"command" doc:"One line to run on any swarm manager. It creates a Docker secret from the join token and starts the agent as a global service. Starts with a space so a shell set to ignore space-led commands keeps it out of its history"`
	StackFile     string `json:"stackFile" doc:"The same service as a stack file for docker stack deploy. A stack file cannot create a secret from text, so it names the secret as external and its first comment line holds the one-line command that creates it"`
	RotateCommand string `json:"rotateCommand,omitempty" doc:"One line that swaps a running service over to this token's secret without stopping it. Present only for a token that replaces an earlier one, never for a first install"`
}

// InstallStandaloneBody is the standalone install: one command, or a compose file.
type InstallStandaloneBody struct {
	Command     string `json:"command" doc:"One line that starts the agent on a single Docker host. Starts with a space so a shell set to ignore space-led commands keeps it out of its history"`
	ComposeFile string `json:"composeFile" doc:"The same container as a compose file for docker compose up, with the same environment as the command"`
}

// InstallCleanupBody is how to take the agent off a host by hand.
type InstallCleanupBody struct {
	Swarm      string `json:"swarm" doc:"Commands that remove the service and the secret on a manager, and the agent volume on every node"`
	Standalone string `json:"standalone" doc:"One line that removes the agent container and its volume"`
}

// InstallBody is what an admin needs to connect hosts to a new agent environment.
// It is in the answer to adding one and nowhere else: the join token is stored
// only as a hash and cannot be shown again. Every text is built in one place,
// agents.BuildInstall.
type InstallBody struct {
	JoinToken     string                `json:"joinToken" doc:"The join token the agent presents once to enrol. Shown only in this answer; it is inside the commands and files below"`
	AgentURL      string                `json:"agentUrl" doc:"The address the agent dials, for example wss://bayplan.example.com/agent/connect. It comes from the agent URL setting, else the public URL, never from the request"`
	Image         string                `json:"image" doc:"The agent image. Its tag is this server's exact version, or edge for a development or edge server"`
	Pin           string                `json:"pin,omitempty" doc:"The SPKI pin of the self-signed certificate Bayplan serves, which the agent trusts instead of system roots. Present only when Bayplan serves its own self-signed certificate"`
	SecretName    string                `json:"secretName" doc:"The Docker secret the swarm install creates for this token, bayplan-agent-token-g followed by the generation"`
	Generation    int                   `json:"generation" doc:"Which token this is for the environment, from 1; each rotation adds one"`
	Swarm         InstallSwarmBody      `json:"swarm" doc:"The install for a swarm"`
	Standalone    InstallStandaloneBody `json:"standalone" doc:"The install for a standalone host"`
	ManualCleanup InstallCleanupBody    `json:"manualCleanup" doc:"Commands that remove the agent by hand, for a host that is offline when its environment is removed"`
}

// AddEnvironmentBody is the answer to adding an environment: the environment, and
// for an agent environment the install details.
type AddEnvironmentBody struct {
	EnvironmentBody
	Install *InstallBody `json:"install,omitempty" doc:"Present for an agent environment only: the join token, the address the agent dials, and the commands and files that install it"`
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
	o := e.Overview
	body := EnvironmentBody{
		ID: e.ID, Name: e.Name, Kind: e.Kind,
		Type:       p.Type,
		IsSelfHost: e.Kind == envs.KindLocal,
		Enabled:    e.Enabled,
		CreatedAt:  rfc3339(e.CreatedAt),
		Status:     statusBody(e.Health, e.Status.LastAttempt),
		Engine: EnvironmentEngineBody{
			Name: p.EngineName, Version: p.EngineVersion, APIVersion: p.APIVersion, NegotiatedAPI: p.NegotiatedAPI,
		},
		EnvType: o.EnvType, IsLocalSocket: o.IsLocalSocket, UpdateAvailable: o.UpdateAvailable,
	}
	if o.Nodes != nil {
		body.Nodes = Some(EnvironmentNodesBody{Ready: o.Nodes.Ready, Total: o.Nodes.Total})
	}
	if o.Failing != nil {
		body.FailingServices = Some(EnvironmentFailingBody{Failing: o.Failing.Failing, Total: o.Failing.Total})
	}
	if o.Agents != nil {
		body.Agents = Some(EnvironmentAgentsBody{Connected: o.Agents.Connected, Expected: o.Agents.Expected})
	}
	if o.AgentVersion != "" {
		body.AgentVersion = &o.AgentVersion
	}
	if o.EngineVersion != "" {
		body.EngineVersion = &o.EngineVersion
	}
	if !o.LastSeen.IsZero() {
		seen := rfc3339(o.LastSeen)
		body.LastSeen = &seen
	}
	return body
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
// needs environments.create and nothing is added automatically.
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
		Description: "Each environment comes with its health: for an agent environment the rollup of its agents (pending, reconnecting, online, degraded with its issues, offline, unsupported, revoked or cluster_changed, each with a stable reason code and since), for the local socket the health monitor's status. An environment the caller does not hold " +
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
			"and the swarm and standalone install commands and files, built from the address agents dial (the agent URL, else the public URL, " +
			"with its base path); 409 " + problemPublicURLRequired + " when Bayplan has neither. 422 invalid_name. " +
			"The name defaults to the hostname the daemon, or the first agent, reports. Needs environments.create. Audited as environment.added.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsCreate, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *AddEnvironmentInput) (*AddEnvironmentOutput, error) {
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
		Description: "Needs environments.manage. 422 invalid_name unless the name is 1 to 64 characters. Audited as environment.renamed with the old and new name.",
		Tags:        []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RenameEnvironmentInput) (*EnvironmentOutput, error) {
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
		Description: "Needs environments.delete. Stops managing the host. Changes nothing in Docker: its containers, networks and volumes are untouched " +
			"(D-20). Audited as environment.removed.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsDelete, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RemoveEnvironmentInput) (*struct{}, error) {
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
// token and the install built from it. The token is in this answer only: the
// server keeps its hash, and it is in no audit row.
func addAgentEnvironment(ctx context.Context, d EnvironmentsDeps, fail func(string, error) error, p *rbac.Principal, in *AddEnvironmentInput) (*AddEnvironmentOutput, error) {
	if d.Agents == nil {
		return nil, httpx.Problem(http.StatusConflict, problemAgentsUnavailable, "This server cannot add agent environments.")
	}
	endpoint := ""
	if d.Instance != nil {
		endpoint = d.Instance.AgentEndpoint()
	}
	if endpoint == "" {
		return nil, httpx.Problem(http.StatusConflict, problemPublicURLRequired,
			"Bayplan has no public URL or agent URL, so it cannot tell an agent where to connect. Set one in Settings, or with BAYPLAN_PUBLIC_URL or BAYPLAN_AGENT_URL.")
	}
	created, err := d.Agents.CreateEnvironment(ctx, p.AuditActor(), in.Body.Name, httpx.ClientIP(ctx), in.UserAgent)
	if err != nil {
		return nil, fail("add agent environment", err)
	}
	e, err := d.Envs.Get(ctx, created.EnvID)
	if err != nil {
		return nil, fail("read the new environment", err)
	}
	pin := ""
	if d.TLSPin != nil {
		pin = d.TLSPin()
	}
	install, err := agents.BuildInstall(agents.InstallParams{
		Token: created.JoinToken, Endpoint: endpoint, Pin: pin, ServerVersion: buildinfo.Get().Version, Generation: created.Generation,
	})
	if err != nil {
		// The environment exists and its token cannot be shown again. A value that
		// breaks the quoting is a server bug, so it is logged and not hidden.
		return nil, fail("build the install", err)
	}
	return &AddEnvironmentOutput{CacheControl: "no-store", Body: AddEnvironmentBody{
		EnvironmentBody: environmentBody(e),
		Install:         installBody(install),
	}}, nil
}

// installBody maps the built install to its API shape.
func installBody(in agents.Install) *InstallBody {
	return &InstallBody{
		JoinToken: in.JoinToken, AgentURL: in.AgentURL, Image: in.Image, Pin: in.Pin,
		SecretName: in.SecretName, Generation: in.Generation,
		Swarm:         InstallSwarmBody{Command: in.SwarmCommand, StackFile: in.SwarmStackFile, RotateCommand: in.SwarmRotateCommand},
		Standalone:    InstallStandaloneBody{Command: in.StandaloneCommand, ComposeFile: in.StandaloneComposeFile},
		ManualCleanup: InstallCleanupBody{Swarm: in.SwarmCleanup, Standalone: in.StandaloneCleanup},
	}
}
