// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

const (
	serviceTargetType = "service"

	problemNotASwarm       = "not_a_swarm"
	problemReplicasInvalid = "replicas_invalid"
	problemDuplicateTarget = "duplicate_target"
	problemTooManyTargets  = "too_many_targets"
	problemStatusInvalid   = "status_invalid"
	problemQueryTooLong    = "query_too_long"

	// The codes of one target of a scale, beside the ones containers share.
	codeNotScalable        = "not_scalable"
	codeServiceChanged     = "service_changed"
	codeServiceNotFound    = "service_not_found"
	codeAgentHostPolicy    = "agent_host_policy"
	codeNoManagerConnected = problemNoManager
	codeNoQuorum           = "no_quorum"
	codeRegistryUnresolved = "registry_unresolved"
	codeScaleFailed        = "scale_failed"
	codeUnchanged          = "unchanged"

	// maxServiceQuery is the longest search the services list takes, in characters.
	maxServiceQuery = 128

	// The values of the services list's status filter (UI-SPEC "Services list" chips).
	serviceStatusDegraded = "degraded"
	serviceStatusUpdating = "updating"
	serviceStatusProblem  = "problem"
	serviceStatusLoop     = "loop"

	// maxLoopError is the longest latest-error text a restart loop carries, in characters.
	// Docker's task error is text from the engine and the image; the page clamps it for the
	// eye and this keeps a runaway one off the wire.
	maxLoopError = 512

	// maxReplicas is the most replicas a scale may ask for.
	maxReplicas = 1000
	// maxScaleTargets is the most services one scale request may name.
	maxScaleTargets = 100
	// scaleManager is the "node" a scale target is run against: a service belongs to the
	// cluster, so every target goes to the one manager the environment writes through.
	scaleManager = "manager"
)

// ServicesDeps are the dependencies of the swarm service routes.
type ServicesDeps struct {
	// Aggregator reaches the manager a write goes to: WriteOnManager, the one write path
	// of a cluster-scoped change.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the lists are served from.
	Clusters *envs.Clusters
	// Resolver chooses the registry credential a service update is sent with.
	Resolver *registry.Resolver
	Audit    *audit.Writer
	Checker  *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// ServiceItem is one swarm service as the list shows it.
type ServiceItem struct {
	ID          string `json:"id" doc:"The service ID"`
	Name        string `json:"name" doc:"The service name"`
	Mode        string `json:"mode" enum:"replicated,global,replicated-job,global-job" doc:"How the swarm runs it"`
	Running     uint64 `json:"running" doc:"Tasks that are running now"`
	Desired     uint64 `json:"desired" doc:"Tasks the swarm wants running. For a global service this is the count of nodes it runs on; for a job, 0"`
	UpdateState string `json:"updateState" doc:"The state of the service's last update or rollback (updating, paused, completed, rollback_started, rollback_paused, rollback_completed); empty when it has none"`
	Image       string `json:"image" doc:"The image the service's spec names, as written; empty when it has none"`
	Stack       string `json:"stack" doc:"The stack the service belongs to (com.docker.stack.namespace); empty for a service made on its own"`
	Agent       bool   `json:"agent" doc:"True for a Bayplan agent service"`
	UpdatedAt   string `json:"updatedAt" format:"date-time" doc:"When the service's spec last changed"`

	Ports           []string `json:"ports" doc:"The ports the service publishes, as published->target/protocol (8080->80/tcp), with (host) after a port published in host mode; sorted by published port. Empty when it publishes none"`
	UpdateMessage   string   `json:"updateMessage" doc:"The swarm's message about the last update; empty when it has none"`
	HasPreviousSpec bool     `json:"hasPreviousSpec" doc:"True when the swarm still holds the spec before the last update, so a rollback has something to go back to"`
	CreatedAt       string   `json:"createdAt" format:"date-time" doc:"When the service was created"`

	RestartLoop *RestartLoopBody `json:"restartLoop,omitempty" doc:"Present while the service is in a restart loop: its tasks keep failing. Absent otherwise"`
}

// RestartLoopBody says a service is in a restart loop (RES-13, D-14, D-15).
type RestartLoopBody struct {
	State         string `json:"state" enum:"flagged" doc:"flagged while the loop holds"`
	Count         int    `json:"count" doc:"How many tasks failed or were rejected inside the window, as of the last read"`
	WindowMinutes int    `json:"windowMinutes" doc:"The window the count is over, in minutes: the environment's restart-loop setting"`
	LatestError   string `json:"latestError" doc:"The error text of the newest failed task; empty when Docker gave none"`
	Since         string `json:"since" format:"date-time" doc:"When the service was flagged"`
}

// restartLoopBody turns a flagged loop into its answer. The error is cut to maxLoopError
// characters.
func restartLoopBody(l envs.RestartLoop) *RestartLoopBody {
	return &RestartLoopBody{
		State: "flagged", Count: l.Count, WindowMinutes: int(l.Window / time.Minute),
		LatestError: truncateRunes(l.LatestError, maxLoopError), Since: rfc3339(l.Since),
	}
}

// truncateRunes keeps the first n characters of s.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ServiceStatusCountsBody is how many services each status chip of the list counts. A
// service that is both degraded and updating is in both.
type ServiceStatusCountsBody struct {
	Degraded int `json:"degraded" doc:"Services with some but not all of their tasks running"`
	Updating int `json:"updating" doc:"Services whose update or rollback is in progress (updating, rollback_started)"`
	Problem  int `json:"problem" doc:"Services whose update or rollback stopped or went wrong (paused, rollback_paused, rollback_completed)"`
	Loop     int `json:"loop" doc:"Services in a restart loop: their tasks keep failing (RES-13)"`
}

// ServiceListBody is the answer of the services list.
type ServiceListBody struct {
	Items           []ServiceItem           `json:"items" doc:"At most 500 rows that match the search and the status filter, sorted by name then ID"`
	Total           int                     `json:"total" doc:"How many services match the search and the status filter, whatever the cap"`
	UnfilteredTotal int                     `json:"unfilteredTotal" doc:"How many services there are before the search and the status filter, so a page can say 3 of 600"`
	Capped          bool                    `json:"capped" doc:"True when total is more than the rows returned"`
	StatusCounts    ServiceStatusCountsBody `json:"statusCounts" doc:"How many services each status chip counts, before the search and the status filter, so the numbers never shrink with what was typed"`
	NoManager       bool                    `json:"noManager" doc:"True when no manager of the swarm is connected: the rows are the last ones read, or none"`
	FetchedAt       string                  `json:"fetchedAt" format:"date-time" doc:"When the rows were read from a manager; empty when none were"`
}

// ServicesListInput is the path of the services list with its search and status filter.
type ServicesListInput struct {
	EnvironmentPathInput
	Q      string `query:"q" doc:"Search: a case-insensitive substring of the service name, image, stack or any label key or value, at most 128 characters (else 422 query_too_long). Applied before the 500-row cap"`
	Status string `query:"status" maxLength:"64" doc:"Only services in one of these statuses, comma separated: degraded (some but not all tasks running), updating (updating, rollback_started), problem (paused, rollback_paused, rollback_completed) or loop (in a restart loop). Applied before the 500-row cap; any other value is refused 422 status_invalid"`
}

// ServiceListOutput wraps ServiceListBody for huma.
type ServiceListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceListBody
}

// ServiceScaleTarget names one service and the replica count to give it.
type ServiceScaleTarget struct {
	ServiceID string `json:"serviceId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The service ID or name: the id of a row of the services list"`
	Replicas  int64  `json:"replicas" doc:"The replica count to set, a whole number from 0 to 1000 (else 422 replicas_invalid)"`
}

// ServiceScaleRequest is the body of POST /api/env/{envId}/services/scale.
type ServiceScaleRequest struct {
	BulkID  string               `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request. Send a UUID to name the bulk yourself; with more than one target and none sent, the server makes one (a UUIDv7) and answers it"`
	Targets []ServiceScaleTarget `json:"targets" minItems:"1" doc:"The services to scale, at most 100 (else 422 too_many_targets), each named once (else 422 duplicate_target). Each is answered separately; one that fails never stops the others"`
}

// ServiceScaleInput is the path and body of the scale route.
type ServiceScaleInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceScaleRequest
}

// ServiceScaleResult is the answer for one target.
type ServiceScaleResult struct {
	ServiceID string `json:"serviceId" doc:"The service ID or name the request named"`
	Name      string `json:"name" doc:"The service name when the manager could be asked; otherwise what the request named"`
	Result    string `json:"result" enum:"done,failed,skipped" doc:"done: the service has the replica count asked for (changed says whether an update was sent). failed: it could not be scaled; code says why. skipped: it has no replica count to set (not_scalable)"`
	Code      string `json:"code,omitempty" enum:"not_scalable,service_changed,service_not_found,host_read_only,agent_policy_denied,agent_host_policy,no_manager_connected,no_quorum,registry_unresolved,scale_failed" doc:"Why a target failed or was skipped. not_scalable (with skipped): a global or job service has no replica count. service_changed: somebody else changed the service after it was read, so nothing was changed and nothing is retried; ask again. service_not_found: the swarm has no such service. host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the manager's agent, so nothing was sent. agent_policy_denied: the manager's agent refused the call by its own policy, which is what an agent older than this server does for a call it does not know. agent_host_policy: the service's spec gives tasks host access (a bind of the Docker socket or of a host path outside /srv, /mnt and /media, an added capability, a lowered sandbox, the host network) and the manager's agent refuses that unless its owner set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true; every update carries the whole spec, so a service that already has host access cannot be scaled either. no_manager_connected: no manager of the swarm is connected, or the one reached is not a manager. no_quorum: the swarm has lost its quorum. registry_unresolved: the registry credential chosen for the image could not be read, so nothing was sent. scale_failed: the manager refused it; detail has its message"`
	Detail    string `json:"detail,omitempty" doc:"The manager's message for scale_failed, at most 300 characters; for the other codes a plain sentence"`
	Previous  *int64 `json:"previous,omitempty" doc:"The replica count the service had; absent when it was not read"`
	Replicas  int64  `json:"replicas" doc:"The replica count that was asked for"`
	Changed   bool   `json:"changed" doc:"False when the service already had that count: it was answered done and no update was sent"`
}

// ServiceScaleResponse is the per-target outcome, in request order.
type ServiceScaleResponse struct {
	BulkID  string               `json:"bulkId,omitempty" doc:"The bulk ID recorded in the audit rows of this request: the one sent, or the one the server made for a request with more than one target. Absent for a single target with none sent"`
	Results []ServiceScaleResult `json:"results" doc:"One entry per requested target, in request order"`
}

// ServiceScaleOutput wraps ServiceScaleResponse for huma.
type ServiceScaleOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceScaleResponse
}

// RegisterServices mounts the swarm service routes.
func RegisterServices(r *httpx.Router, d ServicesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listServices", Method: http.MethodGet, Path: "/api/env/{envId}/services",
		Summary: "List the swarm services of an environment",
		Description: "The services of a swarm environment with their mode, running and desired task counts, image, stack and last update, " +
			"read through a manager's agent or through the local socket of a swarm manager, at most 500 rows sorted by name then ID. " +
			"Optionally a search (q) and a status filter (status: degraded, updating, problem, loop). Both run on the server before the 500-row cap, so at most 500 rows come back after them, with total the number that matched, unfilteredTotal the number before them and statusCounts the chip numbers, counted before them too. " +
			"Each row has its published ports, the update message, whether a rollback has a spec to go back to and its creation time. " +
			"409 not_a_swarm when the environment is a standalone host. When no manager is connected the answer is 200 with noManager true and the rows last read, or none. " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServicesListInput) (*ServiceListOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := listServices(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceListOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "scaleServices", Method: http.MethodPost, Path: "/api/env/{envId}/services/scale",
		Summary: "Scale swarm services",
		Description: "Sets the replica count of one or more replicated services, once each and never retried. " +
			"Each target is read from the manager, and the update is sent with the version just read, so a service somebody else changed in between is service_changed and nothing is changed. " +
			"Only the replica count of the spec changes. The registry credential for the service's image is sent with the update. " +
			"A service that already has the count is answered done with changed false and nothing is sent. A global or job service is skipped with not_scalable. " +
			"replicas outside 0 to 1000 is 422 replicas_invalid, a service named twice is 422 duplicate_target, and more than 100 targets is 422 too_many_targets, all before anything is sent. 409 not_a_swarm for a standalone host. " +
			"Needs services.control (Operator) on the environment. Every attempt on a service that exists is audited as service.scaled, with the new and the previous count.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceScaleInput) (*ServiceScaleOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := scaleServices(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceScaleOutput{CacheControl: "no-store", Body: body}, nil
		})

	registerServiceDetail(r, d)
	registerServiceTasks(r, d)
	registerPlacement(r, d)
	registerServiceRollout(r, d)
	registerServiceActions(r, d)
	registerServiceEdit(r, d)
	registerServiceCreate(r, d)
}

// requireSwarm answers 409 not_a_swarm for an environment that is a standalone host.
func requireSwarm(ctx context.Context, d ServicesDeps, envID string) error {
	if d.Clusters == nil {
		d.Log.Error("the services routes have no cluster reader")
		return huma.Error500InternalServerError("internal error")
	}
	swarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return snapshotProblem(d.Log, envID, err)
	}
	if !swarm {
		return httpx.Problem(http.StatusConflict, problemNotASwarm, "This environment is a standalone Docker host, not a swarm.")
	}
	return nil
}

// serviceStatuses are the values status= takes.
var serviceStatuses = []string{serviceStatusDegraded, serviceStatusUpdating, serviceStatusProblem, serviceStatusLoop}

// parseServiceStatuses reads the comma list of status=. An empty list means no filter and
// an unknown value is a 422.
func parseServiceStatuses(raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !slices.Contains(serviceStatuses, part) {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStatusInvalid,
				"status "+strconv.Quote(part)+" is not one of "+strings.Join(serviceStatuses, ", ")+".")
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out, nil
}

// serviceHasStatus says whether a row counts under one status value. Degraded is some
// tasks running and not all of them: a service with none running is not running, not
// degraded, and a job (which wants 0) never is. Loop is a service the restart-loop tracker
// flagged, whatever its state.
func serviceHasStatus(row dockerx.ServiceRow, status string, loops map[string]envs.RestartLoop) bool {
	switch status {
	case serviceStatusDegraded:
		return row.Running > 0 && row.Running < row.Desired
	case serviceStatusUpdating:
		return row.UpdateState == "updating" || row.UpdateState == "rollback_started"
	case serviceStatusProblem:
		return row.UpdateState == "paused" || row.UpdateState == "rollback_paused" || row.UpdateState == "rollback_completed"
	case serviceStatusLoop:
		_, flagged := loops[row.ID]
		return flagged
	}
	return false
}

// serviceStatusCounts counts the rows by status. It is called on every row, before the
// search and the status filter, so the numbers on the chips never shrink with what the
// caller typed.
func serviceStatusCounts(rows []dockerx.ServiceRow, loops map[string]envs.RestartLoop) ServiceStatusCountsBody {
	var c ServiceStatusCountsBody
	for _, r := range rows {
		if serviceHasStatus(r, serviceStatusDegraded, loops) {
			c.Degraded++
		}
		if serviceHasStatus(r, serviceStatusUpdating, loops) {
			c.Updating++
		}
		if serviceHasStatus(r, serviceStatusProblem, loops) {
			c.Problem++
		}
		if serviceHasStatus(r, serviceStatusLoop, loops) {
			c.Loop++
		}
	}
	return c
}

// serviceMatchesQuery says whether q (already lower-cased) occurs in the name, the image,
// the stack or any label key or value of the row. The match is a case-insensitive
// substring. An empty q matches every row. The labels are read here and go no further
// (T-06-13).
func serviceMatchesQuery(row dockerx.ServiceRow, q string) bool {
	if q == "" {
		return true
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	if has(row.Name) || has(row.Image) || has(row.Stack) {
		return true
	}
	for k, v := range row.Labels {
		if has(k) || has(v) {
			return true
		}
	}
	return false
}

// filterServices keeps the rows that match q and any of statuses, in their order. It runs
// before the row cap, so the cap applies to what the caller asked for.
func filterServices(rows []dockerx.ServiceRow, q string, statuses []string, loops map[string]envs.RestartLoop) []dockerx.ServiceRow {
	if q == "" && len(statuses) == 0 {
		return rows
	}
	out := make([]dockerx.ServiceRow, 0, len(rows))
	for _, r := range rows {
		if !serviceMatchesQuery(r, q) {
			continue
		}
		if len(statuses) > 0 && !slices.ContainsFunc(statuses, func(s string) bool { return serviceHasStatus(r, s, loops) }) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// servicePortStrings writes a row's ports as the list shows them: 8080->80/tcp, with
// (host) after a port published in host mode.
func servicePortStrings(ports []dockerx.ServicePort) []string {
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		text := strconv.FormatUint(uint64(p.Published), 10) + "->" + strconv.FormatUint(uint64(p.Target), 10) + "/" + p.Protocol
		if p.Mode == "host" {
			text += " (host)"
		}
		out = append(out, text)
	}
	return out
}

// listServices answers the services of a swarm from the cluster reader.
func listServices(ctx context.Context, d ServicesDeps, envID string, in *ServicesListInput) (ServiceListBody, error) {
	if utf8.RuneCountInString(in.Q) > maxServiceQuery {
		return ServiceListBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemQueryTooLong,
			"The search can be at most "+strconv.Itoa(maxServiceQuery)+" characters.")
	}
	statuses, err := parseServiceStatuses(in.Status)
	if err != nil {
		return ServiceListBody{}, err
	}
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceListBody{}, err
	}
	st, err := d.Clusters.State(ctx, envID)
	noManager := false
	if err != nil {
		switch {
		case errors.Is(err, envs.ErrNoManager), errors.Is(err, envs.ErrOffline), envs.ReasonFor(err) == envs.ReasonNotAManager:
			noManager = true
		case st.FetchedAt.IsZero():
			// Nothing was ever read and the read failed for another reason.
			return ServiceListBody{}, snapshotProblem(d.Log, envID, err)
		}
		// Otherwise the last read is served, with its age in fetchedAt.
	}
	all := st.Read.Services
	loops := st.RestartLoops
	rows := filterServices(all, strings.ToLower(strings.TrimSpace(in.Q)), statuses, loops)
	shown := rows[:min(len(rows), maxListRows)]
	body := ServiceListBody{
		Items: make([]ServiceItem, 0, len(shown)), Total: len(rows), UnfilteredTotal: len(all), Capped: len(rows) > maxListRows,
		StatusCounts: serviceStatusCounts(all, loops), NoManager: noManager, FetchedAt: rfc3339(st.FetchedAt),
	}
	for _, row := range shown {
		item := ServiceItem{
			ID: row.ID, Name: row.Name, Mode: row.Mode, Running: row.Running, Desired: row.Desired,
			UpdateState: row.UpdateState, Image: row.Image, Stack: row.Stack, Agent: row.Agent, UpdatedAt: rfc3339(row.UpdatedAt),
			Ports: servicePortStrings(row.Ports), UpdateMessage: row.UpdateMessage, HasPreviousSpec: row.HasPreviousSpec,
			CreatedAt: rfc3339(row.CreatedAt),
		}
		if l, flagged := loops[row.ID]; flagged {
			item.RestartLoop = restartLoopBody(l)
		}
		body.Items = append(body.Items, item)
	}
	return body, nil
}

// scaleOutcome is what one target's write learned, beyond the ContainerActionResult the
// shared runner keeps: the previous count, whether an update was sent and the service's ID.
type scaleOutcome struct {
	id       string
	previous int64
	known    bool
	changed  bool
}

// scaleServices checks the request, scales each target through the manager once and audits
// each attempt.
func scaleServices(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceScaleInput) (ServiceScaleResponse, error) {
	body := in.Body
	if len(body.Targets) > maxScaleTargets {
		return ServiceScaleResponse{}, httpx.Problem(http.StatusUnprocessableEntity, problemTooManyTargets,
			"Scale at most "+strconv.Itoa(maxScaleTargets)+" services in one request.")
	}
	seen := make(map[string]bool, len(body.Targets))
	for _, t := range body.Targets {
		if t.Replicas < 0 || t.Replicas > maxReplicas {
			return ServiceScaleResponse{}, httpx.Problem(http.StatusUnprocessableEntity, problemReplicasInvalid,
				"Enter a whole number of replicas from 0 to "+strconv.Itoa(maxReplicas)+".")
		}
		if seen[t.ServiceID] {
			return ServiceScaleResponse{}, httpx.Problem(http.StatusUnprocessableEntity, problemDuplicateTarget,
				"Each service can be named once in a request.")
		}
		seen[t.ServiceID] = true
	}
	bulkID, err := bulkIDFor(body.BulkID, len(body.Targets))
	if err != nil {
		d.Log.Error("could not make a bulk ID", "error", err)
		return ServiceScaleResponse{}, huma.Error500InternalServerError("internal error")
	}
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceScaleResponse{}, err
	}

	want := make(map[string]int64, len(body.Targets))
	targets := make([]ContainerActionTarget, len(body.Targets))
	for i, t := range body.Targets {
		want[t.ServiceID] = t.Replicas
		targets[i] = ContainerActionTarget{NodeID: scaleManager, ID: t.ServiceID}
	}
	var mu sync.Mutex
	outcomes := make(map[string]*scaleOutcome, len(targets))
	raw := runTargets(ctx, targets, func(ctx context.Context, t ContainerActionTarget) ContainerActionResult {
		out := &scaleOutcome{}
		res := scaleOne(ctx, d, envID, t, want[t.ID], out)
		mu.Lock()
		outcomes[t.ID] = out
		mu.Unlock()
		return res
	})

	resp := ServiceScaleResponse{BulkID: bulkID, Results: make([]ServiceScaleResult, len(raw))}
	changed := false
	for i, r := range raw {
		out := outcomes[r.ID]
		if out == nil {
			out = &scaleOutcome{} // the request ended before this target was reached
		}
		res := ServiceScaleResult{
			ServiceID: r.ID, Name: r.Name, Result: r.Result, Code: r.Code, Detail: r.Detail,
			Replicas: want[r.ID], Changed: out.changed,
		}
		if out.known {
			prev := out.previous
			res.Previous = &prev
		}
		resp.Results[i] = res
		changed = changed || res.Changed
		auditScale(ctx, d, p, envID, in.UserAgent, bulkID, out, res)
	}
	if changed {
		// The list is served from the cluster read; read it again so the next list shows
		// the counts that were just set. A failed read leaves the last one.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		_ = d.Clusters.Refresh(rctx, envID)
		cancel()
	}
	return resp, nil
}

// scaleOne reads one service from the manager, resolves the credential for its image and
// sets its replica count, all in one write call. It never returns an error: a failure is
// the result. The write is sent once and seen through, so a browser that goes away while
// it runs does not turn a scale that happened into a failure.
func scaleOne(ctx context.Context, d ServicesDeps, envID string, t ContainerActionTarget, replicas int64, out *scaleOutcome) ContainerActionResult {
	res := ContainerActionResult{NodeID: t.NodeID, ID: t.ID, Name: t.ID}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var name string
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		detail, _, err := c.ServiceInspect(writeCtx, t.ID)
		if err != nil {
			return err
		}
		out.id, name = detail.ID, detail.Name
		if detail.Mode != "replicated" {
			return dockerx.ErrNotScalable
		}
		auth, err := registryAuthFor(writeCtx, d, envID, detail.Image)
		if err != nil {
			return err
		}
		previous, changed, err := c.ServiceScale(writeCtx, detail.ID, uint64(replicas), auth) //nolint:gosec // G115: replicas was checked to be 0 to 1000
		if err != nil {
			return err
		}
		out.previous, out.known, out.changed = int64(previous), true, changed //nolint:gosec // G115: a replica count
		return nil
	})
	if name != "" {
		res.Name = name
	}
	if err == nil {
		res.Result = actionDone
		return res
	}
	code, detail := scaleError(err)
	res.Result, res.Code, res.Detail = actionFailed, code, detail
	if code == codeNotScalable {
		res.Result = actionSkipped
	}
	if code == codeScaleFailed || code == codeRegistryUnresolved {
		d.Log.Warn("scaling a service failed", "environment", envID, "service", t.ID, "code", code, "error", err)
	}
	return res
}

// errRegistryUnresolved is the error of a credential that was chosen for an image and could
// not be read. Its text holds no value.
var errRegistryUnresolved = errors.New("the registry credential could not be read")

// registryAuthFor is the X-Registry-Auth header for the image of a service. A service whose
// image is not a reference the resolver can read goes without a header, as the engine
// resolves it itself; a credential that was chosen and cannot be read fails the target,
// because sending the update anonymously could change which image the service runs.
func registryAuthFor(ctx context.Context, d ServicesDeps, envID, image string) (string, error) {
	if d.Resolver == nil || image == "" {
		return "", nil
	}
	ref, err := registry.ParseRef(image)
	if err != nil {
		return "", nil //nolint:nilerr // not a reference: no credential applies
	}
	header, _, err := d.Resolver.AuthFor(ctx, envID, ref)
	if err != nil {
		// The error names the credential's ID and holds no value.
		d.Log.Error("could not resolve the registry credential of a service update", "environment", envID, "ref", ref.String(), "error", err)
		return "", errRegistryUnresolved
	}
	return header, nil
}

// swarmWriteCode is the one place that maps the error of a swarm write to the code every
// swarm write answers with, so the services, nodes, secrets and configs routes word a
// refusal the same way. It returns "" for an error that is none of these: the caller then
// has a code of its own, or a generic failure.
//
//   - host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the manager's agent.
//   - agent_host_policy: the agent refused a spec that gives tasks host access, because its
//     owner did not set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES (D-36).
//   - agent_policy_denied: the agent refused the call by its own policy.
//   - no_manager_connected: no manager is connected, or the one reached is not a manager.
//   - service_changed, service_not_found: the service moved or went away under the write.
func swarmWriteCode(err error) string {
	switch denied := (*envs.AgentPolicyDeniedError)(nil); {
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly
	case errors.As(envs.ClassifyDockerError(err), &denied):
		if denied.HostPolicy {
			return codeAgentHostPolicy
		}
		return codeAgentPolicyDeny
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNoManager), errors.Is(err, envs.ErrOffline),
		errors.Is(err, envs.ErrNotFound), errors.Is(err, tunnel.ErrAgentGone), envs.ReasonFor(err) == envs.ReasonNotAManager:
		return codeNoManagerConnected
	case errors.Is(err, dockerx.ErrServiceChanged):
		return codeServiceChanged
	case errors.Is(err, dockerx.ErrServiceNotFound):
		return codeServiceNotFound
	}
	return ""
}

// swarmWriteDetail is the sentence of a code swarmWriteCode returned.
var swarmWriteDetail = map[string]string{
	codeHostReadOnly:       "This host is read-only: its owner set BAYPLAN_AGENT_READ_ONLY on the agent, so Bayplan cannot change anything on it.",
	codeAgentHostPolicy:    "This swarm's manager agents don't let Bayplan change services with host access. Set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true on them to allow it.",
	codeAgentPolicyDeny:    "The manager's agent refused the request.",
	codeNoManagerConnected: "No manager of the swarm is connected.",
	codeServiceChanged:     "This service was changed by someone else after it was read. Nothing was changed; try again.",
	codeServiceNotFound:    "The service no longer exists.",
}

// scaleError maps the error of one scale to a result code and a sentence.
func scaleError(err error) (code, detail string) {
	switch {
	case errors.Is(err, dockerx.ErrNotScalable):
		return codeNotScalable, "A global or job service has no replica count to set."
	case errors.Is(err, errRegistryUnresolved):
		return codeRegistryUnresolved, "The registry credential for this service's image could not be read, so nothing was sent."
	}
	if code := swarmWriteCode(err); code != "" {
		return code, swarmWriteDetail[code]
	}
	if envs.ReasonFor(err) == envs.ReasonNoQuorum {
		return codeNoQuorum, "The swarm has lost its quorum."
	}
	return codeScaleFailed, clipDetail(err.Error())
}

// auditScale writes the service.scaled row of one target, except for a service the swarm
// does not have. It is written on a context that outlives the request: the scale has
// already happened or been refused.
func auditScale(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, userAgent, bulkID string, out *scaleOutcome, res ServiceScaleResult) {
	if res.Code == codeServiceNotFound {
		return
	}
	result, reason, code := audit.ResultSuccess, "", actionDone
	switch {
	case res.Result != actionDone:
		result, reason, code = audit.ResultFailure, res.Code, res.Code
	case !res.Changed:
		code = codeUnchanged
	}
	details := map[string]string{"name": res.Name, "replicas": strconv.FormatInt(res.Replicas, 10), "code": code}
	if res.Previous != nil {
		details["previous"] = strconv.FormatInt(*res.Previous, 10)
	}
	if bulkID != "" {
		details["bulk"] = bulkID
	}
	id := res.ServiceID
	if out.id != "" {
		id = out.id
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionServiceScaled, TargetType: serviceTargetType, TargetID: id,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		// The scale has been made or refused and the caller must still be told which; the
		// lost row is the server's fault and is logged.
		d.Log.Error("could not audit a service scale", "service", id, "error", err)
	}
}
