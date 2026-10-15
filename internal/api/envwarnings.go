// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// EnvWarningsDeps are the dependencies of the environment warnings route.
type EnvWarningsDeps struct {
	Envs *envs.Service
	// Checker says which environments the caller may read.
	Checker *rbac.Checker
	// Clusters is the cluster reader whose last read says which services are in a restart loop
	// (RES-13). Nil lists none.
	Clusters *envs.Clusters
	// Agents are the node list's own dependencies, so an agent reads here as it does
	// in the list: its certificate, clock, update state and the manual update need.
	Agents AgentsDeps
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EnvWarningBody is one environment problem in the header bell.
type EnvWarningBody struct {
	ID            string `json:"id" enum:"env_offline,cert_not_renewing,manual_update_needed,clock_skew,pending_long,cluster_changed,fold_offered,restart_loop" doc:"Which problem this is"`
	Severity      string `json:"severity" enum:"error,warning" doc:"error: an environment is out of reach or a certificate has expired. warning: something needs attention"`
	EnvID         string `json:"envId,omitempty" doc:"The environment the problem is about. Absent for manual_update_needed when it spans more than one environment"`
	EnvName       string `json:"envName,omitempty" doc:"That environment's name, at most 64 characters. For fold_offered, the swarm environment"`
	Node          string `json:"node,omitempty" doc:"For cert_not_renewing and clock_skew: the node's host name, at most 64 characters"`
	AgentID       string `json:"agentId,omitempty" doc:"For cert_not_renewing and clock_skew: the agent"`
	Reason        string `json:"reason,omitempty" doc:"For env_offline and cluster_changed: the environment's reason code, as in its status"`
	Since         string `json:"since,omitempty" format:"date-time" doc:"When the problem began, where that is known. Items are shown newest first"`
	ExpiresAt     string `json:"expiresAt,omitempty" format:"date-time" doc:"For cert_not_renewing: when the certificate expires"`
	Days          int    `json:"days,omitempty" doc:"For pending_long: whole days since the environment was added"`
	Count         int    `json:"count,omitempty" doc:"For manual_update_needed: how many agents need an update by hand. For restart_loop: how many tasks of the service failed inside the window"`
	OffsetSeconds int    `json:"offsetSeconds,omitempty" doc:"For clock_skew: how far the agent's clock is ahead of the server's, in seconds; negative is behind"`

	Service       string `json:"service,omitempty" doc:"For restart_loop: the service's name, at most 64 characters"`
	ServiceID     string `json:"serviceId,omitempty" doc:"For restart_loop: the service's ID"`
	WindowMinutes int    `json:"windowMinutes,omitempty" doc:"For restart_loop: the window the count is over, in minutes"`
	LatestError   string `json:"latestError,omitempty" doc:"For restart_loop: the error text of the newest failed task, at most 300 characters; absent when Docker gave none"`
}

// EnvWarningsBody lists the environment warnings that apply now.
type EnvWarningsBody struct {
	Items []EnvWarningBody `json:"items" doc:"Only the warnings that apply, errors first, then newest first; empty when there are none"`
}

// EnvWarningsOutput wraps EnvWarningsBody for huma.
type EnvWarningsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvWarningsBody
}

// envWarningBody turns a derived warning into its API shape.
func envWarningBody(w envs.EnvWarning) EnvWarningBody {
	return EnvWarningBody{
		ID: w.ID, Severity: w.Severity, EnvID: w.EnvID, EnvName: w.EnvName, Node: w.Node, AgentID: w.AgentID, Reason: w.Reason,
		Since: rfc3339(w.Since), ExpiresAt: rfc3339(w.ExpiresAt), Days: w.Days, Count: w.Count,
		OffsetSeconds: envs.ClockOffsetSeconds(w.Offset),
		Service:       w.Service, ServiceID: w.ServiceID, WindowMinutes: w.WindowMinutes, LatestError: w.LatestError,
	}
}

// RegisterEnvWarnings registers GET /api/environments/warnings: the environment
// problems the header bell lists (D-37, D-39). It is a route of its own, open to anyone
// signed in and filtered per environment, so a user who can read an environment but not
// the settings still sees that environment's problems; the settings-gated
// /api/warnings keeps its permission.
func RegisterEnvWarnings(r *httpx.Router, d EnvWarningsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listEnvironmentWarnings",
		Method:      http.MethodGet,
		Path:        "/api/environments/warnings",
		Summary:     "List the environment problems that need attention",
		Description: "Derived from live state, never stored, so an item is there exactly while its cause is and cannot be dismissed: an " +
			"environment that is offline (env_offline, with since and reason), a certificate that is not renewing or has expired " +
			"(cert_not_renewing), agents that need an update by hand (manual_update_needed, one item with a count), a clock that is out " +
			"of step (clock_skew), an environment still waiting for its first agent after 7 days (pending_long), a swarm that was " +
			"re-initialised (cluster_changed) and a local host that can be folded into a swarm environment it manages (fold_offered, " +
			"only for callers who hold environments.manage) and a swarm service whose tasks keep failing (restart_loop, one item per service that is not muted, " +
			"only for callers who hold services.read on that environment; with service, serviceId, count, windowMinutes, latestError and since). Only environments the caller holds environments.read for are considered, " +
			"so the list never reveals what the caller cannot open, and the counts are over that set. A revoked environment says nothing here. " +
			"Errors come first, then the newest. Names are at most 64 characters.",
		Tags: []string{"environments"},
	}, rbac.SelfService, func(ctx context.Context, _ *struct{}) (*EnvWarningsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		global, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
		if err != nil {
			d.Log.Error("environment warnings failed", "step", "effective permissions", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		all, err := d.Envs.List(ctx)
		if err != nil {
			d.Log.Error("environment warnings failed", "step", "list environments", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		canFold := slices.Contains(global, rbac.PermEnvironmentsManage)
		at := now()
		visible := make([]envs.Environment, 0, len(all))
		states := map[string][]envs.AgentState{}
		for _, e := range all {
			if !canRead(perEnv, e.ID) {
				continue
			}
			// Reviewing a fold is the manager's business: for anyone else the item
			// would point at a card they cannot use.
			e.FoldOffered = e.FoldOffered && canFold
			visible = append(visible, e)
			if e.Kind != envs.KindAgent || d.Agents.Agents == nil {
				continue
			}
			nodes, err := d.Agents.Agents.ListNodes(ctx, e.ID)
			if err != nil {
				d.Log.Error("environment warnings failed", "step", "list agents", "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			bodies := agentBodies(nodes, d.Agents.Live, e.ID, "", at, d.Agents.updates())
			stateOf := make(map[string]AgentBody, len(bodies))
			for _, b := range bodies {
				stateOf[b.ID] = b
			}
			for _, n := range nodes {
				b := stateOf[n.ID]
				states[e.ID] = append(states[e.ID], envs.AgentState{
					ID: n.ID, Node: n.Name, Revoked: n.Status == "revoked", CertNotAfter: n.CertNotAfter,
					ClockOffset:  time.Duration(b.ClockOffsetSeconds) * time.Second,
					UpdateFailed: b.UpdateState.State == "failed",
					ManualUpdate: b.ManualUpdateCommand != "",
				})
			}
		}
		warnings := envs.Warnings(visible, states, at)
		warnings = append(warnings, restartLoopWarnings(d, perEnv, visible)...)
		envs.SortWarnings(warnings)
		items := make([]EnvWarningBody, 0, len(warnings))
		for _, w := range warnings {
			items = append(items, envWarningBody(w))
		}
		return &EnvWarningsOutput{CacheControl: "no-store", Body: EnvWarningsBody{Items: items}}, nil
	})
}

// canReadServices reports whether the grants listed for each environment include reading its
// services.
func canReadServices(perEnv map[string][]rbac.Perm, id string) bool {
	return slices.Contains(perEnv[id], rbac.PermServicesRead) ||
		slices.Contains(perEnv[rbac.AllEnvironments], rbac.PermServicesRead)
}

// restartLoopWarnings are the restart-loop items of the swarms the caller may read services on,
// from the cluster reader's last read of each: nothing is read from Docker for the bell, and a
// loop that stopped is gone with the next read.
func restartLoopWarnings(d EnvWarningsDeps, perEnv map[string][]rbac.Perm, visible []envs.Environment) []envs.EnvWarning {
	if d.Clusters == nil {
		return nil
	}
	var out []envs.EnvWarning
	for _, e := range visible {
		if !canReadServices(perEnv, e.ID) {
			continue
		}
		if st, ok := d.Clusters.Cached(e.ID); ok && len(st.RestartLoops) > 0 {
			out = append(out, envs.RestartLoopWarnings(e, st.RestartLoops)...)
		}
	}
	return out
}
