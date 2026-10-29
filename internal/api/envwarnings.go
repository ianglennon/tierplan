// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// EnvWarningsDeps are the dependencies of the environment warnings route.
type EnvWarningsDeps struct {
	Envs *envs.Service
	// Checker says which environments the caller may read.
	Checker *rbac.Checker
	// Clusters is the cluster reader whose last read says which services are in a restart loop
	// (RES-13). Nil lists none.
	Clusters *envs.Clusters
	// Repo holds the git stacks' sync states, which the bell lists the errors of. Nil lists none.
	Repo *stackops.Repo
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
	ID            string `json:"id" enum:"env_offline,cert_not_renewing,manual_update_needed,clock_skew,pending_long,cluster_changed,fold_offered,restart_loop,restart_loop_container,git_sync_error,git_autodeploy_skipped,git_autodeploy_reverted,git_autodeploy_paused" doc:"Which problem this is"`
	Severity      string `json:"severity" enum:"error,warning" doc:"error: an environment is out of reach or a certificate has expired. warning: something needs attention"`
	EnvID         string `json:"envId,omitempty" doc:"The environment the problem is about. Absent for manual_update_needed when it spans more than one environment"`
	EnvName       string `json:"envName,omitempty" doc:"That environment's name, at most 64 characters. For fold_offered, the swarm environment"`
	Node          string `json:"node,omitempty" doc:"For cert_not_renewing and clock_skew: the node's host name, at most 64 characters"`
	AgentID       string `json:"agentId,omitempty" doc:"For cert_not_renewing and clock_skew: the agent"`
	Reason        string `json:"reason,omitempty" doc:"For env_offline and cluster_changed: the environment's reason code, as in its status"`
	Since         string `json:"since,omitempty" format:"date-time" doc:"When the problem began, where that is known. Items are shown newest first"`
	ExpiresAt     string `json:"expiresAt,omitempty" format:"date-time" doc:"For cert_not_renewing: when the certificate expires"`
	Days          int    `json:"days,omitempty" doc:"For pending_long: whole days since the environment was added"`
	Count         int    `json:"count,omitempty" doc:"For manual_update_needed: how many agents need an update by hand. For restart_loop: how many tasks of the service failed inside the window. For restart_loop_container: how many times the container died inside the window"`
	OffsetSeconds int    `json:"offsetSeconds,omitempty" doc:"For clock_skew: how far the agent's clock is ahead of the server's, in seconds; negative is behind"`

	Service       string `json:"service,omitempty" doc:"For restart_loop: the service's name, at most 64 characters"`
	ServiceID     string `json:"serviceId,omitempty" doc:"For restart_loop: the service's ID"`
	WindowMinutes int    `json:"windowMinutes,omitempty" doc:"For restart_loop and restart_loop_container: the window the count is over, in minutes"`
	LatestError   string `json:"latestError,omitempty" doc:"For restart_loop: the error text of the newest failed task, at most 300 characters; absent when Docker gave none"`

	Container    string `json:"container,omitempty" doc:"For restart_loop_container: the container's name, at most 64 characters"`
	ContainerID  string `json:"containerId,omitempty" doc:"For restart_loop_container: the container's ID as the last read holds it"`
	NodeID       string `json:"nodeId,omitempty" doc:"For restart_loop_container: the node the container is on, as the container routes take it"`
	LastExitCode string `json:"lastExitCode,omitempty" doc:"For restart_loop_container: the exit code of the container's newest death"`

	StackID      string `json:"stackId,omitempty" doc:"For git_sync_error: the stack's ID"`
	StackName    string `json:"stackName,omitempty" doc:"For git_sync_error: the stack's name, at most 64 characters"`
	ReasonCode   string `json:"reasonCode,omitempty" doc:"For git_sync_error: why the sync check failed, one of the reason codes of the stack's git route (fetch_failed, host_key_changed, host_key_untrusted, ref_gone, folder_gone, checks_failed, auto_deploy_blocked, auto_deploy_failed)"`
	ReasonDetail string `json:"reasonDetail,omitempty" doc:"For git_sync_error: a sentence on the reason, at most 300 characters; it carries nothing from the server. For git_autodeploy_skipped: the problem that held the commit back. For git_autodeploy_paused: the sentence the pause left, when it left one"`

	Commit   string `json:"commit,omitempty" doc:"For git_autodeploy_skipped: the short commit that is waiting for a fix or a manual review"`
	Example  string `json:"example,omitempty" doc:"For git_autodeploy_reverted: the first change that was put back, as a line such as 'web replicas 5 → 3', at most 300 characters; count is how many were put back"`
	Revision int64  `json:"revision,omitempty" doc:"For git_autodeploy_reverted: the number of the revision that put the changes back. For git_autodeploy_paused with reasonCode rollback: the revision rolled back to"`
	Owner    string `json:"owner,omitempty" doc:"For git_autodeploy_paused: the name of the user the automation runs for, at most 64 characters; absent when it has no owner any more"`
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
		Container: w.Container, ContainerID: w.ContainerID, NodeID: w.NodeID, LastExitCode: w.LastExitCode,
		StackID: w.StackID, StackName: w.StackName, ReasonCode: w.ReasonCode, ReasonDetail: w.ReasonDetail,
		Commit: w.Commit, Example: w.Example, Revision: w.Revision, Owner: w.Owner,
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
			"only for callers who hold services.read on that environment; with service, serviceId, count, windowMinutes, latestError and since) and a standalone container that keeps dying (restart_loop_container, one item per container that is not muted, " +
			"only for callers who hold containers.read on that environment; with container, containerId, nodeId, count, windowMinutes, lastExitCode and since) and a git stack whose sync check is in error (git_sync_error, one item per stack, " +
			"only for callers who hold git.manage on that environment; with stackId, stackName, reasonCode, reasonDetail and since; it goes when the stack is in sync again) and the automatic deploys of git stacks: a commit that was held back (git_autodeploy_skipped, with commit and reasonDetail; it goes when a deploy ends the wait), a deploy that put changes made outside the stack back " +
			"(git_autodeploy_reverted, with count, example and revision; it goes with the next deploy of the stack) and an automation that is paused (git_autodeploy_paused, with reasonCode owner or rollback, revision and owner; it goes when it is resumed or turned off). " +
			"These three go to the user the automation runs for and to callers who hold git.manage on the environment, and to no one else. Only environments the caller holds environments.read for are considered, " +
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
		warnings = append(warnings, containerLoopWarnings(d, perEnv, visible)...)
		gitWarnings, err := gitSyncWarnings(ctx, d, perEnv, visible)
		if err != nil {
			d.Log.Error("environment warnings failed", "step", "git sync errors", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		warnings = append(warnings, gitWarnings...)
		autoWarnings, err := gitAutoDeployWarnings(ctx, d, p, perEnv, visible)
		if err != nil {
			d.Log.Error("environment warnings failed", "step", "automatic deploys", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		warnings = append(warnings, autoWarnings...)
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

// canReadContainers reports whether the grants listed for each environment include reading its
// containers.
func canReadContainers(perEnv map[string][]rbac.Perm, id string) bool {
	return slices.Contains(perEnv[id], rbac.PermContainersRead) ||
		slices.Contains(perEnv[rbac.AllEnvironments], rbac.PermContainersRead)
}

// containerLoopWarnings are the container-loop items of the environments the caller may read
// containers on, from the cluster reader's last read of each: nothing is read from Docker for
// the bell.
func containerLoopWarnings(d EnvWarningsDeps, perEnv map[string][]rbac.Perm, visible []envs.Environment) []envs.EnvWarning {
	if d.Clusters == nil {
		return nil
	}
	var out []envs.EnvWarning
	for _, e := range visible {
		if !canReadContainers(perEnv, e.ID) {
			continue
		}
		if st, ok := d.Clusters.Cached(e.ID); ok && len(st.ContainerLoops) > 0 {
			out = append(out, envs.ContainerLoopWarnings(e, st.ContainerLoops)...)
		}
	}
	return out
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

// gitSyncWarnings are the sync errors of the git stacks of the environments the caller manages git on: nothing is read
// from a repository for the bell, only the states the checks stored. An environment whose stacks cannot be read fails the
// whole answer, as the other reads of the bell do.
func gitSyncWarnings(ctx context.Context, d EnvWarningsDeps, perEnv map[string][]rbac.Perm, visible []envs.Environment) ([]envs.EnvWarning, error) {
	if d.Repo == nil || d.Repo.Store == nil {
		return nil, nil
	}
	var out []envs.EnvWarning
	for _, e := range visible {
		if !canIn(perEnv, e.ID, rbac.PermGitManage) {
			continue
		}
		states, err := d.Repo.GitStates(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		var failing []envs.GitSyncError
		for id, st := range states {
			if st.Status != stackops.GitStatusError {
				continue
			}
			failing = append(failing, envs.GitSyncError{StackID: id, ReasonCode: st.ReasonCode, ReasonDetail: st.ReasonDetail, Since: errorSince(st)})
		}
		if len(failing) == 0 {
			continue
		}
		stacks, err := d.Repo.Stacks(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		names := make(map[string]string, len(stacks))
		for _, s := range stacks {
			names[s.ID] = s.Name
		}
		for i := range failing {
			failing[i].StackName = names[failing[i].StackID]
		}
		out = append(out, envs.GitSyncWarnings(e, failing)...)
	}
	return out, nil
}

// errorSince is when a stack's sync went into error, the time of its last check if the state does not say.
func errorSince(st stackops.GitState) time.Time {
	switch {
	case st.ErrorSince != nil:
		return *st.ErrorSince
	case st.CheckedAt != nil:
		return *st.CheckedAt
	}
	return time.Time{}
}

// gitAutoDeployWarnings are the notices of the automatic deploys of the git stacks of the environments the caller may read:
// a commit held back, a deploy that put changes made outside the stack back and an automation that is paused. Each is derived
// from what the checks and deploys stored, so it is there while its cause is. A notice is for the user the automation runs
// for and for the environment's Admins (git.manage) and for no one else (T-09-100); an automation with no owner (a roll back
// forgets it) is told to the Admins only.
func gitAutoDeployWarnings(ctx context.Context, d EnvWarningsDeps, p *rbac.Principal, perEnv map[string][]rbac.Perm, visible []envs.Environment) ([]envs.EnvWarning, error) {
	if d.Repo == nil || d.Repo.Store == nil {
		return nil, nil
	}
	var out []envs.EnvWarning
	for _, e := range visible {
		admin := canIn(perEnv, e.ID, rbac.PermGitManage)
		sgs, err := d.Repo.StackGitsOfEnv(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if len(sgs) == 0 {
			continue
		}
		states, err := d.Repo.GitStates(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		var names map[string]string
		var notices []envs.GitAutoDeployNotice
		for _, sg := range sgs {
			st := states[sg.StackID]
			wantSkipped := st.EventKind == stackops.GitEventSkipped
			wantReverted := st.EventKind == stackops.GitEventReverted
			wantPaused := sg.AutoDeployPausedReason != ""
			if !wantSkipped && !wantReverted && !wantPaused {
				continue
			}
			ownerID, err := autoDeployOwner(ctx, d.Repo, sg.StackID)
			if err != nil {
				return nil, err
			}
			if !admin && (ownerID == "" || p == nil || p.ID != ownerID) {
				continue
			}
			if names == nil {
				all, err := d.Repo.Stacks(ctx, e.ID)
				if err != nil {
					return nil, err
				}
				names = make(map[string]string, len(all))
				for _, s := range all {
					names[s.ID] = s.Name
				}
			}
			base := envs.GitAutoDeployNotice{StackID: sg.StackID, StackName: names[sg.StackID]}
			if wantSkipped {
				n := base
				n.Kind, n.Since, n.Detail, n.Commit = envs.WarnGitAutoDeploySkipped, eventAt(st), st.EventDetail, shortCommit(st.PendingCommit)
				notices = append(notices, n)
			}
			if wantReverted {
				n, ok, err := revertedNotice(ctx, d.Repo, base, st)
				if err != nil {
					return nil, err
				}
				if ok {
					notices = append(notices, n)
				}
			}
			if wantPaused {
				n := base
				n.Kind, n.Reason, n.Revision = envs.WarnGitAutoDeployPaused, sg.AutoDeployPausedReason, sg.AutoDeployPausedSeq
				n.Since = sg.UpdatedAt
				if st.EventKind == stackops.GitEventPaused {
					n.Since, n.Detail = eventAt(st), st.EventDetail
				}
				if ownerID != "" {
					n.Owner = ownerName(ctx, d.Repo, ownerID)
				}
				notices = append(notices, n)
			}
		}
		out = append(out, envs.GitAutoDeployWarnings(e, notices)...)
	}
	return out, nil
}

// revertedNotice is the notice of the deploy a stack's state names: how many lines it put back and the first, read from the
// revision it made. A revision that is gone, or one that recorded none, has no notice.
func revertedNotice(ctx context.Context, repo *stackops.Repo, base envs.GitAutoDeployNotice, st stackops.GitState) (envs.GitAutoDeployNotice, bool, error) {
	seq, err := strconv.ParseInt(st.EventDetail, 10, 64)
	if err != nil {
		return base, false, nil
	}
	lines, err := repo.RevertedDrift(ctx, base.StackID, seq)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(lines) == 0) {
		return base, false, nil
	}
	if err != nil {
		return base, false, err
	}
	base.Kind, base.Since, base.Revision, base.Count, base.Example = envs.WarnGitAutoDeployReverted, eventAt(st), seq, len(lines), lines[0]
	return base, true, nil
}

// autoDeployOwner is the user a stack's automatic deploys run for, empty when nobody owns them.
func autoDeployOwner(ctx context.Context, repo *stackops.Repo, stackID string) (string, error) {
	row, err := repo.Store.RQ.GetOwnedResource(ctx, sqlc.GetOwnedResourceParams{Kind: gitsync.KindGitAutoDeploy, ResourceID: stackID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return row.OwnerID, nil
}

// ownerName is how a user is called in the bell: the display name, else the username; empty for a user who is gone.
func ownerName(ctx context.Context, repo *stackops.Repo, userID string) string {
	u, err := repo.Store.RQ.GetUserByID(ctx, userID)
	if err != nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// eventAt is when a stack's state noted its event.
func eventAt(st stackops.GitState) time.Time {
	if st.EventAt != nil {
		return *st.EventAt
	}
	return time.Time{}
}

// shortCommit is the first seven characters of a commit, as the stack pages show it.
func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
