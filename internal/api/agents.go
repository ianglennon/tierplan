// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// AgentSessions is what the node list reads of the live agents: the tunnel hub.
type AgentSessions interface {
	// Session is the live session of an agent, or nil.
	Session(agentID string) *tunnel.Session
	// Views is what the hub remembers of the agents of an environment that have
	// connected since the server started.
	Views(envID string) []tunnel.AgentLive
}

// AgentsDeps are the dependencies of the agent routes.
type AgentsDeps struct {
	// Cluster says which agent runs on the leading manager of a swarm, so the node
	// list can call it the leader. Nil means none is known and every manager is
	// listed as a manager.
	Cluster AgentCluster
	// Agents rotates and disables join tokens. Nil means the routes answer 409
	// agents_unavailable.
	Agents *agents.Service
	// Live is where the node list reads who is connected now and what each agent
	// reported. Nil means nobody is: every node shows as not connected.
	Live AgentSessions
	// Instance supplies the address agents dial, as for adding an environment.
	Instance *instance.Instance
	// TLSPin returns the SPKI pin of the self-signed certificate Bayplan serves, or
	// "" when it serves none. Nil means none.
	TLSPin func() string
	// AgentAllowInsecure is the server's BAYPLAN_AGENT_ALLOW_INSECURE, a development
	// switch. It only decides whether the install and the manual update command for a
	// plain ws:// address also tell the agent to accept it (T-02-31).
	AgentAllowInsecure bool
	// Local says which node of an environment is the folded local host, so the node
	// list can show it beside the agents. Nil means none is (D-48).
	Local FoldedNodes
	// Updater updates agents and says how each update is going. Nil means the node
	// list shows every agent idle and the update routes answer 409 agents_unavailable.
	Updater *agents.Updater
	// ServerVersion is what the node list measures an agent's version against to
	// offer the manual update command. Empty means this build's own version.
	ServerVersion string
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// FoldedNodes is what the node list asks about the local host folded into an environment.
type FoldedNodes interface {
	// FoldedNode is the folded local host of the environment, and false for one with none.
	FoldedNode(ctx context.Context, envID string) (envs.FoldedNode, bool)
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

// AgentPathInput carries {envId} and {agentId} for the routes that act on one agent.
type AgentPathInput struct {
	EnvironmentPathInput
	AgentID string `path:"agentId" doc:"The agent ID, as listed by GET /api/env/{envId}/agents"`
}

// RevokeAgentBody is the optional body of a revoke.
type RevokeAgentBody struct {
	Reason string `json:"reason,omitempty" maxLength:"256" doc:"Why the node is revoked, for the audit log and the node table. Optional"`
}

// RevokeAgentInput is a revoke of one agent.
type RevokeAgentInput struct {
	AgentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      *RevokeAgentBody
}

// UnblockAgentInput is an unblock of one agent's host.
type UnblockAgentInput struct {
	AgentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// AdoptClusterBody is the answer to adopting a new cluster.
type AdoptClusterBody struct {
	ClusterID         string `json:"clusterId" doc:"The swarm cluster ID the environment is bound to now"`
	PreviousClusterID string `json:"previousClusterId,omitempty" doc:"The cluster ID it was bound to; absent if it had none"`
}

// AdoptClusterOutput wraps AdoptClusterBody for huma.
type AdoptClusterOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AdoptClusterBody
}

// DenialBody is the last request an agent refused by its own policy.
type DenialBody struct {
	Endpoint string `json:"endpoint" doc:"The Docker API path the agent refused"`
	At       string `json:"at" format:"date-time" doc:"When it refused"`
}

// UpdateStateBody is how an agent's update is going.
type UpdateStateBody struct {
	State  string `json:"state" enum:"idle,updating,failed" doc:"idle: nothing is running. updating: an update was sent and the agent has not yet come back on the new version. failed: the last update did not finish, and reason says why, until the agent runs the server's version or another update is started"`
	Reason string `json:"reason,omitempty" doc:"Why the last update failed, as a stable code: the agent's own (image_not_own_repository, not_a_manager, self_update_disabled, insecure_session, busy, update_failed and so on) or no_answer, no_new_version or rolled_back. Present only when state is failed"`
}

// UpdateAgentsBody is the answer to updating every agent of an environment.
type UpdateAgentsBody struct {
	Started int `json:"started" doc:"How many updates were sent: one for a swarm, whose agent service rolls every node, otherwise one per agent that was behind. 0 when none was needed, or one was already running"`
}

// UpdateAgentsOutput wraps UpdateAgentsBody for huma.
type UpdateAgentsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateAgentsBody
}

// UpdateAgentInput is an update of one agent.
type UpdateAgentInput struct {
	AgentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// UpdateAgentsInput is an update of every agent of an environment.
type UpdateAgentsInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// AgentBody is one node's agent.
type AgentBody struct {
	ID                  string          `json:"id" doc:"The agent ID"`
	Node                string          `json:"node" doc:"The hostname the agent reported when it enrolled"`
	Role                string          `json:"role" enum:"leader,manager,worker,standalone,local" doc:"What the node is: the swarm manager that leads, another swarm manager, a swarm worker or a standalone host. leader is as of the last read of the cluster, and is only ever an active manager. local is this Bayplan host, folded into the swarm environment and reached through the Docker socket: it has no agent, certificate or clock, and none of the agent actions apply to it"`
	Status              string          `json:"status" enum:"active,revoked" doc:"revoked means an administrator revoked the agent: it is disconnected, its certificate is refused, and it cannot come back"`
	Blocked             bool            `json:"blocked" doc:"True while the host's engine ID and swarm node ID are blocked from enrolling again, until an administrator unblocks them"`
	Connected           bool            `json:"connected" doc:"Whether the agent has a live connection now"`
	AgentVersion        string          `json:"agentVersion" doc:"The agent's version when it last connected or enrolled"`
	EngineVersion       string          `json:"engineVersion,omitempty" doc:"The Docker Engine version the agent reported. Present only while it is connected"`
	CertFingerprint     string          `json:"certFingerprint,omitempty" doc:"The full SHA-256 of the agent's certificate, in hex. Absent for the local host"`
	CertNotAfter        string          `json:"certNotAfter,omitempty" format:"date-time" doc:"When the agent's certificate expires. Absent for the local host"`
	RenewalFailing      bool            `json:"renewalFailing" doc:"True when the certificate has less than 7 days left, which means it should have renewed already and has not, or has expired. Never set for a revoked agent"`
	ClockOffsetSeconds  int             `json:"clockOffsetSeconds" doc:"How far the agent's clock is ahead of the server's, in seconds, measured when it last connected; negative is behind, 0 when it has not connected since the server started"`
	LastSeen            string          `json:"lastSeen,omitempty" format:"date-time" doc:"When the agent was last heard from. Absent if it never has been"`
	ReadOnly            bool            `json:"readOnly" doc:"The host owner set the agent to read-only. Reported only while it is connected"`
	SelfUpdate          bool            `json:"selfUpdate" doc:"The host owner allowed the agent to update itself. Reported only while it is connected"`
	LastDenial          *DenialBody     `json:"lastDenial,omitempty" doc:"The last request the agent refused by its own policy, while it is connected. Absent if it has refused none"`
	UpdateState         UpdateStateBody `json:"updateState" doc:"How the agent's update is going. Held in memory, so it is idle again after the server restarts"`
	UpdateAvailable     bool            `json:"updateAvailable" doc:"True for an agent that is not revoked and runs an older release than this server, so an update would bring it up to date. Never true for a development build on either side, which cannot be compared"`
	ManualUpdateCommand string          `json:"manualUpdateCommand,omitempty" doc:"The one-line command that updates this agent by hand, present when the agent is behind and Bayplan cannot update it: it is outside the server's version window, it is too old to update itself, or its host owner turned self-update off"`
	RevokedAt           string          `json:"revokedAt,omitempty" format:"date-time" doc:"When the agent was revoked"`
	RevokeReason        string          `json:"revokeReason,omitempty" doc:"The reason given when it was revoked"`
}

// AgentListBody is every agent of an environment.
type AgentListBody struct {
	Items         []AgentBody        `json:"items" doc:"One entry per enrolled node, problems first: revoked agents, then agents that are not connected, then the rest by node name. A node that was revoked and enrolled again has an entry for each agent"`
	ManualCleanup InstallCleanupBody `json:"manualCleanup" doc:"The commands that take the agent off a host by hand, for the hosts Bayplan cannot uninstall when the environment is removed. The swarm secret in them is named for the join token's current generation. They carry no token"`
}

// AgentListOutput wraps AgentListBody for huma.
type AgentListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AgentListBody
}

// agentBodies merges the database's view of an environment's agents with the
// live one: connected, what the agent reported and when it was last heard from.
func agentBodies(nodes []agents.Node, live AgentSessions, envID, leaderID string, now time.Time, u updateView) []AgentBody {
	views := map[string]tunnel.AgentLive{}
	if live != nil {
		for _, v := range live.Views(envID) {
			views[v.AgentID] = v
		}
	}
	items := make([]AgentBody, 0, len(nodes))
	for _, n := range nodes {
		role := n.Role
		if role == "manager" && n.Status == "active" && leaderID != "" && n.ID == leaderID {
			role = "leader"
		}
		b := AgentBody{
			ID: n.ID, Node: n.Name, Role: role, Status: n.Status, Blocked: n.Blocked,
			AgentVersion: n.AgentVersion, CertFingerprint: n.CertFingerprint, CertNotAfter: rfc3339(n.CertNotAfter),
			RevokedAt: rfc3339(n.RevokedAt), RevokeReason: n.RevokeReason,
			// The same threshold as the environment's health (envs.CertWarnBefore): a
			// certificate this close to its end should have renewed at a third of
			// its life left, so something is stopping it. A revoked agent is not
			// trying.
			RenewalFailing: n.Status == "active" && n.CertNotAfter.Sub(now) < envs.CertWarnBefore,
		}
		lastSeen := n.LastSeen
		if v, ok := views[n.ID]; ok {
			b.AgentVersion = cmpOr(v.Version, b.AgentVersion)
			b.ClockOffsetSeconds = int(v.ClockOffset.Round(time.Second) / time.Second)
			if v.LastSeen.After(lastSeen) {
				lastSeen = v.LastSeen
			}
			b.Connected = v.Connected && n.Status == "active"
		}
		if b.Connected && live != nil {
			if s := live.Session(n.ID); s != nil {
				b.EngineVersion = s.Facts.EngineVersion
				b.ReadOnly, b.SelfUpdate = s.Facts.Policy.ReadOnly, s.Facts.Policy.SelfUpdate
				if d := s.Status().LastDenial; d != nil {
					b.LastDenial = &DenialBody{Endpoint: d.Endpoint, At: rfc3339(d.At)}
				}
			} else {
				b.Connected = false
			}
		}
		b.LastSeen = rfc3339(lastSeen)
		if u.state != nil {
			st := u.state(n.ID)
			b.UpdateState = UpdateStateBody{State: st.State, Reason: st.Reason}
		}
		if b.UpdateState.State == "" {
			b.UpdateState.State = agents.UpdateIdle
		}
		b.UpdateAvailable = n.Status == "active" && tunnel.Behind(u.serverVersion, b.AgentVersion)
		if n.Status == "active" && agents.ManualUpdateNeeded(u.serverVersion, b.AgentVersion, b.Connected, b.SelfUpdate) {
			b.ManualUpdateCommand = u.command(b.Role)
		}
		items = append(items, b)
	}
	sortAgentBodies(items)
	return items
}

// sortAgentBodies puts the problems first: revoked agents, then those that are not
// connected, then the rest by node name.
func sortAgentBodies(items []AgentBody) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if ra, rb := a.Status == "revoked", b.Status == "revoked"; ra != rb {
			return ra
		}
		if a.Connected != b.Connected {
			return !a.Connected
		}
		if na, nb := strings.ToLower(a.Node), strings.ToLower(b.Node); na != nb {
			return na < nb
		}
		return a.ID < b.ID
	})
}

// localBody is the folded local host as a row of the node list. It has no agent, so it
// has no certificate, no clock offset, no version to update and no actions.
func localBody(n envs.FoldedNode) AgentBody {
	return AgentBody{
		ID: n.ID, Node: n.Node, Role: "local", Status: "active", Connected: n.Online, EngineVersion: n.EngineVersion,
		UpdateState: UpdateStateBody{State: agents.UpdateIdle},
	}
}

// updateView is what the node list needs to report updates: how each is going, the
// server's version, and the command that updates an agent of a role by hand.
type updateView struct {
	state         func(agentID string) agents.UpdateState
	serverVersion string
	command       func(role string) string
}

// updates builds the view the node list uses from the route's dependencies. A
// standalone agent's manual update command is empty (and the SPA shows none) when
// the address agents are given is plain ws:// for another host with the switch off:
// agents.ManualUpdateCommand answers "" there, so nothing is checked here.
func (d AgentsDeps) updates() updateView {
	version := cmpOr(d.ServerVersion, buildinfo.Get().Version)
	v := updateView{serverVersion: version, command: func(role string) string {
		endpoint := ""
		if d.Instance != nil {
			endpoint = d.Instance.AgentEndpoint()
		}
		pin := ""
		if d.TLSPin != nil {
			pin = d.TLSPin()
		}
		envType := agents.EnvTypeStandalone
		if role == "leader" || role == "manager" || role == "worker" {
			envType = agents.EnvTypeSwarm
		}
		return agents.ManualUpdateCommand(envType, agents.AgentImage(version), agents.InstallParams{Endpoint: endpoint, Pin: pin, ServerVersion: version, AllowInsecure: d.AgentAllowInsecure})
	}}
	if d.Updater != nil {
		v.state = d.Updater.State
	}
	return v
}

// cmpOr is v unless it is empty, then fallback.
func cmpOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// AgentCluster is what the node list asks of the cluster reader.
type AgentCluster interface {
	// LeaderAgent is the ID of the agent on the leading manager of the environment as
	// of the last read of its cluster, or empty. It does no I/O.
	LeaderAgent(envID string) string
}

// leaderAgent is the agent on the leading manager, or empty when no cluster reader
// is set.
func leaderAgent(d AgentsDeps, envID string) string {
	if d.Cluster == nil {
		return ""
	}
	return d.Cluster.LeaderAgent(envID)
}

// RegisterAgents registers the agent routes: the join token (rotate, disable and
// inspect, environments.token, a dangerous global permission, because a token is
// what lets a new host in, D-02), the node list (environments.read on the
// environment) and revoke and unblock (environments.revoke, D-08, D-09).
func RegisterAgents(r *httpx.Router, d AgentsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	fail := func(what string, err error) error {
		switch {
		case errors.Is(err, agents.ErrEnvironmentNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		case errors.Is(err, agents.ErrAgentNotFound):
			return httpx.Problem(http.StatusNotFound, problemAgentNotFound, "No such agent in this environment.")
		case errors.Is(err, agents.ErrAlreadyRevoked):
			return httpx.Problem(http.StatusConflict, problemAgentAlreadyRevoked, "That node was revoked while you were looking at it.")
		case errors.Is(err, agents.ErrNotBlocked):
			return httpx.Problem(http.StatusConflict, problemNotBlocked, "That node's host is not blocked.")
		case errors.Is(err, agents.ErrNoClusterChange):
			return httpx.Problem(http.StatusConflict, problemNoClusterChange, "This environment has no new cluster to adopt. It may have been adopted a moment ago.")
		case errors.Is(err, agents.ErrAgentOffline):
			return httpx.Problem(http.StatusConflict, problemAgentOffline, "That agent is not connected, so it cannot be told to update. Update it by hand, or wait until it is back.")
		case errors.Is(err, agents.ErrNoManager):
			return httpx.Problem(http.StatusConflict, problemNoManager, "No manager of this swarm is connected, and a swarm's agents are updated through a manager.")
		case errors.Is(err, agents.ErrSelfUpdateOff):
			return httpx.Problem(http.StatusConflict, problemSelfUpdateDisabled, "The owner of that host turned agent self-update off (BAYPLAN_AGENT_SELF_UPDATE=false). Update it by hand.")
		case errors.Is(err, agents.ErrAgentUpToDate):
			return httpx.Problem(http.StatusConflict, problemAgentUpToDate, "That agent already runs this server's version.")
		case errors.Is(err, agents.ErrUpdateInProgress):
			return httpx.Problem(http.StatusConflict, problemUpdateInProgress, "An update of that agent is already running.")
		case errors.Is(err, agents.ErrClusterInUse):
			return httpx.Problem(http.StatusConflict, problemClusterInUse, "Another environment already manages that cluster, so this one cannot adopt it.")
		}
		d.Log.Error("agent request failed", "step", what, "error", err)
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
			"address to give agents, and 409 " + problemInsecureAgentURL + " when that address is plain ws:// for a host that is not this machine " +
			"and BAYPLAN_AGENT_ALLOW_INSECURE is off; the current token stays in both cases. Needs environments.token. Audited as environment.token_rotated with the new generation.",
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
		if tunnel.PlainAddressRefused(endpoint, d.AgentAllowInsecure) {
			return nil, insecureAgentURLProblem()
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
			AllowInsecure: d.AgentAllowInsecure,
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

	httpx.Register(r, huma.Operation{
		OperationID: "listEnvironmentAgents",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/agents",
		Summary:     "List the agents of an environment's nodes",
		Description: "Every enrolled agent with its node name, role, status, whether its host is blocked, whether it is connected, versions, " +
			"the full certificate fingerprint and expiry, clock offset, when it was last seen, the policy flags the host owner set and the last " +
			"request it refused. Problems come first: revoked agents, then agents that are not connected, then the rest by node name. A " +
			"local-socket environment has none, and an empty manualCleanup. Never carries a key, a token or a serial. 404 environment_not_found when the environment does " +
			"not exist or the caller may not read it. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment}, func(ctx context.Context, in *EnvironmentPathInput) (*AgentListOutput, error) {
		if err := usable(); err != nil {
			return nil, err
		}
		nodes, err := d.Agents.ListNodes(ctx, in.EnvID)
		if err != nil {
			return nil, fail("list agents", err)
		}
		// A local-socket environment has no agents and no token, and so no cleanup.
		var swarm, standalone string
		switch st, err := d.Agents.TokenStatus(ctx, in.EnvID); {
		case err == nil:
			swarm, standalone = agents.CleanupCommands(st.Generation)
		case !errors.Is(err, agents.ErrEnvironmentNotFound):
			return nil, fail("token status", err)
		}
		items := agentBodies(nodes, d.Live, in.EnvID, leaderAgent(d, in.EnvID), time.Now(), d.updates())
		if d.Local != nil {
			if n, ok := d.Local.FoldedNode(ctx, in.EnvID); ok {
				items = append(items, localBody(n))
				sortAgentBodies(items)
			}
		}
		return &AgentListOutput{CacheControl: "no-store", Body: AgentListBody{
			Items:         items,
			ManualCleanup: InstallCleanupBody{Swarm: swarm, Standalone: standalone},
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "revokeEnvironmentAgent",
		Method:        http.MethodPost,
		Path:          "/api/environments/{envId}/agents/{agentId}/revoke",
		Summary:       "Revoke a node's agent",
		DefaultStatus: http.StatusNoContent,
		Description: "The agent is disconnected within a second, its certificate is refused from now on, and its host's engine ID and swarm " +
			"node ID are blocked so it cannot enrol again with a join token that is still valid, until the node is unblocked. Nothing on the host " +
			"is changed. The optional reason is kept in the audit log. 404 environment_not_found or agent_not_found; 409 agent_already_revoked " +
			"when it was revoked already, by this admin or another. Needs environments.revoke. Audited as agent.revoked and agent.blocked.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRevoke, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *RevokeAgentInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := usable(); err != nil {
			return nil, err
		}
		reason := ""
		if in.Body != nil {
			reason = in.Body.Reason
		}
		if err := d.Agents.Revoke(ctx, p.AuditActor(), in.EnvID, in.AgentID, reason, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail("revoke agent", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "unblockEnvironmentAgent",
		Method:        http.MethodPost,
		Path:          "/api/environments/{envId}/agents/{agentId}/unblock",
		Summary:       "Let a revoked node's host enrol again",
		DefaultStatus: http.StatusNoContent,
		Description: "Lifts the blocks the agent's revoke made, so its host can enrol again with the environment's join token, as a new agent. The " +
			"revoked agent stays revoked. An agent that still has its join token drops its revoked identity and enrols again by itself at its next try, " +
			"within a minute; one without a token waits at its backoff limit until it is installed again. 404 environment_not_found or agent_not_found; 409 not_blocked when there is nothing to lift. Needs environments.revoke. " +
			"Audited as agent.unblocked.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRevoke, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UnblockAgentInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := usable(); err != nil {
			return nil, err
		}
		if err := d.Agents.Unblock(ctx, p.AuditActor(), in.EnvID, in.AgentID, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail("unblock agent", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "adoptEnvironmentCluster",
		Method:      http.MethodPost,
		Path:        "/api/environments/{envId}/adopt-cluster",
		Summary:     "Adopt the new cluster of a re-initialised swarm",
		Description: "A manager reported a swarm cluster ID different from the one the environment was bound to, so the environment is " +
			"cluster_changed and nothing was rebound. This binds it to the new cluster (the pendingClusterId the environment shows), clears " +
			"cluster_changed and audits environment.cluster_adopted with the cluster it was and the one it is. The environment's agents are not " +
			"changed. 404 environment_not_found; 409 no_cluster_change when none is recorded, or " + problemClusterInUse + " when another " +
			"environment manages the new cluster. Needs environments.manage.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *TokenMutationInput) (*AdoptClusterOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := usable(); err != nil {
			return nil, err
		}
		got, err := d.Agents.AdoptCluster(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("adopt cluster", err)
		}
		return &AdoptClusterOutput{CacheControl: "no-store", Body: AdoptClusterBody{ClusterID: got.To, PreviousClusterID: got.From}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "updateEnvironmentAgent",
		Method:        http.MethodPost,
		Path:          "/api/environments/{envId}/agents/{agentId}/update",
		Summary:       "Update an agent to this server's version",
		DefaultStatus: http.StatusAccepted,
		Description: "Tells the agent to replace itself with this server's own agent image, for a development server the edge image. The update is " +
			"started, not finished: the node list's updateState says updating until the agent reconnects on the new version. In a swarm the agent updates " +
			"its own service, which rolls every node, so one request through a connected manager updates every agent that is behind. It works whatever " +
			"the environment's autoUpdate is. 404 environment_not_found or agent_not_found; 409 agent_offline, no_manager_connected, self_update_disabled " +
			"(the host owner refused), agent_up_to_date or update_in_progress. Needs environments.manage. Audited as agent.updated, or agent.update_failed with " +
			"its reason when the update does not finish.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UpdateAgentInput) (*struct{}, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Updater == nil {
			return nil, httpx.Problem(http.StatusConflict, problemAgentsUnavailable, "This server cannot update agents.")
		}
		if err := d.Updater.UpdateAgent(ctx, p.AuditActor(), in.EnvID, in.AgentID, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail("update agent", err)
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "updateEnvironmentAgents",
		Method:        http.MethodPost,
		Path:          "/api/environments/{envId}/update-agents",
		Summary:       "Update every agent of an environment that is behind",
		DefaultStatus: http.StatusAccepted,
		Description: "Starts an update for each connected agent older than this server: one for a swarm, one per agent for standalone hosts. Agents whose " +
			"host owner refused self-update, agents that are up to date and agents already updating are skipped, so started can be 0. 404 environment_not_found; " +
			"409 agent_offline when the environment has agents and none is connected, or no_manager_connected for a swarm without a connected manager. " +
			"Needs environments.manage. Audited like an update of one agent.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsManage, Scope: rbac.ScopeGlobal}, func(ctx context.Context, in *UpdateAgentsInput) (*UpdateAgentsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Updater == nil {
			return nil, httpx.Problem(http.StatusConflict, problemAgentsUnavailable, "This server cannot update agents.")
		}
		n, err := d.Updater.UpdateAll(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("update agents", err)
		}
		return &UpdateAgentsOutput{CacheControl: "no-store", Body: UpdateAgentsBody{Started: n}}, nil
	})
}
