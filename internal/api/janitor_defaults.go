// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The janitor defaults of the instance and the retention of the run history (plan 11-21, JAN-01, JAN-10,
// D-04, D-16, D-21): every environment follows a default until it sets the field itself, and an instance
// admin (settings.write) changes them for all of them at once.

// JanitorDefaultsFields are the instance defaults and the retention. Every field is required: the
// defaults are what an environment follows, so none of them can be left to follow anything.
type JanitorDefaultsFields struct {
	Enabled                 bool   `json:"enabled" doc:"Whether the schedule runs the janitor on an environment that does not set this itself"`
	Schedule                string `json:"schedule" minLength:"1" maxLength:"100" doc:"A five-field cron expression that fires no more often than every 15 minutes, read in the server's time zone"`
	SchedulePreset          string `json:"schedulePreset" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the schedule came from, shown on the page; custom for one written by hand"`
	KeepN                   int    `json:"keepN" minimum:"0" maximum:"20" doc:"How many of the newest exited task containers of each service slot are kept, 0 to 20"`
	GraceHours              int    `json:"graceHours" minimum:"1" maximum:"720" doc:"How many hours an object must have been stopped or idle before it can be removed, 1 to 720"`
	RemoveUnusedNetworks    bool   `json:"removeUnusedNetworks" doc:"Whether networks no stack ever owned are removed too"`
	RemoveStoppedContainers bool   `json:"removeStoppedContainers" doc:"Whether stopped containers no stack or project ever owned are removed too"`
	RetentionDays           int    `json:"retentionDays" minimum:"7" maximum:"3650" doc:"How many days a run stays in the history, 7 to 3650. A run is kept while it is that recent or among the newest retentionRuns runs of its environment, whichever keeps more"`
	RetentionRuns           int    `json:"retentionRuns" minimum:"10" maximum:"1000" doc:"How many of the newest runs of each environment stay in the history, 10 to 1000"`
}

// policy is the defaults as the policy store takes them.
func (f JanitorDefaultsFields) policy() janitor.Policy {
	return janitor.Policy{
		Enabled: f.Enabled, Schedule: f.Schedule, SchedulePreset: f.SchedulePreset, KeepN: f.KeepN,
		Grace: time.Duration(f.GraceHours) * time.Hour, RemoveUnusedNetworks: f.RemoveUnusedNetworks,
		RemoveStoppedContainers: f.RemoveStoppedContainers,
	}
}

func (f JanitorDefaultsFields) retention() janitor.Retention {
	return janitor.Retention{Days: f.RetentionDays, Runs: f.RetentionRuns}
}

func janitorDefaultsFields(p janitor.Policy, r janitor.Retention) JanitorDefaultsFields {
	return JanitorDefaultsFields{
		Enabled: p.Enabled, Schedule: p.Schedule, SchedulePreset: presetName(p.SchedulePreset), KeepN: p.KeepN,
		GraceHours: int(p.Grace / time.Hour), RemoveUnusedNetworks: p.RemoveUnusedNetworks,
		RemoveStoppedContainers: p.RemoveStoppedContainers, RetentionDays: r.Days, RetentionRuns: r.Runs,
	}
}

// JanitorDefaultsBody is the janitor defaults of the instance, the run-history retention, the version a save
// must send back and how many environments follow at least one of the defaults.
type JanitorDefaultsBody struct {
	JanitorDefaultsFields
	Version               int64  `json:"version" minimum:"1" doc:"How many times the defaults were saved, starting at 1. Send it back with a save: a save made against any other version is 409 settings_changed"`
	Timezone              string `json:"timezone" doc:"The server's time zone, which the schedule is read in"`
	FollowingEnvironments int    `json:"followingEnvironments" minimum:"0" doc:"How many environments follow at least one of these defaults, because they set nothing themselves or leave a field to the default"`
	RemovedRuns           int    `json:"removedRuns" minimum:"0" doc:"How many run records the save removed because it lowered the retention; 0 for a read and for a save that kept every run"`
}

// JanitorDefaultsOutput wraps JanitorDefaultsBody for huma.
type JanitorDefaultsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorDefaultsBody
}

// JanitorDefaultsRequest is the body of PUT /api/settings/janitor: the version read and every default.
type JanitorDefaultsRequest struct {
	JanitorDefaultsFields
	Version int64 `json:"version" minimum:"1" doc:"The version read with the defaults; a save against another version is 409 settings_changed"`
}

// JanitorDefaultsInput is the body of the defaults save.
type JanitorDefaultsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      JanitorDefaultsRequest
}

// JanitorRetentionPreviewInput is the retention a preview counts for.
type JanitorRetentionPreviewInput struct {
	Days int `query:"days" required:"true" minimum:"7" maximum:"3650" doc:"The days to count for, 7 to 3650"`
	Runs int `query:"runs" required:"true" minimum:"10" maximum:"1000" doc:"The runs per environment to count for, 10 to 1000"`
}

// JanitorRetentionPreviewBody is what a retention would delete.
type JanitorRetentionPreviewBody struct {
	Runs         int `json:"runs" doc:"How many run records saving this retention would delete now, across all environments"`
	Environments int `json:"environments" doc:"How many environments those runs belong to"`
}

// JanitorRetentionPreviewOutput wraps JanitorRetentionPreviewBody for huma.
type JanitorRetentionPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorRetentionPreviewBody
}

const problemRetentionOutOfRange = "retention_out_of_range"

// registerJanitorDefaults registers the janitor defaults and the retention preview.
func registerJanitorDefaults(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getJanitorDefaults", Method: http.MethodGet, Path: "/api/settings/janitor",
		Summary: "Read the instance-wide janitor defaults",
		Description: "The settings every environment's janitor follows until the environment sets the field itself (on or off, schedule, how many exited task containers to keep, the grace period and the two opt-ins), " +
			"how long the run history is kept (retentionDays and retentionRuns), the version a save must send back, the server's time zone and how many environments follow at least one default. Needs settings.read.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, _ *struct{}) (*JanitorDefaultsOutput, error) {
			if d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := janitorDefaultsBody(ctx, d)
			if err != nil {
				return nil, janitorDefaultsFailure(d, "read the defaults", err)
			}
			return &JanitorDefaultsOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setJanitorDefaults", Method: http.MethodPut, Path: "/api/settings/janitor",
		Summary: "Change the instance-wide janitor defaults",
		Description: "Replaces every default and the run-history retention. A change reaches every environment that follows the field and none that sets it itself; nothing is written to an environment. " +
			"keepN is 0 to 20, graceHours 1 to 720, retentionDays 7 to 3650, retentionRuns 10 to 1000, and the schedule a five-field cron expression that fires no more often than every 15 minutes (422 invalid_schedule, 422 schedule_too_frequent). " +
			"version is the one read with the defaults: a save made against any other version is 409 settings_changed and names who saved last. " +
			"Lowering the retention deletes the run records it no longer keeps, in every environment, in the same transaction as the new value; the answer's removedRuns says how many, and a run that is waiting or running is never deleted. " +
			"Use the retention preview to count them first. A save that changes nothing answers 200 and writes nothing. " +
			"Needs settings.write: an instance admin, not an environment admin or an operator with janitor.policy. A change is audited as janitor.defaults_changed, naming the fields and how many runs were removed.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, in *JanitorDefaultsInput) (*JanitorDefaultsOutput, error) {
			p := httpx.PrincipalFrom(ctx)
			if p == nil || d.Store == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setJanitorDefaults(ctx, d, p, in)
			if err != nil {
				return nil, err
			}
			return &JanitorDefaultsOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "previewJanitorRetention", Method: http.MethodGet, Path: "/api/settings/janitor/retention-preview",
		Summary: "Count the runs a lower retention would delete",
		Description: "Answers how many run records across how many environments saving a retention of days and runs would delete now, by the rule saving applies: a run stays while it is recent enough or among the newest runs of its environment, " +
			"and a run that is waiting or running is never counted. A retention that does not lower the stored one deletes nothing and answers 0. It only reads. Needs settings.write, as the change it previews does.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, in *JanitorRetentionPreviewInput) (*JanitorRetentionPreviewOutput, error) {
			if d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			runs, envs, err := d.Policies.RetentionPreview(ctx, janitor.Retention{Days: in.Days, Runs: in.Runs}, d.now())
			if err != nil {
				if errors.Is(err, janitor.ErrOutOfRange) {
					return nil, retentionRangeProblem()
				}
				return nil, janitorDefaultsFailure(d, "count the runs a retention would delete", err)
			}
			return &JanitorRetentionPreviewOutput{CacheControl: "no-store", Body: JanitorRetentionPreviewBody{Runs: runs, Environments: envs}}, nil
		})
}

// janitorDefaultsBody reads the defaults, the retention and the number of environments that follow them.
func janitorDefaultsBody(ctx context.Context, d JanitorDeps) (JanitorDefaultsBody, error) {
	pol, ret, version, err := d.Policies.Defaults(ctx)
	if err != nil {
		return JanitorDefaultsBody{}, err
	}
	followers, err := d.Policies.Followers(ctx)
	if err != nil {
		return JanitorDefaultsBody{}, err
	}
	return JanitorDefaultsBody{
		JanitorDefaultsFields: janitorDefaultsFields(pol, ret), Version: version,
		Timezone: janitor.ZoneName(d.location(), d.now()), FollowingEnvironments: followers,
	}, nil
}

// janitorDefaultsFailure logs a failed read or write of the defaults and answers it as an internal error.
func janitorDefaultsFailure(d JanitorDeps, what string, err error) error {
	d.log().Error("janitor defaults request failed", "step", what, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// retentionRangeProblem is the 422 for a retention outside the ranges.
func retentionRangeProblem() error {
	return httpx.Problem(http.StatusUnprocessableEntity, problemRetentionOutOfRange,
		fmt.Sprintf("Keep run history for %d to %d days and %d to %d runs per environment.",
			janitor.MinRetentionDays, janitor.MaxRetentionDays, janitor.MinRetentionRuns, janitor.MaxRetentionRuns))
}

// janitorDefaultsProblem maps a value the defaults refuse to its 422, and answers nil for any other error.
func janitorDefaultsProblem(r janitor.Retention, err error) error {
	if prob := janitorPolicyValueProblem(err); prob != nil {
		if errors.Is(err, janitor.ErrOutOfRange) && !r.InRange() {
			return retentionRangeProblem()
		}
		return prob
	}
	return nil
}

// changedDefaultsFields names, in a fixed order, the fields whose value differs between two sets of defaults.
func changedDefaultsFields(old, now JanitorDefaultsFields) []string {
	var out []string
	add := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}
	add("enabled", old.Enabled != now.Enabled)
	add("schedule", old.Schedule != now.Schedule || old.SchedulePreset != now.SchedulePreset)
	add("keepN", old.KeepN != now.KeepN)
	add("graceHours", old.GraceHours != now.GraceHours)
	add("removeUnusedNetworks", old.RemoveUnusedNetworks != now.RemoveUnusedNetworks)
	add("removeStoppedContainers", old.RemoveStoppedContainers != now.RemoveStoppedContainers)
	add("retentionDays", old.RetentionDays != now.RetentionDays)
	add("retentionRuns", old.RetentionRuns != now.RetentionRuns)
	return out
}

// setJanitorDefaults saves the defaults. The values are checked first; then the old defaults are read, the new
// ones written, the runs a lowered retention no longer keeps deleted and the audit row recorded, all in one
// transaction. A save that changes nothing writes nothing.
func setJanitorDefaults(ctx context.Context, d JanitorDeps, p *rbac.Principal, in *JanitorDefaultsInput) (JanitorDefaultsBody, error) {
	pol, ret := in.Body.policy(), in.Body.retention()
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	oldPol, oldRet, _, err := d.Policies.DefaultsOn(ctx, q)
	if err != nil {
		return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "read the defaults", err)
	}
	removed, changed, err := d.Policies.SetDefaults(ctx, q, pol, ret, in.Body.Version, p.ID, d.now())
	switch {
	case errors.Is(err, janitor.ErrPolicyVersion):
		return JanitorDefaultsBody{}, settingsChanged(lastJanitorDefaultsAuthor(ctx, d, q))
	case err != nil:
		if prob := janitorDefaultsProblem(ret, err); prob != nil {
			return JanitorDefaultsBody{}, prob
		}
		return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "save the defaults", err)
	}
	if changed {
		fields := changedDefaultsFields(janitorDefaultsFields(oldPol, oldRet), in.Body.JanitorDefaultsFields)
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionJanitorDefaultsChanged, TargetType: targetSettings, TargetID: targetSettings,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"fields":       strings.Join(fields, ","),
				"removed_runs": strconv.FormatInt(removed, 10),
				"version":      strconv.FormatInt(in.Body.Version+1, 10),
			},
		}); err != nil {
			return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "audit", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "commit", err)
	}
	if changed && oldPol.Grace != pol.Grace {
		forgetOrphansFollowingGrace(ctx, d)
	}
	body, err := janitorDefaultsBody(ctx, d)
	if err != nil {
		return JanitorDefaultsBody{}, janitorDefaultsFailure(d, "read the defaults", err)
	}
	body.RemovedRuns = int(removed)
	return body, nil
}

// forgetOrphansFollowingGrace drops the kept orphan report of every environment whose grace follows the
// default, so the Address space page judges by the new default at its next read (D-20). An environment that
// sets its own grace keeps its report. A failure is logged and changes nothing else: a report is kept for a
// few seconds only.
func forgetOrphansFollowingGrace(ctx context.Context, d JanitorDeps) {
	if d.AddressSpaces == nil {
		return
	}
	rows, err := d.Store.RQ.ListEnvironments(ctx)
	if err != nil {
		d.log().Warn("could not list the environments to refresh their orphan lists after the default grace changed", "error", err)
		return
	}
	for _, row := range rows {
		over, _, err := d.Policies.Env(ctx, row.ID)
		if err != nil {
			d.log().Warn("could not read an environment's janitor policy to refresh its orphan list", "environment", row.ID, "error", err)
		}
		if err != nil || over.GraceHours == nil {
			// A policy that could not be read is dropped too: a stale list is worse than one more read.
			d.AddressSpaces.ForgetOrphans(row.ID)
		}
	}
}

// lastJanitorDefaultsAuthor is the display name of whoever saved the defaults last, empty when nobody did or
// that person is gone.
func lastJanitorDefaultsAuthor(ctx context.Context, d JanitorDeps, q *sqlc.Queries) string {
	row, err := q.GetJanitorDefaults(ctx)
	if err != nil || row.UpdatedBy == "" {
		return ""
	}
	return keeperName(ctx, AddressSpaceDeps{Store: d.Store}, map[string]string{}, row.UpdatedBy)
}
