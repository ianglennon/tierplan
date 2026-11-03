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

// The janitor policy of one environment (plan 11-19, JAN-01, D-04, D-15, D-21): each field follows the
// instance default until the environment sets it, and a save is checked, versioned and audited.

const (
	problemInvalidSchedule     = "invalid_schedule"
	problemScheduleTooFrequent = "schedule_too_frequent"
	problemPolicyOutOfRange    = "policy_out_of_range"
)

// JanitorBoolField is an on/off setting with the value that applies and whether the environment set it.
type JanitorBoolField struct {
	Value      bool `json:"value" doc:"The value that applies: the environment's own when it set one, else the instance default"`
	Overridden bool `json:"overridden" doc:"True when this environment set the value itself. False means it follows the instance default, so a changed default reaches it"`
}

// JanitorIntField is a number setting with the value that applies and whether the environment set it.
type JanitorIntField struct {
	Value      int  `json:"value" doc:"The value that applies: the environment's own when it set one, else the instance default"`
	Overridden bool `json:"overridden" doc:"True when this environment set the value itself. False means it follows the instance default, so a changed default reaches it"`
}

// JanitorScheduleField is the schedule with its preset, the value that applies and whether the environment set it.
type JanitorScheduleField struct {
	Value      string `json:"value" doc:"The five-field cron expression that applies, read in the server's time zone"`
	Preset     string `json:"preset" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the expression came from; custom for one written by hand"`
	Overridden bool   `json:"overridden" doc:"True when this environment set the schedule itself. False means it follows the instance default"`
}

// JanitorPolicyValues are the values of a policy with nothing marked as set: the instance defaults.
type JanitorPolicyValues struct {
	Enabled                 bool   `json:"enabled" doc:"Whether the schedule runs the janitor"`
	Schedule                string `json:"schedule" doc:"The five-field cron expression"`
	SchedulePreset          string `json:"schedulePreset" enum:"hourly,every_6h,daily,weekly,custom"`
	KeepN                   int    `json:"keepN" minimum:"0" maximum:"20" doc:"How many of the newest exited task containers of each service slot are kept"`
	GraceHours              int    `json:"graceHours" minimum:"1" maximum:"720" doc:"How many hours an object must have been stopped or idle before it can be removed"`
	RemoveUnusedNetworks    bool   `json:"removeUnusedNetworks" doc:"Whether networks no stack ever owned are removed too"`
	RemoveStoppedContainers bool   `json:"removeStoppedContainers" doc:"Whether stopped containers no stack or project ever owned are removed too"`
}

// JanitorPolicyBody is the janitor policy of an environment: each field's effective value and whether the
// environment set it, the instance defaults, and the version a save must send back.
type JanitorPolicyBody struct {
	Version                 int64                `json:"version" minimum:"0" doc:"How many times this environment's policy was saved, 0 while it has never been. Send it back with a save: a save made against any other version is 409 settings_changed"`
	Timezone                string               `json:"timezone" doc:"The server's time zone, which the schedule is read in"`
	Enabled                 JanitorBoolField     `json:"enabled"`
	Schedule                JanitorScheduleField `json:"schedule"`
	KeepN                   JanitorIntField      `json:"keepN"`
	GraceHours              JanitorIntField      `json:"graceHours"`
	RemoveUnusedNetworks    JanitorBoolField     `json:"removeUnusedNetworks"`
	RemoveStoppedContainers JanitorBoolField     `json:"removeStoppedContainers"`
	Defaults                JanitorPolicyValues  `json:"defaults" doc:"The instance defaults every field that is not overridden follows"`
}

// JanitorPolicyOutput wraps JanitorPolicyBody for huma.
type JanitorPolicyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorPolicyBody
}

// JanitorPolicyFields are the fields an environment sets itself. A field that is null or left out follows
// the instance default.
type JanitorPolicyFields struct {
	Enabled                 *bool   `json:"enabled,omitempty" nullable:"true" doc:"Whether the schedule runs the janitor. Null follows the instance default"`
	Schedule                *string `json:"schedule,omitempty" nullable:"true" maxLength:"100" doc:"A five-field cron expression that fires no more often than every 15 minutes. Null follows the instance default"`
	SchedulePreset          *string `json:"schedulePreset,omitempty" nullable:"true" enum:"hourly,every_6h,daily,weekly,custom" doc:"The preset the schedule came from, shown on the page. Needs a schedule; custom when null with a schedule"`
	KeepN                   *int    `json:"keepN,omitempty" nullable:"true" minimum:"0" maximum:"20" doc:"How many of the newest exited task containers of each service slot are kept, 0 to 20. Null follows the instance default"`
	GraceHours              *int    `json:"graceHours,omitempty" nullable:"true" minimum:"1" maximum:"720" doc:"How many hours an object must have been stopped or idle before it can be removed, 1 to 720. Null follows the instance default"`
	RemoveUnusedNetworks    *bool   `json:"removeUnusedNetworks,omitempty" nullable:"true" doc:"Whether networks no stack ever owned are removed too. Null follows the instance default"`
	RemoveStoppedContainers *bool   `json:"removeStoppedContainers,omitempty" nullable:"true" doc:"Whether stopped containers no stack or project ever owned are removed too. Null follows the instance default"`
}

// overrides is the request as the policy store takes it.
func (f JanitorPolicyFields) overrides() janitor.Overrides {
	return janitor.Overrides{
		Enabled: f.Enabled, Schedule: f.Schedule, SchedulePreset: f.SchedulePreset, KeepN: f.KeepN, GraceHours: f.GraceHours,
		RemoveUnusedNetworks: f.RemoveUnusedNetworks, RemoveStoppedContainers: f.RemoveStoppedContainers,
	}
}

// JanitorPolicyRequest is the body of PUT /api/env/{envId}/janitor/policy: the version read and the
// environment's own value for every field, or null to follow the instance default.
type JanitorPolicyRequest struct {
	JanitorPolicyFields
	Version int64 `json:"version" minimum:"0" doc:"The version read with the policy; a save against another version is 409 settings_changed"`
}

// JanitorPolicyInput is the path and body of the policy save.
type JanitorPolicyInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      JanitorPolicyRequest
}

// registerJanitorPolicy registers the policy routes of an environment.
func registerJanitorPolicy(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getJanitorPolicy", Method: http.MethodGet, Path: "/api/env/{envId}/janitor/policy",
		Summary: "Read an environment's janitor policy",
		Description: "Answers, for each setting (on or off, schedule, how many exited task containers to keep, the grace period and the two opt-ins), the value that applies and whether this environment set it or follows the instance default, " +
			"the instance defaults, the version a save must send back and the server's time zone the schedule is read in. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, _ *EnvironmentPathInput) (*JanitorPolicyOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := janitorPolicyBody(ctx, d, env.ID)
			if err != nil {
				return nil, janitorPolicyFailure(d, env.ID, "read the policy", err)
			}
			return &JanitorPolicyOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setJanitorPolicy", Method: http.MethodPut, Path: "/api/env/{envId}/janitor/policy",
		Summary: "Change an environment's janitor policy",
		Description: "Replaces the settings this environment sets itself: every field is a value, or null to follow the instance default again. " +
			"keepN is 0 to 20, graceHours 1 to 720, and the schedule a five-field cron expression that fires no more often than every 15 minutes (422 invalid_schedule, 422 schedule_too_frequent). " +
			"version is the one read with the policy: a save made against any other version is 409 settings_changed and names who saved last. " +
			"A save that changes nothing answers 200 and writes nothing. Turning the janitor off stops its scheduled runs at once; Clean now still works. " +
			"Use the janitor preview with the candidate policy to see what the change would remove before it is saved. " +
			"Needs janitor.policy (an Operator with that extra, or an Env Admin) on the environment. A change is audited as janitor.policy_changed, naming the fields that changed.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermJanitorPolicy, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorPolicyInput) (*JanitorPolicyOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Store == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setJanitorPolicy(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &JanitorPolicyOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// presetName is the preset the page shows, custom for any word it does not know.
func presetName(preset string) string {
	switch preset {
	case janitor.PresetHourly, janitor.PresetEvery6h, janitor.PresetDaily, janitor.PresetWeekly:
		return preset
	}
	return janitor.PresetCustom
}

// policyValues is a policy as the wire shows it.
func policyValues(p janitor.Policy) JanitorPolicyValues {
	return JanitorPolicyValues{
		Enabled: p.Enabled, Schedule: p.Schedule, SchedulePreset: presetName(p.SchedulePreset), KeepN: p.KeepN,
		GraceHours: int(p.Grace / time.Hour), RemoveUnusedNetworks: p.RemoveUnusedNetworks, RemoveStoppedContainers: p.RemoveStoppedContainers,
	}
}

// janitorPolicyBody reads the defaults and the environment's overrides into the answer of the policy routes.
func janitorPolicyBody(ctx context.Context, d JanitorDeps, envID string) (JanitorPolicyBody, error) {
	def, _, _, err := d.Policies.Defaults(ctx)
	if err != nil {
		return JanitorPolicyBody{}, err
	}
	env, version, err := d.Policies.Env(ctx, envID)
	if err != nil {
		return JanitorPolicyBody{}, err
	}
	eff := policyValues(janitor.Effective(def, env))
	return JanitorPolicyBody{
		Version: version, Timezone: janitor.ZoneName(d.location(), d.now()),
		Enabled:                 JanitorBoolField{Value: eff.Enabled, Overridden: env.Enabled != nil},
		Schedule:                JanitorScheduleField{Value: eff.Schedule, Preset: eff.SchedulePreset, Overridden: env.Schedule != nil},
		KeepN:                   JanitorIntField{Value: eff.KeepN, Overridden: env.KeepN != nil},
		GraceHours:              JanitorIntField{Value: eff.GraceHours, Overridden: env.GraceHours != nil},
		RemoveUnusedNetworks:    JanitorBoolField{Value: eff.RemoveUnusedNetworks, Overridden: env.RemoveUnusedNetworks != nil},
		RemoveStoppedContainers: JanitorBoolField{Value: eff.RemoveStoppedContainers, Overridden: env.RemoveStoppedContainers != nil},
		Defaults:                policyValues(def),
	}, nil
}

// janitorPolicyValueProblem maps a value the policy refuses to its 422, and answers nil for any other error.
func janitorPolicyValueProblem(err error) error {
	switch {
	case errors.Is(err, janitor.ErrScheduleTooFrequent):
		return httpx.Problem(http.StatusUnprocessableEntity, problemScheduleTooFrequent,
			"That schedule runs more often than every "+strconv.Itoa(int(janitor.MinInterval/time.Minute))+" minutes. Choose a slower one.")
	case errors.Is(err, janitor.ErrInvalidSchedule):
		return httpx.Problem(http.StatusUnprocessableEntity, problemInvalidSchedule,
			"That is not a schedule TierPlan can use. Write five fields, such as 0 3 * * *, and give a preset only together with its schedule.")
	case errors.Is(err, janitor.ErrOutOfRange):
		return httpx.Problem(http.StatusUnprocessableEntity, problemPolicyOutOfRange,
			fmt.Sprintf("Keep must be %d to %d containers and the grace period %d to %d hours.",
				janitor.MinKeepN, janitor.MaxKeepN, janitor.MinGraceHours, janitor.MaxGraceHours))
	}
	return nil
}

// janitorPolicyFailure logs a failed read or write of the policy and answers it as an internal error.
func janitorPolicyFailure(d JanitorDeps, envID, what string, err error) error {
	d.log().Error("janitor policy request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// changedPolicyFields names, in a fixed order, the fields whose stored value differs between two sets of overrides.
func changedPolicyFields(old, now janitor.Overrides) []string {
	var out []string
	add := func(name string, changed bool) {
		if changed {
			out = append(out, name)
		}
	}
	add("enabled", !samePtr(old.Enabled, now.Enabled))
	add("schedule", !samePtr(old.Schedule, now.Schedule) || !samePtr(old.SchedulePreset, now.SchedulePreset))
	add("keepN", !samePtr(old.KeepN, now.KeepN))
	add("graceHours", !samePtr(old.GraceHours, now.GraceHours))
	add("removeUnusedNetworks", !samePtr(old.RemoveUnusedNetworks, now.RemoveUnusedNetworks))
	add("removeStoppedContainers", !samePtr(old.RemoveStoppedContainers, now.RemoveStoppedContainers))
	return out
}

// samePtr is true when both are nil or both point at equal values.
func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// setJanitorPolicy saves the overrides of an environment. The values are checked first; then the old
// overrides are read, the new ones written and the audit row recorded in one transaction. A save that
// changes nothing writes nothing.
func setJanitorPolicy(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID string, in *JanitorPolicyInput) (JanitorPolicyBody, error) {
	want := in.Body.overrides()
	if err := janitor.Validate(want); err != nil {
		if prob := janitorPolicyValueProblem(err); prob != nil {
			return JanitorPolicyBody{}, prob
		}
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "validate", err)
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	old, _, err := d.Policies.EnvOn(ctx, q, envID)
	if err != nil {
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "read the policy", err)
	}
	changed, err := d.Policies.SetEnv(ctx, q, envID, want, in.Body.Version, p.ID, d.now())
	switch {
	case errors.Is(err, janitor.ErrPolicyVersion):
		return JanitorPolicyBody{}, settingsChanged(lastJanitorPolicyAuthor(ctx, d, q, envID))
	case err != nil:
		if prob := janitorPolicyValueProblem(err); prob != nil {
			return JanitorPolicyBody{}, prob
		}
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "save the policy", err)
	}
	if changed {
		if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionJanitorPolicyChanged, TargetType: auditTargetEnvironment, TargetID: envID, EnvID: envID,
			Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{
				"fields":  strings.Join(changedPolicyFields(old, want), ","),
				"version": strconv.FormatInt(in.Body.Version+1, 10),
			},
		}); err != nil {
			return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "audit", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "commit", err)
	}
	body, err := janitorPolicyBody(ctx, d, envID)
	if err != nil {
		return JanitorPolicyBody{}, janitorPolicyFailure(d, envID, "read the policy", err)
	}
	return body, nil
}

// lastJanitorPolicyAuthor is the display name of whoever saved the environment's policy last, empty when
// nobody did or that person is gone.
func lastJanitorPolicyAuthor(ctx context.Context, d JanitorDeps, q *sqlc.Queries, envID string) string {
	row, err := q.GetJanitorEnvPolicy(ctx, envID)
	if err != nil || row.UpdatedBy == "" {
		return ""
	}
	return keeperName(ctx, AddressSpaceDeps{Store: d.Store}, map[string]string{}, row.UpdatedBy)
}
