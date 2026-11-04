// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/internal/updates"
)

// When an environment is checked for image updates (plan 12-11, IMG-04, D-01, D-10, D-22): each field follows
// the instance default until the environment sets it, as the janitor's do, and a save is checked, versioned
// and audited. The routes copy the janitor's field for field.

// UpdateCheckBoolField is an on/off setting with the value that applies and whether the environment set it.
type UpdateCheckBoolField struct {
	Value      bool `json:"value" doc:"The value that applies: the environment's own when it set one, else the instance default"`
	Overridden bool `json:"overridden" doc:"True when this environment set the value itself. False means it follows the instance default, so a changed default reaches it"`
}

// UpdateCheckScheduleField is the schedule with its preset, the value that applies and whether the environment set it.
type UpdateCheckScheduleField struct {
	Value      string `json:"value" doc:"The five-field cron expression that applies, read in the server's time zone"`
	Preset     string `json:"preset" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the expression came from; custom for one written by hand"`
	Overridden bool   `json:"overridden" doc:"True when this environment set the schedule itself. False means it follows the instance default"`
}

// UpdateCheckStatusBody is how an environment is checked for image updates: its schedule, when it was checked
// last and when it is next, and the version a save must send back.
type UpdateCheckStatusBody struct {
	Enabled       UpdateCheckBoolField     `json:"enabled"`
	Schedule      UpdateCheckScheduleField `json:"schedule"`
	Timezone      string                   `json:"timezone" doc:"The server's time zone, which the schedule is read in"`
	LastCheckedAt string                   `json:"lastCheckedAt" format:"date-time" doc:"When an image of this environment was last checked, by the schedule or by Check now, whatever came of it; empty when none was or when it cannot be read now"`
	NextCheckAt   *string                  `json:"nextCheckAt" nullable:"true" format:"date-time" doc:"When the next scheduled check starts, in the server's time zone, including this environment's start offset of up to 10 minutes. Null when scheduled checks are off or the schedule cannot be used"`
	Version       int64                    `json:"version" minimum:"0" doc:"How many times this environment's schedule was saved, 0 while it has never been. Send it back with a save: a save made against any other version is 409 settings_changed"`
}

// UpdateCheckStatusOutput wraps UpdateCheckStatusBody for huma.
type UpdateCheckStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateCheckStatusBody
}

// UpdateCheckPolicyFields are the fields an environment sets itself. A field that is null or left out follows
// the instance default.
type UpdateCheckPolicyFields struct {
	Enabled  *bool   `json:"enabled,omitempty" nullable:"true" doc:"Whether the schedule checks this environment. Null follows the instance default"`
	Schedule *string `json:"schedule,omitempty" nullable:"true" maxLength:"100" doc:"A five-field cron expression that fires no more often than every hour. Null follows the instance default"`
	Preset   *string `json:"preset,omitempty" nullable:"true" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the schedule came from, shown on the page. Needs a schedule; custom when null with a schedule"`
}

// overrides is the request as the policy store takes it.
func (f UpdateCheckPolicyFields) overrides() updates.Overrides {
	return updates.Overrides{Enabled: f.Enabled, Schedule: f.Schedule, Preset: f.Preset}
}

// UpdateCheckPolicyRequest is the body of PUT /api/env/{envId}/updates/policy: the version read and the
// environment's own value for every field, or null to follow the instance default.
type UpdateCheckPolicyRequest struct {
	UpdateCheckPolicyFields
	Version int64 `json:"version" minimum:"0" doc:"The version read with the status; a save against another version is 409 settings_changed"`
}

// UpdateCheckPolicyInput is the path and body of the policy save.
type UpdateCheckPolicyInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      UpdateCheckPolicyRequest
}

// registerUpdatePolicy registers the routes that read and change when an environment is checked.
func registerUpdatePolicy(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getUpdateCheckStatus", Method: http.MethodGet, Path: "/api/env/{envId}/updates",
		Summary: "Read how an environment is checked for image updates",
		Description: "Answers whether the environment is checked by itself, its schedule and the time zone it is read in (each with the value that applies and whether this environment set it or follows the instance default), " +
			"when an image of it was last checked, when the next scheduled check starts, and the version a save must send back. " +
			"The default is every 6 hours; each environment starts at its own offset of up to 10 minutes that never changes. A check missed while the server was stopped is not made up. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, _ *EnvironmentPathInput) (*UpdateCheckStatusOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Policies == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := updateCheckStatus(ctx, d, env.ID)
			if err != nil {
				return nil, updatePolicyFailure(d, env.ID, "read the status", err)
			}
			return &UpdateCheckStatusOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setUpdateCheckPolicy", Method: http.MethodPut, Path: "/api/env/{envId}/updates/policy",
		Summary: "Change when an environment is checked for image updates",
		Description: "Replaces the settings this environment sets itself: enabled and the schedule, each a value, or null to follow the instance default again. " +
			"The schedule is a five-field cron expression that fires no more often than every hour (422 invalid_schedule, 422 schedule_too_frequent). " +
			"version is the one read with the status: a save made against any other version is 409 settings_changed and names who saved last. " +
			"A save that changes nothing answers 200 and writes nothing. Turning the checks off stops the scheduled ones at once; Check now still works. " +
			"Needs updates.manage (an Env Admin) on the environment. A change is audited as updates.policy_changed, naming the fields that changed; scheduled checks and Check now are not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermUpdatesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateCheckPolicyInput) (*UpdateCheckStatusOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Store == nil || d.Audit == nil || d.Policies == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setUpdateCheckPolicy(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &UpdateCheckStatusOutput{CacheControl: "no-store", Body: body}, nil
		})
	registerUpdatePreview(r, d)
	registerUpdateDefaults(r, d)
}

// updatePresetName is the preset the page shows, custom for any word it does not know.
func updatePresetName(preset string) string {
	switch preset {
	case schedule.PresetHourly, schedule.PresetEvery6h, schedule.PresetDaily, schedule.PresetWeekly:
		return preset
	}
	return schedule.PresetCustom
}

// updateCheckStatus reads the defaults, the environment's overrides and the stored checks into the status of an
// environment. The next check is worked out from the policy by the very function the scheduler fires by, so a
// schedule just saved shows its new time at once instead of when the scheduler next looks.
func updateCheckStatus(ctx context.Context, d UpdatesDeps, envID string) (UpdateCheckStatusBody, error) {
	def, _, err := d.Policies.Defaults(ctx)
	if err != nil {
		return UpdateCheckStatusBody{}, err
	}
	over, version, err := d.Policies.Env(ctx, envID)
	if err != nil {
		return UpdateCheckStatusBody{}, err
	}
	eff := updates.Effective(def, over)
	now, loc := d.now(), d.location()
	b := UpdateCheckStatusBody{
		Enabled:  UpdateCheckBoolField{Value: eff.Enabled, Overridden: over.Enabled != nil},
		Schedule: UpdateCheckScheduleField{Value: eff.Schedule, Preset: updatePresetName(eff.Preset), Overridden: over.Schedule != nil},
		Timezone: schedule.ZoneName(loc, now), Version: version,
	}
	if eff.Enabled {
		if at, err := schedule.NextRun(eff.Schedule, envID, now, loc, updates.MinInterval); err == nil && !at.IsZero() {
			s := at.In(loc).Format(time.RFC3339)
			b.NextCheckAt = &s
		}
	}
	if d.Updates != nil {
		// An environment that cannot be read now still has a schedule worth showing.
		if at, err := d.Updates.LastChecked(ctx, envID); err != nil {
			d.log().Warn("could not read when an environment was last checked for image updates", "environment", envID, "error", err)
		} else {
			b.LastCheckedAt = rfc3339(at)
		}
	}
	return b, nil
}

// updatePolicyValueProblem maps a value the policy refuses to its 422, and answers nil for any other error.
func updatePolicyValueProblem(err error) error {
	switch {
	case errors.Is(err, updates.ErrTooFrequent):
		return httpx.Problem(http.StatusUnprocessableEntity, problemScheduleTooFrequent,
			"That schedule checks more often than every "+strconv.Itoa(int(updates.MinInterval/time.Hour))+" hour. Choose a slower one.")
	case errors.Is(err, updates.ErrInvalidSchedule):
		return httpx.Problem(http.StatusUnprocessableEntity, problemInvalidSchedule,
			"That is not a schedule TierPlan can use. Write five fields, such as 0 3 * * *, and give a preset only together with its schedule.")
	}
	return nil
}

// updatePolicyFailure logs a failed read or write of the policy and answers it as an internal error.
func updatePolicyFailure(d UpdatesDeps, envID, what string, err error) error {
	d.log().Error("update check policy request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// storedPreset is the preset a stored schedule carries: the one given, custom when a schedule names none, and
// none without a schedule. It is what the store compares, so a field is only "changed" when what is stored is.
func storedPreset(sched, preset *string) string {
	if sched == nil {
		return ""
	}
	if preset == nil {
		return "custom"
	}
	return *preset
}

// changedUpdateFields names, in a fixed order, the fields whose stored value differs between two sets of overrides.
func changedUpdateFields(old, now updates.Overrides) []string {
	var out []string
	if !samePtr(old.Enabled, now.Enabled) {
		out = append(out, "enabled")
	}
	if !samePtr(old.Schedule, now.Schedule) || storedPreset(old.Schedule, old.Preset) != storedPreset(now.Schedule, now.Preset) {
		out = append(out, "schedule")
	}
	return out
}

// setUpdateCheckPolicy saves the overrides of an environment. The old overrides are read, the new ones written
// and the audit row recorded in one transaction. A save that changes nothing writes nothing.
func setUpdateCheckPolicy(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdateCheckPolicyInput) (UpdateCheckStatusBody, error) {
	want := in.Body.overrides()
	if err := updates.Validate(want); err != nil {
		if prob := updatePolicyValueProblem(err); prob != nil {
			return UpdateCheckStatusBody{}, prob
		}
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "validate", err)
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	old, _, err := d.Policies.EnvOn(ctx, q, envID)
	if err != nil {
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "read the policy", err)
	}
	changed, err := d.Policies.SetEnv(ctx, q, envID, want, in.Body.Version, p.ID, d.now())
	switch {
	case errors.Is(err, updates.ErrVersionChanged):
		return UpdateCheckStatusBody{}, settingsChanged(lastUpdatePolicyAuthor(ctx, d, q, envID))
	case err != nil:
		if prob := updatePolicyValueProblem(err); prob != nil {
			return UpdateCheckStatusBody{}, prob
		}
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "save the policy", err)
	}
	if changed {
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionUpdatesPolicyChanged, TargetType: auditTargetEnvironment, TargetID: envID, EnvID: envID,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"fields":  strings.Join(changedUpdateFields(old, want), ","),
				"version": strconv.FormatInt(in.Body.Version+1, 10),
			},
		}); err != nil {
			return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "audit", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "commit", err)
	}
	body, err := updateCheckStatus(ctx, d, envID)
	if err != nil {
		return UpdateCheckStatusBody{}, updatePolicyFailure(d, envID, "read the status", err)
	}
	return body, nil
}

// lastUpdatePolicyAuthor is the display name of whoever saved the environment's schedule last, empty when
// nobody did or that person is gone.
func lastUpdatePolicyAuthor(ctx context.Context, d UpdatesDeps, q *sqlc.Queries, envID string) string {
	row, err := q.GetImageUpdateEnvPolicy(ctx, envID)
	if err != nil || row.UpdatedBy == "" {
		return ""
	}
	return keeperName(ctx, AddressSpaceDeps{Store: d.Store}, map[string]string{}, row.UpdatedBy)
}

// UpdateSchedulePreviewRequest is the body of POST /api/env/{envId}/updates/schedule-preview.
type UpdateSchedulePreviewRequest struct {
	Schedule string `json:"schedule" maxLength:"100" doc:"A five-field cron expression, such as 0 3 * * *"`
}

// UpdateSchedulePreviewInput is the request of the schedule preview.
type UpdateSchedulePreviewInput struct {
	EnvironmentPathInput
	Body UpdateSchedulePreviewRequest
}

// UpdateSchedulePreviewBody is what a schedule would do.
type UpdateSchedulePreviewBody struct {
	Valid    bool        `json:"valid" doc:"Whether update checks accept the expression"`
	Problem  string      `json:"problem" enum:",invalid,too_frequent" doc:"Why it is not accepted: invalid when it is not a five-field cron expression update checks can use, too_frequent when two of its next ten firings are less than an hour apart. Empty when valid"`
	NextRuns []time.Time `json:"nextRuns" doc:"The next three checks in the server's time zone, with this environment's start offset; empty when the expression is not valid"`
	Timezone string      `json:"timezone" doc:"The server's time zone the expression is read in"`
}

// UpdateSchedulePreviewOutput wraps UpdateSchedulePreviewBody for huma.
type UpdateSchedulePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateSchedulePreviewBody
}

// registerUpdatePreview registers the schedule preview.
func registerUpdatePreview(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "previewUpdateCheckSchedule", Method: http.MethodPost, Path: "/api/env/{envId}/updates/schedule-preview",
		Summary: "Preview an update check schedule",
		Description: "Checks a cron expression and answers the next three times it would check this environment in the server's time zone, with this environment's start offset, " +
			"so the page shows what a schedule does before it is saved. An expression is valid when it is five fields and no two of its next ten firings are less than an hour apart. " +
			"An expression that is not valid answers 200 with valid false and the problem. It writes nothing. Needs environments.read on the environment.",
		Tags: []string{"environments"}, MaxBodyBytes: maxSchedulePreviewBody,
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateSchedulePreviewInput) (*UpdateSchedulePreviewOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			now, loc := d.now(), d.location()
			body := UpdateSchedulePreviewBody{NextRuns: []time.Time{}, Timezone: schedule.ZoneName(loc, now)}
			runs, err := schedule.NextRuns(in.Body.Schedule, env.ID, now, loc, schedulePreviewRuns, updates.MinInterval)
			switch {
			case errors.Is(err, schedule.ErrTooFrequent):
				body.Problem = "too_frequent"
			case err != nil:
				body.Problem = "invalid"
			default:
				body.Valid = true
				for _, at := range runs {
					body.NextRuns = append(body.NextRuns, at.In(loc))
				}
			}
			return &UpdateSchedulePreviewOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// UpdateCheckDefaultsFields are the instance defaults. Every field is required: the defaults are what an
// environment follows, so none of them can be left to follow anything.
type UpdateCheckDefaultsFields struct {
	Enabled  bool   `json:"enabled" doc:"Whether the schedule checks an environment that does not set this itself"`
	Schedule string `json:"schedule" minLength:"1" maxLength:"100" doc:"A five-field cron expression that fires no more often than every hour, read in the server's time zone"`
	Preset   string `json:"preset" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the schedule came from, shown on the page; custom for one written by hand"`
}

// policy is the defaults as the policy store takes them.
func (f UpdateCheckDefaultsFields) policy() updates.Policy {
	return updates.Policy{Enabled: f.Enabled, Schedule: f.Schedule, Preset: f.Preset}
}

func updateCheckDefaultsFields(p updates.Policy) UpdateCheckDefaultsFields {
	return UpdateCheckDefaultsFields{Enabled: p.Enabled, Schedule: p.Schedule, Preset: updatePresetName(p.Preset)}
}

// UpdateCheckDefaultsBody is the update check defaults of the instance, the version a save must send back and
// how many environments follow at least one of them.
type UpdateCheckDefaultsBody struct {
	UpdateCheckDefaultsFields
	Version   int64  `json:"version" minimum:"1" doc:"How many times the defaults were saved, starting at 1. Send it back with a save: a save made against any other version is 409 settings_changed"`
	Timezone  string `json:"timezone" doc:"The server's time zone, which the schedule is read in"`
	Followers int    `json:"followers" minimum:"0" doc:"How many environments follow at least one of these defaults, because they set nothing themselves or leave a field to the default"`
}

// UpdateCheckDefaultsOutput wraps UpdateCheckDefaultsBody for huma.
type UpdateCheckDefaultsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateCheckDefaultsBody
}

// UpdateCheckDefaultsRequest is the body of PUT /api/settings/update-checks: the version read and every default.
type UpdateCheckDefaultsRequest struct {
	UpdateCheckDefaultsFields
	Version int64 `json:"version" minimum:"1" doc:"The version read with the defaults; a save against another version is 409 settings_changed"`
}

// UpdateCheckDefaultsInput is the body of the defaults save.
type UpdateCheckDefaultsInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      UpdateCheckDefaultsRequest
}

// registerUpdateDefaults registers the instance defaults.
func registerUpdateDefaults(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getUpdateCheckDefaults", Method: http.MethodGet, Path: "/api/settings/update-checks",
		Summary: "Read the instance-wide update check defaults",
		Description: "The settings every environment's update checks follow until the environment sets the field itself (on or off, and the schedule, every 6 hours unless changed), " +
			"the version a save must send back, the server's time zone and how many environments follow at least one default. Needs settings.read.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, _ *struct{}) (*UpdateCheckDefaultsOutput, error) {
			if d.Policies == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := updateCheckDefaultsBody(ctx, d)
			if err != nil {
				return nil, updateDefaultsFailure(d, "read the defaults", err)
			}
			return &UpdateCheckDefaultsOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setUpdateCheckDefaults", Method: http.MethodPut, Path: "/api/settings/update-checks",
		Summary: "Change the instance-wide update check defaults",
		Description: "Replaces every default. A change reaches every environment that follows the field and none that sets it itself; nothing is written to an environment. " +
			"The schedule is a five-field cron expression that fires no more often than every hour (422 invalid_schedule, 422 schedule_too_frequent). " +
			"version is the one read with the defaults: a save made against any other version is 409 settings_changed and names who saved last. " +
			"A save that changes nothing answers 200 and writes nothing. " +
			"Needs settings.write: an instance admin, not an environment admin. A change is audited as updates.defaults_changed, naming the fields.",
		Tags: []string{"settings"},
	}, rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal},
		func(ctx context.Context, in *UpdateCheckDefaultsInput) (*UpdateCheckDefaultsOutput, error) {
			p := httpx.PrincipalFrom(ctx)
			if p == nil || d.Store == nil || d.Audit == nil || d.Policies == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setUpdateCheckDefaults(ctx, d, p, in)
			if err != nil {
				return nil, err
			}
			return &UpdateCheckDefaultsOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// updateCheckDefaultsBody reads the defaults and the number of environments that follow them.
func updateCheckDefaultsBody(ctx context.Context, d UpdatesDeps) (UpdateCheckDefaultsBody, error) {
	pol, version, err := d.Policies.Defaults(ctx)
	if err != nil {
		return UpdateCheckDefaultsBody{}, err
	}
	followers, err := d.Policies.Followers(ctx)
	if err != nil {
		return UpdateCheckDefaultsBody{}, err
	}
	return UpdateCheckDefaultsBody{
		UpdateCheckDefaultsFields: updateCheckDefaultsFields(pol), Version: version,
		Timezone: schedule.ZoneName(d.location(), d.now()), Followers: followers,
	}, nil
}

// updateDefaultsFailure logs a failed read or write of the defaults and answers it as an internal error.
func updateDefaultsFailure(d UpdatesDeps, what string, err error) error {
	d.log().Error("update check defaults request failed", "step", what, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// changedUpdateDefaultsFields names, in a fixed order, the fields whose value differs between two sets of defaults.
func changedUpdateDefaultsFields(old, now UpdateCheckDefaultsFields) []string {
	var out []string
	if old.Enabled != now.Enabled {
		out = append(out, "enabled")
	}
	if old.Schedule != now.Schedule || old.Preset != now.Preset {
		out = append(out, "schedule")
	}
	return out
}

// setUpdateCheckDefaults saves the defaults. The old defaults are read, the new ones written and the audit row
// recorded in one transaction. A save that changes nothing writes nothing.
func setUpdateCheckDefaults(ctx context.Context, d UpdatesDeps, p *rbac.Principal, in *UpdateCheckDefaultsInput) (UpdateCheckDefaultsBody, error) {
	pol := in.Body.policy()
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	oldPol, _, err := d.Policies.DefaultsOn(ctx, q)
	if err != nil {
		return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "read the defaults", err)
	}
	changed, err := d.Policies.SetDefaults(ctx, q, pol, in.Body.Version, p.ID, d.now())
	switch {
	case errors.Is(err, updates.ErrVersionChanged):
		return UpdateCheckDefaultsBody{}, settingsChanged(lastUpdateDefaultsAuthor(ctx, d, q))
	case err != nil:
		if prob := updatePolicyValueProblem(err); prob != nil {
			return UpdateCheckDefaultsBody{}, prob
		}
		return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "save the defaults", err)
	}
	if changed {
		fields := changedUpdateDefaultsFields(updateCheckDefaultsFields(oldPol), in.Body.UpdateCheckDefaultsFields)
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionUpdatesDefaultsChanged, TargetType: targetSettings, TargetID: targetSettings,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"fields":  strings.Join(fields, ","),
				"version": strconv.FormatInt(in.Body.Version+1, 10),
			},
		}); err != nil {
			return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "audit", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "commit", err)
	}
	body, err := updateCheckDefaultsBody(ctx, d)
	if err != nil {
		return UpdateCheckDefaultsBody{}, updateDefaultsFailure(d, "read the defaults", err)
	}
	return body, nil
}

// lastUpdateDefaultsAuthor is the display name of whoever saved the defaults last, empty when nobody did or
// that person is gone.
func lastUpdateDefaultsAuthor(ctx context.Context, d UpdatesDeps, q *sqlc.Queries) string {
	row, err := q.GetImageUpdateDefaults(ctx)
	if err != nil || row.UpdatedBy == "" {
		return ""
	}
	return keeperName(ctx, AddressSpaceDeps{Store: d.Store}, map[string]string{}, row.UpdatedBy)
}
