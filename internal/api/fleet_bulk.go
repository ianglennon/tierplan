// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Updating and skipping many workloads across the fleet in one action (plan 12-30, ENV-10, IMG-06, D-10, D-11, D-14, D-15). The
// request names the workloads and the environment of each; the server judges every one of them itself, for the caller and as it is
// now, and a workload that fails any test is named in the answer and does not stop the others. One job runs for each environment.

const (
	// bulkMaxItems is the most workloads one request may name (T-12-91).
	bulkMaxItems = 500

	// The reasons an item of a bulk request was not taken.
	bulkNoPermission   = updates.BulkNoPermission
	bulkNotSelectable  = "not_selectable"
	bulkOffline        = "offline"
	bulkLocked         = updates.BulkLocked
	bulkMoved          = updates.BulkMoved
	bulkNotUpdatable   = updates.BulkNotUpdatable
	bulkTooManyJobs    = "too_many_jobs"
	bulkReasonsDocText = "no_permission: the caller may not do this to that workload there (or may not see the environment). not_selectable: it is not an update that is available now (skipped, ignored, rolled back, up to date, running, gone). offline: the environment could not be read. locked: another operation holds the environment. moved: the newest version changed since it was confirmed. not_updatable: it is a kind of workload this does not update, with code saying why. too_many_jobs: the caller already has the most jobs running"
)

// FleetBulkItem names one workload and the environment it is in.
type FleetBulkItem struct {
	EnvID  string            `json:"envId" minLength:"1" maxLength:"128" doc:"The environment's ID"`
	Kind   string            `json:"kind" enum:"service,container" doc:"service for a swarm service, container for a standalone container"`
	Name   string            `json:"name" minLength:"1" maxLength:"255" doc:"The service's or container's name"`
	Expect map[string]string `json:"expect,omitempty" maxProperties:"100" doc:"For each service name (the compose service name for a container of a stack; the container's name otherwise), the digest the confirm showed. The server compares it with what the registry says now and skips the item as moved when they differ; it never deploys a digest from the request"`
}

// FleetBulkRunRequest is the body of POST /api/fleet/updates/run.
type FleetBulkRunRequest struct {
	Items []FleetBulkItem `json:"items" minItems:"1" maxItems:"500" doc:"The workloads to update, at most 500"`
}

// FleetBulkSkipItem names a workload without the digests, for skipping the version it is offered.
type FleetBulkSkipItem struct {
	EnvID string `json:"envId" minLength:"1" maxLength:"128" doc:"The environment's ID"`
	Kind  string `json:"kind" enum:"service,container" doc:"service for a swarm service, container for a standalone container"`
	Name  string `json:"name" minLength:"1" maxLength:"255" doc:"The service's or container's name"`
}

// FleetBulkSkipRequest is the body of POST /api/fleet/updates/skip.
type FleetBulkSkipRequest struct {
	Items []FleetBulkSkipItem `json:"items" minItems:"1" maxItems:"500" doc:"The workloads whose offered version is skipped, at most 500"`
}

// FleetBulkJob is the job that updates the workloads taken in one environment.
type FleetBulkJob struct {
	EnvID string `json:"envId" doc:"The environment's ID"`
	JobID string `json:"jobId" doc:"The job: follow it at GET /api/env/{envId}/update-jobs/{jobId} or its stream. Its items are the workloads, in the order they are updated, then a summary"`
}

// FleetBulkRefused is a workload that was not taken, and why.
type FleetBulkRefused struct {
	EnvID  string `json:"envId" doc:"The environment's ID"`
	Kind   string `json:"kind" enum:"service,container" doc:"service or container"`
	Name   string `json:"name" doc:"The workload's name"`
	Reason string `json:"reason" enum:"no_permission,not_selectable,offline,locked,moved,not_updatable,too_many_jobs" doc:"Why it was not taken"`
	Code   string `json:"code,omitempty" doc:"For not_updatable, why in TierPlan's words (agent, job_mode, managed_by_stack, pinned_in_file, compose_unmanaged, task_container, workload_gone and the like)"`
}

// FleetBulkRunBody is the answer of POST /api/fleet/updates/run.
type FleetBulkRunBody struct {
	BulkID  string             `json:"bulkId" doc:"The ID that ties every audit entry of the selection together (detail bulk)"`
	Jobs    []FleetBulkJob     `json:"jobs" doc:"One job for each environment that had workloads to update"`
	Skipped []FleetBulkRefused `json:"skipped" doc:"The workloads that were not taken, and why. Items the caller may not update in an environment they may not read are named no_permission, as an environment that does not exist is"`
}

// FleetBulkRunOutput wraps FleetBulkRunBody for huma.
type FleetBulkRunOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetBulkRunBody
}

// FleetBulkRunInput is the request of POST /api/fleet/updates/run.
type FleetBulkRunInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      FleetBulkRunRequest
}

// FleetBulkSkipBody is the answer of POST /api/fleet/updates/skip.
type FleetBulkSkipBody struct {
	BulkID  string             `json:"bulkId" doc:"The ID that ties every audit entry of the selection together (detail bulk)"`
	Skipped int                `json:"skipped" minimum:"0" doc:"How many versions are skipped now, those that were skipped already included"`
	Refused []FleetBulkRefused `json:"refused" doc:"The workloads whose version was not skipped, and why"`
}

// FleetBulkSkipOutput wraps FleetBulkSkipBody for huma.
type FleetBulkSkipOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetBulkSkipBody
}

// FleetBulkSkipInput is the request of POST /api/fleet/updates/skip.
type FleetBulkSkipInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      FleetBulkSkipRequest
}

// holdsEnvPerm reports whether the grants listed for each environment include perm on that environment.
func holdsEnvPerm(perEnv map[string][]rbac.Perm, id string, perm rbac.Perm) bool {
	return slices.Contains(perEnv[id], perm) || slices.Contains(perEnv[rbac.AllEnvironments], perm)
}

// bulkKey names a workload within an environment.
func bulkKey(kind, name string) string { return kind + "/" + strings.TrimPrefix(name, "/") }

// groupByEnv groups items by environment, in the order each environment first appears.
func groupByEnv[T any](items []T, envOf func(T) string) (order []string, groups map[string][]T) {
	groups = map[string][]T{}
	for _, it := range items {
		id := envOf(it)
		if _, seen := groups[id]; !seen {
			order = append(order, id)
		}
		groups[id] = append(groups[id], it)
	}
	return order, groups
}

// bulkEnvResult is what one environment's part of a request gave.
type bulkEnvResult struct {
	jobID   string
	skipped []FleetBulkRefused
}

// registerFleetBulk registers the fleet's bulk update and bulk skip routes.
func registerFleetBulk(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "runFleetUpdates", Method: http.MethodPost, Path: "/api/fleet/updates/run",
		Summary: "Update the selected workloads across environments",
		Description: "Updates the chosen services and containers to the newest version of their image, in one action: one job for each environment, which takes the environment's operation lock once and updates its workloads one at a time, so a workload whose update fails is put back by itself and the job goes on with the next. " +
			"Every workload is judged by the server, for the caller, as it is now: the caller needs services.control on its environment (and containers.control for a container, and stacks.deploy for a service of a stack TierPlan manages, which is updated as its own 'update images' revision of that stack inside the job), " +
			"and only a workload whose update is available now is taken (a skipped, ignored, rolled back, running or held back one is not_selectable, and a rolled back version is never retried here). " +
			"Each workload's newest version is asked of the registry again when its turn comes, and one that moved since expect is skipped as moved. A workload that is not taken is named in skipped and the others go on. " +
			"While a job waits, its workloads read queued, then updating. At most 500 workloads. One bulk ID ties together the audit entries image.updated, image.update_failed and image.update_rolled_back of every workload, and each job ends with one image.bulk_updated entry. " +
			"Open to every signed-in person; what they may update is judged for each workload. Reasons: " + bulkReasonsDocText + ".",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetBulkRunInput) (*FleetBulkRunOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		body, err := runFleetUpdates(ctx, d, p, in)
		if err != nil {
			return nil, err
		}
		return &FleetBulkRunOutput{CacheControl: "no-store", Body: body}, nil
	})
	httpx.Register(r, huma.Operation{
		OperationID: "skipFleetUpdates", Method: http.MethodPost, Path: "/api/fleet/updates/skip",
		Summary: "Skip the offered version of the selected workloads across environments",
		Description: "Hides the version of its image each chosen service or container is offered now, until the tag points at a newer one, as skipping one does, for many at once. " +
			"Needs updates.manage (an Environment Admin) on each environment: a workload in an environment where the caller lacks it is refused no_permission and the others are skipped. " +
			"A workload that has no update available now is refused not_selectable. One audit entry image.update_skipped for each workload, all with the same bulk ID in the detail bulk. At most 500 workloads. " +
			"Open to every signed-in person; what they may skip is judged for each workload. Reasons: " + bulkReasonsDocText + ".",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetBulkSkipInput) (*FleetBulkSkipOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		body, err := skipFleetUpdates(ctx, d, p, in)
		if err != nil {
			return nil, err
		}
		return &FleetBulkSkipOutput{CacheControl: "no-store", Body: body}, nil
	})
}

// bulkEnv resolves an environment of a request, as a person may know it: one that does not exist, or that the caller may not read,
// is the same answer as an environment the caller has no permission in.
func bulkEnv(ctx context.Context, d UpdatesDeps, perEnv map[string][]rbac.Perm, envID string) (*httpx.Env, bool, error) {
	if !holdsEnvPerm(perEnv, envID, rbac.PermEnvironmentsRead) || d.Stacks.Envs == nil {
		return nil, false, nil
	}
	env, err := d.Stacks.Envs.Resolve(ctx, envID)
	switch {
	case errors.Is(err, httpx.ErrEnvNotFound):
		return nil, false, nil
	case err != nil:
		return nil, true, err
	}
	return &env, true, nil
}

// bulkReadiness answers the problem to give when the bulk routes cannot run.
func (d UpdatesDeps) bulkReadiness() error {
	if d.Applier == nil || d.Updates == nil || d.Checker == nil {
		return httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	return nil
}

// runFleetUpdates judges the items of a request and starts one bulk job for each environment that has some left.
func runFleetUpdates(ctx context.Context, d UpdatesDeps, p *rbac.Principal, in *FleetBulkRunInput) (FleetBulkRunBody, error) {
	if err := d.bulkReadiness(); err != nil {
		return FleetBulkRunBody{}, err
	}
	bulkID, err := uuid.NewV7()
	if err != nil {
		return FleetBulkRunBody{}, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		return FleetBulkRunBody{}, snapshotProblem(d.log(), "", err)
	}
	order, groups := groupByEnv(in.Body.Items, func(it FleetBulkItem) string { return it.EnvID })

	results := make([]bulkEnvResult, len(order))
	var g errgroup.Group
	g.SetLimit(fleetParallel)
	for i, envID := range order {
		g.Go(func() error {
			results[i] = runFleetEnv(ctx, d, p, perEnv, in.UserAgent, bulkID.String(), envID, groups[envID])
			return nil
		})
	}
	_ = g.Wait()

	body := FleetBulkRunBody{BulkID: bulkID.String(), Jobs: []FleetBulkJob{}, Skipped: []FleetBulkRefused{}}
	for i, envID := range order {
		if results[i].jobID != "" {
			body.Jobs = append(body.Jobs, FleetBulkJob{EnvID: envID, JobID: results[i].jobID})
		}
		body.Skipped = append(body.Skipped, results[i].skipped...)
	}
	return body, nil
}

// runFleetEnv is runFleetUpdates for the items of one environment.
func runFleetEnv(ctx context.Context, d UpdatesDeps, p *rbac.Principal, perEnv map[string][]rbac.Perm, userAgent, bulkID, envID string, items []FleetBulkItem) bulkEnvResult {
	var res bulkEnvResult
	refuse := func(it FleetBulkItem, reason, code string) {
		res.skipped = append(res.skipped, FleetBulkRefused{EnvID: envID, Kind: it.Kind, Name: it.Name, Reason: reason, Code: code})
	}
	refuseAll := func(reason string) {
		for _, it := range items {
			refuse(it, reason, "")
		}
	}
	env, readable, err := bulkEnv(ctx, d, perEnv, envID)
	switch {
	case err != nil:
		d.log().Warn("could not resolve an environment of a bulk update", "environment", envID, "error", err)
		refuseAll(bulkOffline)
		return res
	case !readable || env == nil:
		refuseAll(bulkNoPermission)
		return res
	}

	// What the caller may do to each item here. A service of a stack TierPlan manages also needs stacks.deploy, which the applier
	// asks (BulkRequest.StackDeploy) once it knows which services those are.
	canControl := holdsEnvPerm(perEnv, envID, rbac.PermServicesControl)
	canContainers := holdsEnvPerm(perEnv, envID, rbac.PermContainersControl)
	var permitted []FleetBulkItem
	for _, it := range items {
		if !canControl || (it.Kind == fleetKindContainer && !canContainers) {
			refuse(it, bulkNoPermission, "")
			continue
		}
		permitted = append(permitted, it)
	}
	if len(permitted) == 0 {
		return res
	}

	// What the server sees now decides what can be taken, never what the client saw.
	selectable, err := bulkSelectable(ctx, d, envID)
	if err != nil {
		d.log().Warn("could not read an environment to update its workloads in bulk", "environment", envID, "error", err)
		for _, it := range permitted {
			refuse(it, bulkOffline, "")
		}
		return res
	}
	var taken []updates.BulkItem
	for _, it := range permitted {
		switch selectable[bulkKey(it.Kind, it.Name)] {
		case bulkTakeable:
			taken = append(taken, updates.BulkItem{Kind: it.Kind, Name: it.Name, Expect: it.Expect})
		case bulkHeldBack:
			refuse(it, bulkLocked, "")
		default:
			refuse(it, bulkNotSelectable, "")
		}
	}
	if len(taken) == 0 {
		return res
	}

	req := updates.BulkRequest{
		EnvID: envID, EnvName: env.Name, BulkID: bulkID, Items: taken,
		Actor: p.AuditActor(), IP: httpx.ClientIP(ctx), UserAgent: userAgent, Owner: stackJobOwner(p),
		StackDeploy: holdsEnvPerm(perEnv, envID, rbac.PermStacksDeploy),
	}
	if d.Stacks.Store != nil && d.Stacks.Locks != nil {
		req.StackItem = bulkStackItem(d, p, env)
	}
	jobID, skipped, err := d.Applier.StartBulk(ctx, req)
	switch {
	case errors.Is(err, jobs.ErrTooMany):
		for _, it := range taken {
			refuse(FleetBulkItem{Kind: it.Kind, Name: it.Name}, bulkTooManyJobs, "")
		}
		return res
	case err != nil:
		d.log().Warn("could not start a bulk image update", "environment", envID, "error", err)
		for _, it := range taken {
			refuse(FleetBulkItem{Kind: it.Kind, Name: it.Name}, bulkOffline, "")
		}
		return res
	}
	for _, s := range skipped {
		refuse(FleetBulkItem{Kind: s.Kind, Name: s.Name}, s.Reason, s.Code)
	}
	res.jobID = jobID
	return res
}

// The verdicts of bulkSelectable's answer.
const (
	bulkTakeable = "takeable"
	bulkHeldBack = "held_back"
)

// bulkSelectable reads the environment's workloads as the fleet's Updates tab does and says, for each (kind and name), whether it
// can be updated now: bulkTakeable when an update is available and nothing is wrong with it, bulkHeldBack when another operation
// holds the environment, and nothing else. It is the same rule as FleetUpdateItem.Selectable, which the server applies itself here.
func bulkSelectable(ctx context.Context, d UpdatesDeps, envID string) (map[string]string, error) {
	list, _, err := d.Updates.EnvVerdicts(ctx, envID)
	if err != nil {
		return nil, err
	}
	known := loadStackIDs(ctx, d.Store, envID, d.log())
	out := make(map[string]string, len(list))
	for _, wv := range list {
		item := fleetUpdateItem(wv, known)
		key := bulkKey(item.Kind, item.Name)
		switch {
		case item.Selectable:
			out[key] = bulkTakeable
		case wv.Verdict.State == updates.StateBusy:
			out[key] = bulkHeldBack
		}
	}
	return out, nil
}

// bulkStackItem is the deploy of a stack's share of a bulk update, as the caller: it finds the stack TierPlan manages and updates the
// chosen services as one revision of it inside the bulk job.
func bulkStackItem(d UpdatesDeps, p *rbac.Principal, env *httpx.Env) func(context.Context, *jobs.Reporter, updates.StackItemInput) error {
	return func(ctx context.Context, r *jobs.Reporter, in updates.StackItemInput) error {
		st, err := d.Stacks.repo().Stack(ctx, in.EnvID, in.StackID)
		switch {
		case errors.Is(err, stackops.ErrStackNotFound):
			return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: updates.NotUpdatableComposeUnmanaged}
		case err != nil:
			return err
		}
		return bulkRefusalOf(updateStackImagesInJob(ctx, d.Stacks, d.Applier, p, env, st, in, r))
	}
}

// bulkRefusalOf turns the problem a stack update refused with into the refusal the bulk job records, and leaves any other error as
// it is. A problem of the server's own (5xx) is not a refusal.
func bulkRefusalOf(err error) error {
	if err == nil {
		return nil
	}
	var he *huma.ErrorModel
	if !errors.As(err, &he) || he.Status >= http.StatusInternalServerError {
		return err
	}
	code := func() string {
		if len(he.Errors) > 0 {
			return he.Errors[0].Message
		}
		return ""
	}
	switch he.Type {
	case problemUpdateMoved:
		return &updates.BulkRefusal{Reason: bulkMoved, Code: bulkMoved}
	case problemOperationLocked:
		return &updates.BulkRefusal{Reason: bulkLocked, Code: bulkLocked}
	case problemNothingToUpdate:
		return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: updates.CodeNothingToUpdate}
	case problemPreviouslyFailed:
		return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: updates.CodePreviouslyFail}
	case problemUpdateCheckFailed:
		return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: updates.CodeCheckFailed}
	case problemNotUpdatable:
		return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: code()}
	}
	return &updates.BulkRefusal{Reason: bulkNotUpdatable, Code: he.Type}
}

// skipFleetUpdates skips the offered version of each item the caller may, with one bulk ID on every audit entry.
func skipFleetUpdates(ctx context.Context, d UpdatesDeps, p *rbac.Principal, in *FleetBulkSkipInput) (FleetBulkSkipBody, error) {
	if d.Checker == nil {
		return FleetBulkSkipBody{}, httpx.Problem(http.StatusServiceUnavailable, "", "Update checks are not available on this server.")
	}
	if err := d.ignoresReady(); err != nil {
		return FleetBulkSkipBody{}, err
	}
	bulkID, err := uuid.NewV7()
	if err != nil {
		return FleetBulkSkipBody{}, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		return FleetBulkSkipBody{}, snapshotProblem(d.log(), "", err)
	}
	body := FleetBulkSkipBody{BulkID: bulkID.String(), Refused: []FleetBulkRefused{}}
	var mu sync.Mutex
	refuse := func(envID string, it FleetBulkSkipItem, reason string) {
		mu.Lock()
		defer mu.Unlock()
		body.Refused = append(body.Refused, FleetBulkRefused{EnvID: envID, Kind: it.Kind, Name: it.Name, Reason: reason})
	}
	order, groups := groupByEnv(in.Body.Items, func(it FleetBulkSkipItem) string { return it.EnvID })
	for _, envID := range order {
		_, readable, err := bulkEnv(ctx, d, perEnv, envID)
		switch {
		case err != nil:
			d.log().Warn("could not resolve an environment of a bulk skip", "environment", envID, "error", err)
			for _, it := range groups[envID] {
				refuse(envID, it, bulkOffline)
			}
			continue
		case !readable || !holdsEnvPerm(perEnv, envID, rbac.PermUpdatesManage):
			for _, it := range groups[envID] {
				refuse(envID, it, bulkNoPermission)
			}
			continue
		}
		for _, it := range groups[envID] {
			// The skip of a single workload, as its own route does it: the version is the one the last check found, read here.
			// An item that was skipped already counts: the first skip stands.
			_, err := skipImageUpdate(ctx, d, p, envID, &UpdateSkipInput{
				UserAgent: in.UserAgent, Body: UpdateSkipRequest{Kind: it.Kind, Name: it.Name, Bulk: bulkID.String()},
			})
			if err == nil {
				body.Skipped++
				continue
			}
			var he *huma.ErrorModel
			if !errors.As(err, &he) {
				return FleetBulkSkipBody{}, err
			}
			switch {
			case he.Type == problemOperationLocked:
				refuse(envID, it, bulkLocked)
			case he.Status < http.StatusInternalServerError:
				refuse(envID, it, bulkNotSelectable)
			default:
				return FleetBulkSkipBody{}, err
			}
		}
	}
	return body, nil
}
