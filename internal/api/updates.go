// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/updates"
)

const (
	// checkNowTimeout is the longest a Check now request runs (D-01, D-10). What the check had stored by
	// then stays stored.
	checkNowTimeout = 60 * time.Second

	problemCheckTimedOut = "check_timed_out"
)

// UpdatesDeps are the dependencies of the image update routes.
type UpdatesDeps struct {
	// Updates asks the registries and judges what they answered. Nil means update checks are not wired
	// and Check now answers 503.
	Updates *updates.Service
	Checker *rbac.Checker
	// Store, Audit and Policies are what the schedule routes save through (plan 12-11): the policy store
	// writes on the transaction the audit row is recorded in. Nil means those routes answer 500.
	Store    *store.Store
	Audit    *audit.Writer
	Policies *updates.PolicyStore
	// Scheduler gives the time zone the schedules are read in. Nil means the server's local zone.
	Scheduler UpdateScheduler
	// Applier updates workloads to their newer image (plan 12-15). Nil means the update routes answer 503.
	Applier *updates.Applier
	// Heartbeat is how often an idle update stream gets a ping, MaxStreamsPerUser how many one person may hold open
	// and MaxLifetime how long one may live. Zero takes the defaults.
	Heartbeat, MaxLifetime time.Duration
	MaxStreamsPerUser      int
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// UpdateScheduler is what the routes ask of the update check scheduler. *updates.Scheduler is one.
type UpdateScheduler interface {
	// Location is the time zone the schedules are read in.
	Location() *time.Location
}

func (d UpdatesDeps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d UpdatesDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// location is the time zone schedules are read in: the scheduler's, else the server's local zone.
func (d UpdatesDeps) location() *time.Location {
	if d.Scheduler != nil {
		return d.Scheduler.Location()
	}
	return time.Local
}

// UpdateInfoBody is what the page says about the image of one workload (D-03). It carries TierPlan's
// words only: a state, a reason code and the digests, never the registry's own text.
type UpdateInfoBody struct {
	State            string            `json:"state" enum:"update_available,updating,busy,queued,up_to_date,check_stale,check_failed,rolled_back,rollback_failed,not_from_registry,pinned_in_file,not_checked,skipped,ignored" doc:"What is known about the image: update_available (the tag points at a digest the service does not run), up_to_date, check_stale (up to date when last checked, but that was too long ago), check_failed (see reason; never read as up to date), not_checked (no check yet), pinned_in_file (the file names a digest, not a tag), not_from_registry (the service names no digest to compare), and the states of an update in progress or finished: updating, busy, queued, rolled_back, rollback_failed, skipped, ignored"`
	Reason           string            `json:"reason" enum:",auth,rate_limited,unreachable,not_found,unsupported,other" doc:"Why the last check failed, in TierPlan's words; empty unless state is check_failed"`
	Registry         string            `json:"registry" doc:"The registry host the image is checked at; empty when the image cannot be checked"`
	Repository       string            `json:"repository" doc:"The repository below the registry host; empty when the image cannot be checked"`
	Tag              string            `json:"tag" doc:"The tag that is followed; empty for an image named by digest alone"`
	RunningDigest    string            `json:"runningDigest" doc:"The digest the service runs, from its spec; empty when its spec names none"`
	RemoteDigest     string            `json:"remoteDigest" doc:"The digest the tag pointed at when it was last checked successfully; empty before any check did"`
	ChangedPlatforms []string          `json:"changedPlatforms" doc:"For a multi-platform image, the platforms the service runs on whose image changed (linux/amd64). Empty when none is named"`
	CheckedAt        string            `json:"checkedAt" format:"date-time" doc:"When the tag was last checked, whatever came of it; empty when it never was"`
	SuccessAt        string            `json:"successAt" format:"date-time" doc:"When a check of the tag last succeeded; empty when none did"`
	Note             string            `json:"note" enum:",running_index_gone,may_be_stale" doc:"A plain-words addition: running_index_gone (the registry no longer serves the image index the service runs, so what changed cannot be told) or may_be_stale (the check may be out of date); empty for most states"`
	Ignore           *UpdateIgnoreBody `json:"ignore,omitempty" doc:"Set when the state is skipped or ignored: which, how far it reaches, who chose it and when"`
	Task             bool              `json:"task" doc:"True for a task container of a swarm service: its update is the service's, so it cannot be updated on its own"`
	Holder           *UpdateHolderBody `json:"holder,omitempty" doc:"Set when the state is busy: who holds the environment's operation lock, which an update cannot start under"`
	JobID            string            `json:"jobId,omitempty" doc:"Left out unless set: when the state is updating, the update's job, which GET /api/env/{envId}/update-jobs/{jobId} follows"`
}

// UpdateHolderBody is who holds an environment's operation lock, as the Busy badge and the confirm name them.
type UpdateHolderBody struct {
	Kind     string `json:"kind" enum:"git_deploy,deploy,rollback,janitor,update" doc:"What holds the lock: git_deploy (a stack's automatic deploy from git), deploy (any other stack operation that deploys, stops, starts or removes), rollback, janitor, or update (an image update, or a stack's Update images)"`
	By       string `json:"by" doc:"Who started it, as a name; for an automatic deploy a system name"`
	Started  string `json:"started" format:"date-time" doc:"When it started"`
	Workload string `json:"workload,omitempty" doc:"Left out unless set: for an image update, the service, container or stack being updated"`
	Stack    string `json:"stack,omitempty" doc:"Left out unless set: the stack a stack operation is on"`
}

// holderBodyOf turns the holder of an operation lock into its kind (updates.HolderKind) and who.
func holderBodyOf(h stackops.Holder) UpdateHolderBody {
	return UpdateHolderBody{Kind: updates.HolderKind(h), By: h.By, Started: rfc3339(h.Started), Workload: h.Workload, Stack: h.StackName}
}

// updateInfoOf turns a verdict into its answer. w is the workload the verdict is for, which names the
// image that is followed.
func updateInfoOf(v updates.Verdict, w updates.Workload) *UpdateInfoBody {
	b := &UpdateInfoBody{
		State: string(v.State), Reason: string(v.Reason),
		RunningDigest: v.RunningDigest, RemoteDigest: v.RemoteDigest,
		ChangedPlatforms: append([]string{}, v.ChangedPlatforms...),
		CheckedAt:        rfc3339(v.CheckedAt), SuccessAt: rfc3339(v.SuccessAt),
		Note: v.Note, Task: w.Task, Ignore: updateIgnoreOf(v.Ignore), JobID: v.JobID,
	}
	if v.Holder != nil {
		h := holderBodyOf(*v.Holder)
		b.Holder = &h
	}
	if w.Checkable() {
		b.Registry, b.Repository, b.Tag = w.Ref.Domain, w.Ref.Path, w.Ref.Tag
	}
	return b
}

// StackUpdateBody is what the page says about the images of a whole stack: its workloads' states rolled up
// (UI-SPEC "Stack aggregate"). Only the workloads that can be checked count.
type StackUpdateBody struct {
	State        string   `json:"state" enum:"update_available,updating,busy,queued,up_to_date,check_stale,check_failed,rolled_back,rollback_failed,not_checked,skipped,ignored,none" doc:"The first that applies of: updating, busy, queued, update_available, rollback_failed, rolled_back, check_failed, not_checked, ignored or skipped (when every other checkable workload is one), check_stale, up_to_date. up_to_date only when every checkable workload is up to date. none when no workload of the stack can be checked (see notCheckable): never read as up to date"`
	Count        int      `json:"count" minimum:"0" doc:"How many workloads have an update available; set when state is update_available, 0 otherwise"`
	NotCheckable []string `json:"notCheckable" enum:"pinned_in_file,not_from_registry" doc:"Why some workloads of the stack are not part of the state: pinned_in_file (the file names a digest, not a tag) or not_from_registry (the image has no registry to ask). With state none these are the reasons nothing can be checked"`
}

// stackUpdateOf turns a stack's roll-up into its answer.
func stackUpdateOf(v updates.StackVerdict) *StackUpdateBody {
	b := &StackUpdateBody{State: string(v.State), Count: v.Count, NotCheckable: make([]string, 0, len(v.NotCheckable))}
	for _, s := range v.NotCheckable {
		b.NotCheckable = append(b.NotCheckable, string(s))
	}
	return b
}

// CheckUpdatesBody is the answer of Check now.
type CheckUpdatesBody struct {
	Checked int `json:"checked" doc:"How many distinct images (registry, repository and tag) were asked about"`
	Updates int `json:"updates" doc:"How many of the environment's services have an update available now, judged platform by platform"`
	Failed  int `json:"failed" doc:"How many images could not be checked; each service's update object says why"`
}

// CheckUpdatesOutput wraps CheckUpdatesBody for huma.
type CheckUpdatesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         CheckUpdatesBody
}

// RegisterUpdates registers the image update routes.
func RegisterUpdates(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkEnvironmentUpdates", Method: http.MethodPost, Path: "/api/env/{envId}/updates/check",
		Summary: "Check the environment's images for updates now",
		Description: "Asks the registries what each image tag of the environment's swarm services points at now, with one manifest request for each distinct image and no more, and stores the answers. " +
			"Answers within 60 seconds with how many images were asked about, how many of the environment's services and containers have an update available (judged for the platforms each runs on; a swarm service's task containers count once, as the service) and how many images could not be checked; " +
			"a check that takes longer is 504 check_timed_out, and what it had learned stays stored. " +
			"The services and containers lists carry the outcome for each. Needs updates.check (Operator) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermUpdatesCheck, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*CheckUpdatesOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := checkEnvironmentUpdates(ctx, d, env.ID)
			if err != nil {
				return nil, err
			}
			return &CheckUpdatesOutput{CacheControl: "no-store", Body: body}, nil
		})
	registerUpdatePolicy(r, d)
	registerUpdateIgnores(r, d)
	registerUpdateRun(r, d)
	registerImageUpdatePreview(r, d)
	registerUpdateJobs(r, d)
}

// checkEnvironmentUpdates checks the images of an environment's services now and counts what it found.
func checkEnvironmentUpdates(ctx context.Context, d UpdatesDeps, envID string) (CheckUpdatesBody, error) {
	u := d.Updates
	if u == nil || u.Clusters == nil || u.Checker == nil {
		return CheckUpdatesBody{}, httpx.Problem(http.StatusServiceUnavailable, "", "Update checks are not available on this server.")
	}
	cctx, cancel := context.WithTimeout(ctx, checkNowTimeout)
	defer cancel()
	res, err := u.CheckNow(cctx, envID)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return CheckUpdatesBody{}, httpx.Problem(http.StatusGatewayTimeout, problemCheckTimedOut, "The check took longer than 60 seconds. What it learned is kept; check again.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return CheckUpdatesBody{}, httpx.Problem(http.StatusServiceUnavailable, "", "The check was cut short.")
	default:
		return CheckUpdatesBody{}, snapshotProblem(d.log(), envID, err)
	}
	// CheckResult.Updates counts workloads whose running digest differs from the tag's, which a rebuild of a
	// platform nobody runs also does. The count shown is the one the verdicts give.
	return CheckUpdatesBody{Checked: res.Checked, Updates: countUpdates(cctx, d, envID), Failed: res.Failed}, nil
}

// countUpdates counts the services and containers of an environment whose verdict is update_available. A
// verdict that cannot be read is logged and counts none: the lists say what each one is.
func countUpdates(ctx context.Context, d UpdatesDeps, envID string) int {
	n, err := d.Updates.Count(ctx, envID)
	if err != nil {
		d.log().Warn("could not judge the images to count their updates", "environment", envID, "error", err)
		return 0
	}
	return n
}

// containerUpdates judges the images of the containers of agg, keyed by container ID, with the workload each
// verdict is for. A failure to read the verdicts is logged and answers none: a list never fails for it.
func containerUpdates(ctx context.Context, u *updates.Service, log *slog.Logger, envID string, agg envs.Aggregate) updates.ContainerJudgement {
	if u == nil {
		return updates.ContainerJudgement{}
	}
	j, err := u.ContainerJudgements(ctx, envID, agg)
	if err != nil {
		log.Warn("could not judge the images of the containers", "environment", envID, "error", err)
		return updates.ContainerJudgement{}
	}
	return j
}

// updateOfContainer is the update object of one container, nil when it has none.
func updateOfContainer(j updates.ContainerJudgement, id string) *UpdateInfoBody {
	if v, ok := j.Verdicts[id]; ok {
		return updateInfoOf(v, j.Workloads[id])
	}
	return nil
}
