// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// Muting the alerts of a service in a restart loop, and the threshold that makes a loop
// (plan 06-35, RES-13, D-14, D-16). Detection itself is 06-34; nothing here touches Docker.

const (
	problemFailuresInvalid = "failures_invalid"
	problemWindowInvalid   = "window_invalid"

	// The bounds of the per-environment setting, as the migration's CHECKs have them.
	minRestartLoopFailures, maxRestartLoopFailures = 2, 50
	minRestartLoopWindow, maxRestartLoopWindow     = 1, 120

	restartLoopTargetService   = "service"
	restartLoopTargetContainer = "container"
	restartLoopAuditTargetEnv  = "environment"
)

// RestartLoopDeps are the dependencies of the restart-loop mute and settings routes.
type RestartLoopDeps struct {
	Store *store.Store
	// Clusters is the cluster reader a mute is shown in at once, and whose last read says
	// which services exist and what Docker's history limit is.
	Clusters *envs.Clusters
	// Aggregator finds the container a container mute names, on its node (06-36).
	Aggregator *envs.Aggregator
	Audit      *audit.Writer
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// RestartLoopMuteInput names the service whose alerts are muted or unmuted.
type RestartLoopMuteInput struct {
	EnvironmentPathInput
	ServiceID string `path:"serviceId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The service ID or name: the id of a row of the services list"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RestartLoopMuteBody is the answer to a mute or an unmute.
type RestartLoopMuteBody struct {
	ServiceID string `json:"serviceId" doc:"The service's ID"`
	Muted     bool   `json:"muted" doc:"Whether the service's restart-loop alerts are muted now"`
	Changed   bool   `json:"changed" doc:"False when the service was in that state already, so nothing was written"`
}

// RestartLoopMuteOutput wraps RestartLoopMuteBody for huma.
type RestartLoopMuteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RestartLoopMuteBody
}

// ContainerRestartLoopMuteInput names the container whose alerts are muted or unmuted, and the
// node it is on.
type ContainerRestartLoopMuteInput struct {
	EnvironmentPathInput
	ContainerID string `path:"containerId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The container ID or name"`
	NodeID      string `query:"nodeId" maxLength:"128" doc:"The node the container is on: the nodeId of its row in the containers list. May be left out on an environment of one node; on a swarm it is required (422 node_required)"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// ContainerRestartLoopMuteBody is the answer to a mute or an unmute of a container.
type ContainerRestartLoopMuteBody struct {
	ContainerID string `json:"containerId" doc:"The container's ID as the last read holds it; the name that was asked when the container is gone"`
	Name        string `json:"name" doc:"The container's name: with the node, what the mute is keyed by, so a container recreated under the same name stays muted"`
	NodeID      string `json:"nodeId" doc:"The node the container is on"`
	Muted       bool   `json:"muted" doc:"Whether the container's restart-loop alerts are muted now"`
	Changed     bool   `json:"changed" doc:"False when the container was in that state already, so nothing was written"`
}

// ContainerRestartLoopMuteOutput wraps ContainerRestartLoopMuteBody for huma.
type ContainerRestartLoopMuteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerRestartLoopMuteBody
}

// RestartLoopSettingsBody is the threshold of one environment.
type RestartLoopSettingsBody struct {
	Failures      int `json:"failures" doc:"How many failed tasks within the window make a restart loop, 2 to 50"`
	WindowMinutes int `json:"windowMinutes" doc:"The window the failures are counted in, in minutes, 1 to 120"`
	HistoryLimit  int `json:"historyLimit" doc:"How many tasks per slot Docker keeps (its task history limit, R): Docker shows R-1 failed tasks per slot. 0 when the environment has not been read yet or is not a swarm"`
}

// RestartLoopSettingsOutput wraps RestartLoopSettingsBody for huma.
type RestartLoopSettingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RestartLoopSettingsBody
}

// RestartLoopSettingsInput sets the threshold of an environment.
type RestartLoopSettingsInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Failures      int `json:"failures" doc:"How many failed tasks within the window make a restart loop: 2 to 50, else 422 failures_invalid"`
		WindowMinutes int `json:"windowMinutes" doc:"The window in minutes: 1 to 120, else 422 window_invalid"`
	}
}

// RegisterRestartLoop registers the mute and settings routes.
func RegisterRestartLoop(r *httpx.Router, d RestartLoopDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	httpx.Register(r, huma.Operation{
		OperationID: "muteServiceRestartLoop", Method: http.MethodPut, Path: "/api/env/{envId}/services/{serviceId}/restart-loop-mute",
		Summary: "Mute the restart-loop alerts of a service",
		Description: "The service keeps being flagged as in a restart loop, but raises no health bell item and is not counted on the dashboard. " +
			"The mute is kept until it is removed, and goes away by itself when the service is removed. Muting a service that is muted already changes and records nothing and answers 200 with changed false. " +
			"404 service_not_found for a service the last cluster read does not hold; 409 not_a_swarm for a standalone host. " +
			"Needs services.control (Operator) on the environment. Audited as restart_loop.muted.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *RestartLoopMuteInput) (*RestartLoopMuteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setServiceRestartLoopMute(ctx, d, p, env.ID, in, true)
			if err != nil {
				return nil, err
			}
			return &RestartLoopMuteOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "unmuteServiceRestartLoop", Method: http.MethodDelete, Path: "/api/env/{envId}/services/{serviceId}/restart-loop-mute",
		Summary: "Unmute the restart-loop alerts of a service",
		Description: "Removes the mute. Unmuting a service that is not muted changes and records nothing and answers 200 with changed false. " +
			"404 service_not_found for a service that is neither held by the last cluster read nor muted; 409 not_a_swarm for a standalone host. " +
			"Needs services.control (Operator) on the environment. Audited as restart_loop.unmuted.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *RestartLoopMuteInput) (*RestartLoopMuteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setServiceRestartLoopMute(ctx, d, p, env.ID, in, false)
			if err != nil {
				return nil, err
			}
			return &RestartLoopMuteOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "muteContainerRestartLoop", Method: http.MethodPut, Path: "/api/env/{envId}/containers/{containerId}/restart-loop-mute",
		Summary: "Mute the restart-loop alerts of a standalone container",
		Description: "The container keeps being flagged as in a restart loop, but raises no health bell item. " +
			"The mute is keyed by the node and the container's name, so a container recreated under the same name stays muted, and it is kept until it is removed. " +
			"Muting a container that is muted already changes and records nothing and answers 200 with changed false. " +
			"The container lives on one node, named by nodeId, with the same rules and problems as the container detail route (422 node_required, 404 container_not_found, 503 node_unavailable). " +
			"Needs containers.control (Operator) on the environment. Audited as restart_loop.muted, with kind container.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermContainersControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerRestartLoopMuteInput) (*ContainerRestartLoopMuteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setContainerRestartLoopMute(ctx, d, p, env.ID, in, true)
			if err != nil {
				return nil, err
			}
			return &ContainerRestartLoopMuteOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "unmuteContainerRestartLoop", Method: http.MethodDelete, Path: "/api/env/{envId}/containers/{containerId}/restart-loop-mute",
		Summary: "Unmute the restart-loop alerts of a standalone container",
		Description: "Removes the mute. Unmuting a container that is not muted changes and records nothing and answers 200 with changed false. " +
			"A container that is gone can be unmuted by its name. Same node rules and problems as the mute. " +
			"Needs containers.control (Operator) on the environment. Audited as restart_loop.unmuted, with kind container.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermContainersControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerRestartLoopMuteInput) (*ContainerRestartLoopMuteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setContainerRestartLoopMute(ctx, d, p, env.ID, in, false)
			if err != nil {
				return nil, err
			}
			return &ContainerRestartLoopMuteOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getRestartLoopSettings", Method: http.MethodGet, Path: "/api/env/{envId}/restart-loop-settings",
		Summary: "The restart-loop threshold of an environment",
		Description: "How many failed tasks within how many minutes make a restart loop here (default 3 in 10), and Docker's task history limit as of the last cluster read, " +
			"which says how many failed tasks Docker itself keeps per slot (the limit less one). Needs environments.configure (Env Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsConfigure, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*RestartLoopSettingsOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getRestartLoopSettings(ctx, d, env.ID)
			if err != nil {
				return nil, err
			}
			return &RestartLoopSettingsOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "setRestartLoopSettings", Method: http.MethodPut, Path: "/api/env/{envId}/restart-loop-settings",
		Summary: "Set the restart-loop threshold of an environment",
		Description: "failures is 2 to 50 (422 failures_invalid) and windowMinutes 1 to 120 (422 window_invalid), both checked before anything is written. " +
			"The next cluster read uses them. A save that changes nothing answers 200 and records nothing. " +
			"Needs environments.configure (Env Admin) on the environment. A change is audited as environment.restart_loop_settings_changed, with the new and the previous values.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsConfigure, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *RestartLoopSettingsInput) (*RestartLoopSettingsOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setRestartLoopSettings(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &RestartLoopSettingsOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// findLoopService finds the service the path names, by ID or by name, in the last cluster
// read. The answer is false when that read does not hold it.
func findLoopService(ctx context.Context, d RestartLoopDeps, envID, idOrName string) (dockerx.ServiceRow, bool, error) {
	swarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return dockerx.ServiceRow{}, false, snapshotProblem(d.Log, envID, err)
	}
	if !swarm {
		return dockerx.ServiceRow{}, false, httpx.Problem(http.StatusConflict, problemNotASwarm, "This environment is a standalone Docker host, not a swarm.")
	}
	st, err := d.Clusters.State(ctx, envID)
	if err != nil && st.FetchedAt.IsZero() {
		return dockerx.ServiceRow{}, false, serviceReadProblem(ServicesDeps{Log: d.Log}, envID, err)
	}
	for _, r := range st.Read.Services {
		if r.ID == idOrName {
			return r, true, nil
		}
	}
	for _, r := range st.Read.Services {
		if r.Name == idOrName {
			return r, true, nil
		}
	}
	return dockerx.ServiceRow{}, false, nil
}

// setServiceRestartLoopMute mutes or unmutes one service. The store row and its audit row are
// one transaction, and a request that changes nothing writes neither.
func setServiceRestartLoopMute(ctx context.Context, d RestartLoopDeps, p *rbac.Principal, envID string, in *RestartLoopMuteInput, mute bool) (RestartLoopMuteBody, error) {
	row, found, err := findLoopService(ctx, d, envID, in.ServiceID)
	if err != nil {
		return RestartLoopMuteBody{}, err
	}
	id, name := in.ServiceID, ""
	if found {
		id, name = row.ID, row.Name
	}

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return RestartLoopMuteBody{}, restartLoopInternal(d, "begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	mutes, err := q.ListRestartLoopMutes(ctx, envID)
	if err != nil {
		return RestartLoopMuteBody{}, restartLoopInternal(d, "list the mutes", envID, err)
	}
	existing := -1
	for i, m := range mutes {
		if m.Kind == restartLoopTargetService && m.TargetID == id {
			existing = i
		}
	}
	// A service the last read does not hold can still be unmuted when its mute is there: the
	// mute row of a removed service lives until the next successful read prunes it.
	if !found && (mute || existing < 0) {
		return RestartLoopMuteBody{}, httpx.Problem(http.StatusNotFound, problemServiceNotFound, "This service does not exist. It may have been removed.")
	}
	if !found {
		name = mutes[existing].Name
	}
	body := RestartLoopMuteBody{ServiceID: id, Muted: mute}
	if mute == (existing >= 0) {
		return body, nil // already so: nothing to write or record
	}
	action := audit.ActionRestartLoopUnmuted
	if mute {
		action = audit.ActionRestartLoopMuted
		if err := q.InsertRestartLoopMute(ctx, sqlc.InsertRestartLoopMuteParams{
			EnvID: envID, Kind: restartLoopTargetService, TargetID: id, Name: name, MutedBy: p.ID, MutedAt: d.Now().UTC().UnixNano(),
		}); err != nil {
			return RestartLoopMuteBody{}, restartLoopInternal(d, "mute", envID, err)
		}
	} else if _, err := q.DeleteRestartLoopMute(ctx, sqlc.DeleteRestartLoopMuteParams{EnvID: envID, Kind: restartLoopTargetService, TargetID: id}); err != nil {
		return RestartLoopMuteBody{}, restartLoopInternal(d, "unmute", envID, err)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: restartLoopTargetService, TargetID: id, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"name": name, "kind": restartLoopTargetService},
	}); err != nil {
		return RestartLoopMuteBody{}, restartLoopInternal(d, "audit", envID, err)
	}
	if err := tx.Commit(); err != nil {
		return RestartLoopMuteBody{}, restartLoopInternal(d, "commit", envID, err)
	}
	d.Clusters.SetRestartLoopMute(envID, id, mute)
	body.Changed = true
	return body, nil
}

// findLoopContainer finds the container the path names, by name, full ID or an ID prefix, among
// the containers of one node in the last snapshot. The answer is false when it holds none.
func findLoopContainer(rows []envs.ContainerRow, nodeID, idOrName string) (envs.ContainerRow, bool) {
	var prefix *envs.ContainerRow
	for i, r := range rows {
		if r.NodeID != nodeID {
			continue
		}
		if r.ID == idOrName || r.Name == idOrName {
			return r, true
		}
		if prefix == nil && len(idOrName) >= minContainerIDPrefix && strings.HasPrefix(r.ID, idOrName) {
			prefix = &rows[i]
		}
	}
	if prefix != nil {
		return *prefix, true
	}
	return envs.ContainerRow{}, false
}

// minContainerIDPrefix is the shortest ID prefix that names a container, as Docker's own 12
// character short ID does.
const minContainerIDPrefix = 12

// setContainerRestartLoopMute mutes or unmutes one standalone container, keyed by its node and
// name. The store row and its audit row are one transaction, and a request that changes
// nothing writes neither.
func setContainerRestartLoopMute(ctx context.Context, d RestartLoopDeps, p *rbac.Principal, envID string, in *ContainerRestartLoopMuteInput, mute bool) (ContainerRestartLoopMuteBody, error) {
	agg, nodeID, node, err := resolveContainerNode(ctx, d.Aggregator, d.Log, envID, in.NodeID)
	if err != nil {
		return ContainerRestartLoopMuteBody{}, err
	}
	if node.ID == "" {
		return ContainerRestartLoopMuteBody{}, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Coaming.")
	}
	id, name := in.ContainerID, in.ContainerID
	row, found := findLoopContainer(agg.Containers, nodeID, in.ContainerID)
	if found {
		id, name = row.ID, row.Name
	}
	key := envs.ContainerLoopKey(nodeID, name)

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	mutes, err := q.ListRestartLoopMutes(ctx, envID)
	if err != nil {
		return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "list the mutes", envID, err)
	}
	existing := slices.ContainsFunc(mutes, func(m sqlc.RestartLoopMute) bool { return m.Kind == restartLoopTargetContainer && m.TargetID == key })
	// A container the last snapshot does not hold can still be unmuted by its name when its
	// mute is there: a mute outlives its container, which may come back under that name.
	if !found && (mute || !existing) {
		return ContainerRestartLoopMuteBody{}, httpx.Problem(http.StatusNotFound, problemContainerNotFound, "This container does not exist on that node. It may have been removed.")
	}
	body := ContainerRestartLoopMuteBody{ContainerID: id, Name: name, NodeID: nodeID, Muted: mute}
	if mute == existing {
		return body, nil // already so: nothing to write or record
	}
	action := audit.ActionRestartLoopUnmuted
	if mute {
		action = audit.ActionRestartLoopMuted
		if err := q.InsertRestartLoopMute(ctx, sqlc.InsertRestartLoopMuteParams{
			EnvID: envID, Kind: restartLoopTargetContainer, TargetID: key, Name: name, MutedBy: p.ID, MutedAt: d.Now().UTC().UnixNano(),
		}); err != nil {
			return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "mute", envID, err)
		}
	} else if _, err := q.DeleteRestartLoopMute(ctx, sqlc.DeleteRestartLoopMuteParams{EnvID: envID, Kind: restartLoopTargetContainer, TargetID: key}); err != nil {
		return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "unmute", envID, err)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: restartLoopTargetContainer, TargetID: key, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"name": name, "kind": restartLoopTargetContainer, "node": nodeID},
	}); err != nil {
		return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "audit", envID, err)
	}
	if err := tx.Commit(); err != nil {
		return ContainerRestartLoopMuteBody{}, restartLoopInternal(d, "commit", envID, err)
	}
	d.Clusters.SetContainerRestartLoopMute(envID, key, mute)
	body.Changed = true
	return body, nil
}

// ContainerRestartLoopBody says a standalone container is in a restart loop (RES-13, D-15).
type ContainerRestartLoopBody struct {
	State         string `json:"state" enum:"flagged,muted" doc:"flagged while the loop holds; muted when the loop holds and a user muted its alerts: the container raises no bell item"`
	Count         int    `json:"count" doc:"How many times the container died with a non-zero exit code inside the window"`
	WindowMinutes int    `json:"windowMinutes" doc:"The window the deaths are counted in, in minutes: the environment's setting"`
	LastExitCode  string `json:"lastExitCode" doc:"The exit code of its newest death, at most 12 characters"`
}

// containerRestartLoopBody turns a flagged loop into its answer.
func containerRestartLoopBody(l envs.ContainerLoop) *ContainerRestartLoopBody {
	state := "flagged"
	if l.Muted {
		state = "muted"
	}
	return &ContainerRestartLoopBody{
		State: state, Count: l.Count, WindowMinutes: int(l.Window / time.Minute), LastExitCode: truncateRunes(l.LastExitCode, maxExitCodeLen),
	}
}

// maxExitCodeLen is the longest exit code the API sends. A real one is a small integer.
const maxExitCodeLen = 12

// containerLoopsOf are the container loops and mutes of an environment's last cluster read.
// Nothing is read from Docker for a list or a page. Both are nil before the first read or
// without a cluster reader.
func containerLoopsOf(c *envs.Clusters, envID string) (loops map[string]envs.ContainerLoop, muted map[string]bool) {
	if c == nil {
		return nil, nil
	}
	st, ok := c.Cached(envID)
	if !ok {
		return nil, nil
	}
	return st.ContainerLoops, st.MutedContainers
}

// getRestartLoopSettings answers the environment's threshold and Docker's history limit.
func getRestartLoopSettings(ctx context.Context, d RestartLoopDeps, envID string) (RestartLoopSettingsBody, error) {
	s, err := d.Store.RQ.GetRestartLoopSettings(ctx, envID)
	if err != nil {
		return RestartLoopSettingsBody{}, restartLoopSettingsError(d, envID, err)
	}
	st, _ := d.Clusters.Cached(envID)
	return RestartLoopSettingsBody{
		Failures: int(s.RestartLoopFailures), WindowMinutes: int(s.RestartLoopWindowMinutes), HistoryLimit: st.HistoryLimit,
	}, nil
}

// setRestartLoopSettings validates and stores the threshold, and audits a change with the
// values it replaced. A save that changes nothing writes nothing.
func setRestartLoopSettings(ctx context.Context, d RestartLoopDeps, p *rbac.Principal, envID string, in *RestartLoopSettingsInput) (RestartLoopSettingsBody, error) {
	failures, window := in.Body.Failures, in.Body.WindowMinutes
	if failures < minRestartLoopFailures || failures > maxRestartLoopFailures {
		return RestartLoopSettingsBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemFailuresInvalid,
			fmt.Sprintf("Failed tasks must be between %d and %d.", minRestartLoopFailures, maxRestartLoopFailures))
	}
	if window < minRestartLoopWindow || window > maxRestartLoopWindow {
		return RestartLoopSettingsBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemWindowInvalid,
			fmt.Sprintf("The window must be between %d and %d minutes.", minRestartLoopWindow, maxRestartLoopWindow))
	}

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return RestartLoopSettingsBody{}, restartLoopInternal(d, "begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	old, err := q.GetRestartLoopSettings(ctx, envID)
	if err != nil {
		return RestartLoopSettingsBody{}, restartLoopSettingsError(d, envID, err)
	}
	if int(old.RestartLoopFailures) != failures || int(old.RestartLoopWindowMinutes) != window {
		if _, err := q.SetRestartLoopSettings(ctx, sqlc.SetRestartLoopSettingsParams{
			RestartLoopFailures: int64(failures), RestartLoopWindowMinutes: int64(window), UpdatedAt: d.Now().UTC().UnixNano(), ID: envID,
		}); err != nil {
			return RestartLoopSettingsBody{}, restartLoopInternal(d, "save the settings", envID, err)
		}
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionEnvironmentRestartLoopSettingsChanged, TargetType: restartLoopAuditTargetEnv, TargetID: envID, EnvID: envID,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"failures": strconv.Itoa(failures), "window_minutes": strconv.Itoa(window),
				"previous_failures": strconv.FormatInt(old.RestartLoopFailures, 10), "previous_window_minutes": strconv.FormatInt(old.RestartLoopWindowMinutes, 10),
			},
		}); err != nil {
			return RestartLoopSettingsBody{}, restartLoopInternal(d, "audit", envID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RestartLoopSettingsBody{}, restartLoopInternal(d, "commit", envID, err)
	}
	st, _ := d.Clusters.Cached(envID)
	return RestartLoopSettingsBody{Failures: failures, WindowMinutes: window, HistoryLimit: st.HistoryLimit}, nil
}

// restartLoopSettingsError maps a failed read of the settings: an environment that is not
// there is the environment's 404, anything else is logged and answered as an internal error.
func restartLoopSettingsError(d RestartLoopDeps, envID string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Problem(http.StatusNotFound, "environment_not_found", "This environment does not exist.")
	}
	return restartLoopInternal(d, "read the settings", envID, err)
}

// restartLoopInternal logs a server-side failure and answers an internal error.
func restartLoopInternal(d RestartLoopDeps, what, envID string, err error) error {
	d.Log.Error("restart-loop request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}
