// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/updates"
)

// When an environment is checked for image updates (plan 12-11, IMG-04, D-01, D-10, D-22): each field follows
// the instance default until the environment sets it, and a save is checked, versioned and audited. Schedules
// are read in UTC in these tests, and the clock is the fixture's, which stands at 2026-01-01 00:00 UTC.

type updateCheckStatusAnswer struct {
	Enabled  policyBool `json:"enabled"`
	Schedule struct {
		Value      string `json:"value"`
		Preset     string `json:"preset"`
		Overridden bool   `json:"overridden"`
	} `json:"schedule"`
	Timezone      string  `json:"timezone"`
	LastCheckedAt string  `json:"lastCheckedAt"`
	NextCheckAt   *string `json:"nextCheckAt"`
	Version       int64   `json:"version"`
}

// updatePolicyJSON is a save body that sets nothing but what is given: every other field is null.
func updatePolicyJSON(version int64, fields map[string]any) string {
	body := map[string]any{"version": version, "enabled": nil, "schedule": nil, "preset": nil}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *envFix) readUpdateCheckStatus(t *testing.T, e envBody) updateCheckStatusAnswer {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/updates", "")
	if rec.code != http.StatusOK {
		t.Fatalf("update check status: %d %s", rec.code, rec.body)
	}
	return decode[updateCheckStatusAnswer](t, rec)
}

func (f *envFix) putUpdatePolicy(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/updates/policy", body)
}

// updatePolicyEnv is an environment whose update check routes are built, with no registry to ask.
func updatePolicyEnv(t *testing.T) (*envFix, envBody) {
	t.Helper()
	return dashEnv(t, dockertest.FakeConfig{Hostname: "upd"})
}

// nextCheckOf is the time of nextCheckAt, which must be present.
func nextCheckOf(t *testing.T, st updateCheckStatusAnswer) time.Time {
	t.Helper()
	if st.NextCheckAt == nil {
		t.Fatalf("status = %+v, want a next check", st)
	}
	at, err := time.Parse(time.RFC3339, *st.NextCheckAt)
	if err != nil {
		t.Fatalf("nextCheckAt %q: %v", *st.NextCheckAt, err)
	}
	return at
}

// The tracer (plan 12-11, IMG-04, D-01, D-10): an environment follows the default of every 6 hours; its Admin
// moves it to daily at 03:00 and the status shows the change at once, with the next check at 03:00 plus the
// environment's own offset. An Operator is refused, and the save is one audit row.
func TestUpdatePolicyOverrideMovesNextCheck(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)

	before := f.readUpdateCheckStatus(t, e)
	if before.Version != 0 || !before.Enabled.Value || before.Enabled.Overridden || before.Schedule.Value != updates.DefaultSchedule ||
		before.Schedule.Preset != "every_6h" || before.Schedule.Overridden || before.Timezone == "" || before.LastCheckedAt != "" {
		t.Fatalf("status before = %+v, want the default every 6 hours, nothing overridden, version 0, never checked", before)
	}
	// 00:00 is the slot at the fixture's now: this environment's offset puts its check just after it, or it is the 06:00 slot.
	if at := nextCheckOf(t, before); (at.Hour() != 0 && at.Hour() != 6) || at.Day() != 1 {
		t.Fatalf("next check before = %s, want the 00:00 or the 06:00 slot of 1 January", at)
	}

	// An Operator may read the status and may not change it.
	f.addUser(t, "olive", false)
	f.addEnvRole(t, "olive", e.ID, "operator")
	cookie, csrf := f.signIn(t, "olive", false)
	path := "/api/env/" + e.ID + "/updates/policy"
	body := updatePolicyJSON(0, map[string]any{"schedule": "0 3 * * *", "preset": "daily"})
	if rec := f.call(t, authReq{method: http.MethodPut, path: path, body: body, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator saving the schedule: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/updates", cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("an operator reading the status: %d %s, want 200", rec.Code, rec.Body)
	}
	if got := f.readUpdateCheckStatus(t, e); got.Version != 0 || got.Schedule.Overridden {
		t.Fatalf("status after the refusal = %+v, want it untouched", got)
	}
	if rows := f.auditRows(t, "updates.policy_changed"); len(rows) != 0 {
		t.Fatalf("audit rows after a refusal = %+v, want none", rows)
	}

	rec := f.putUpdatePolicy(t, e, body)
	if rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	saved := decode[updateCheckStatusAnswer](t, rec)
	read := f.readUpdateCheckStatus(t, e)
	for name, got := range map[string]updateCheckStatusAnswer{"the save answer": saved, "the status": read} {
		if got.Version != 1 || !got.Schedule.Overridden || got.Schedule.Value != "0 3 * * *" || got.Schedule.Preset != "daily" || got.Enabled.Overridden || !got.Enabled.Value {
			t.Errorf("%s = %+v, want the daily schedule overridden at version 1 and the on/off still followed", name, got)
		}
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want, err := schedule.NextRun("0 3 * * *", e.ID, now, time.UTC, updates.MinInterval)
	if err != nil {
		t.Fatal(err)
	}
	got := nextCheckOf(t, read)
	if !got.Equal(want) || got.Hour() != 3 || got.Minute() >= 10 || got.Day() != 1 {
		t.Errorf("next check = %s, want %s (03:00 plus an offset under 10 minutes)", got, want)
	}

	rows := f.auditRows(t, "updates.policy_changed")
	if len(rows) != 1 || rows[0].TargetType != "environment" || rows[0].TargetID != e.ID || rows[0].EnvID != e.ID || rows[0].Result != "success" ||
		rows[0].Details["fields"] != "schedule" || rows[0].Details["version"] != "1" {
		t.Fatalf("audit rows = %+v, want one environment row naming the schedule at version 1", rows)
	}
}

type updateDefaultsAnswer struct {
	Enabled   bool   `json:"enabled"`
	Schedule  string `json:"schedule"`
	Preset    string `json:"preset"`
	Followers int    `json:"followers"`
	Version   int64  `json:"version"`
	Timezone  string `json:"timezone"`
}

// updateDefaultsJSON is a save body: the defaults as they were read, with the fields given changed.
func updateDefaultsJSON(cur updateDefaultsAnswer, fields map[string]any) string {
	body := map[string]any{"version": cur.Version, "enabled": cur.Enabled, "schedule": cur.Schedule, "preset": cur.Preset}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *envFix) readUpdateDefaults(t *testing.T) updateDefaultsAnswer {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/settings/update-checks", "")
	if rec.code != http.StatusOK {
		t.Fatalf("update check defaults: %d %s", rec.code, rec.body)
	}
	return decode[updateDefaultsAnswer](t, rec)
}

// The instance defaults (plan 12-11, D-01, D-22): seeded to every 6 hours, read with settings.read, changed with
// settings.write by an instance admin and not by an environment's Admin, and one audit row for a real change.
func TestUpdateDefaultsReadAndWrite(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)

	cur := f.readUpdateDefaults(t)
	if cur.Version != 1 || !cur.Enabled || cur.Schedule != "0 */6 * * *" || cur.Preset != "every_6h" || cur.Followers != 1 || cur.Timezone == "" {
		t.Fatalf("defaults = %+v, want the seeded every 6 hours at version 1, followed by the one environment", cur)
	}

	// An Admin of the environment may not change the instance default, nor read it: both are the instance admin's.
	f.addUser(t, "enid", false)
	f.addEnvRole(t, "enid", e.ID, "admin")
	cookie, csrf := f.signIn(t, "enid", false)
	save := updateDefaultsJSON(cur, map[string]any{"schedule": "0 3 * * *", "preset": "daily"})
	if rec := f.call(t, authReq{method: http.MethodPut, path: "/api/settings/update-checks", body: save, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Fatalf("an environment admin saving the defaults: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/settings/update-checks", cookie: cookie}); rec.Code != http.StatusForbidden {
		t.Fatalf("an environment admin reading the defaults: %d %s, want 403", rec.Code, rec.Body)
	}
	if got := f.readUpdateDefaults(t); got != cur {
		t.Fatalf("defaults = %+v after the refusals, want them untouched: %+v", got, cur)
	}
	if rows := f.auditRows(t, "updates.defaults_changed"); len(rows) != 0 {
		t.Fatalf("audit rows after the refusals = %+v, want none", rows)
	}

	rec := f.req(t, http.MethodPut, "/api/settings/update-checks", save)
	if rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	saved := decode[updateDefaultsAnswer](t, rec)
	if saved.Version != 2 || saved.Schedule != "0 3 * * *" || saved.Preset != "daily" || !saved.Enabled || saved != f.readUpdateDefaults(t) {
		t.Errorf("saved = %+v, want the daily default at version 2, as it reads back", saved)
	}
	rows := f.auditRows(t, "updates.defaults_changed")
	if len(rows) != 1 || rows[0].TargetType != "settings" || rows[0].EnvID != "" || rows[0].Result != "success" ||
		rows[0].Details["fields"] != "schedule" || rows[0].Details["version"] != "2" {
		t.Fatalf("audit rows = %+v, want one instance row naming the schedule at version 2", rows)
	}
}

// An environment that follows the default moves with it, and a field it sets itself does not (D-01).
func TestUpdateDefaultsInherited(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)
	// The environment sets only whether it is checked, and follows the schedule.
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(0, map[string]any{"enabled": false})); rec.code != http.StatusOK {
		t.Fatalf("override: %d %s", rec.code, rec.body)
	}
	if got := f.readUpdateDefaults(t); got.Followers != 1 {
		t.Fatalf("followers = %d, want 1: the environment still follows the schedule", got.Followers)
	}
	before := f.readUpdateCheckStatus(t, e)
	if before.Enabled != (policyBool{Value: false, Overridden: true}) || before.Schedule.Overridden || before.Schedule.Value != "0 */6 * * *" || before.NextCheckAt != nil {
		t.Fatalf("status before = %+v, want switched off by itself and following every 6 hours, with no next check", before)
	}

	cur := f.readUpdateDefaults(t)
	if rec := f.req(t, http.MethodPut, "/api/settings/update-checks", updateDefaultsJSON(cur, map[string]any{"enabled": true, "schedule": "0 3 * * *", "preset": "daily"})); rec.code != http.StatusOK {
		t.Fatalf("change the defaults: %d %s", rec.code, rec.body)
	}
	after := f.readUpdateCheckStatus(t, e)
	if after.Enabled != (policyBool{Value: false, Overridden: true}) || after.Schedule.Value != "0 3 * * *" || after.Schedule.Preset != "daily" || after.Schedule.Overridden ||
		after.Version != before.Version || after.NextCheckAt != nil {
		t.Fatalf("status after = %+v, want the schedule moved with the default and the environment's own off kept", after)
	}
	// No row of the environment was written by the change of the default.
	if n := f.countRows(t, "image_update_env_policy"); n != 1 {
		t.Errorf("environment policy rows = %d, want 1", n)
	}

	// Once the environment sets the schedule too, nothing follows the default any more.
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(1, map[string]any{"enabled": false, "schedule": "0 4 * * *", "preset": "daily"})); rec.code != http.StatusOK {
		t.Fatalf("override the schedule: %d %s", rec.code, rec.body)
	}
	if got := f.readUpdateDefaults(t); got.Followers != 0 {
		t.Errorf("followers = %d, want 0", got.Followers)
	}
}

func (f *envFix) countRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A schedule that cannot be used, that fires too often, or a save made from a stale read is refused and writes
// nothing (T-12-26, T-12-28).
func TestUpdatePolicyRefusals(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)
	for name, c := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"every half hour":       {map[string]any{"schedule": "*/30 * * * *"}, "schedule_too_frequent"},
		"every minute":          {map[string]any{"schedule": "* * * * *", "preset": "custom"}, "schedule_too_frequent"},
		"nonsense":              {map[string]any{"schedule": "nonsense"}, "invalid_schedule"},
		"four fields":           {map[string]any{"schedule": "0 3 * *"}, "invalid_schedule"},
		"@every":                {map[string]any{"schedule": "@every 2h"}, "invalid_schedule"},
		"a preset with no time": {map[string]any{"preset": "daily"}, "invalid_schedule"},
	} {
		rec := f.putUpdatePolicy(t, e, updatePolicyJSON(0, c.fields))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != c.want {
			t.Errorf("%s: %d %s (%s), want 422 %s", name, rec.code, rec.body, rec.ptyp, c.want)
		}
	}
	// One hour is the floor and is accepted; the 100 characters of a schedule are the limit.
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(0, map[string]any{"schedule": "0 * * * *", "preset": "hourly"})); rec.code != http.StatusOK {
		t.Errorf("hourly: %d %s, want 200", rec.code, rec.body)
	}
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(1, map[string]any{"schedule": strings.Repeat("*", 101)})); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a 101 character schedule: %d %s, want 422", rec.code, rec.body)
	}

	// The stale read: the stored version is 1 now (the hourly save above), and a save at 0 is refused.
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(0, map[string]any{"enabled": false})); rec.code != http.StatusConflict || rec.ptyp != "settings_changed" {
		t.Errorf("a save at version 0 when the stored is 1: %d %s (%s), want 409 settings_changed", rec.code, rec.body, rec.ptyp)
	}
	if got := f.readUpdateCheckStatus(t, e); got.Version != 1 || got.Enabled.Overridden || got.Schedule.Value != "0 * * * *" {
		t.Errorf("status = %+v, want only the hourly save, at version 1", got)
	}
	if rows := f.auditRows(t, "updates.policy_changed"); len(rows) != 1 {
		t.Errorf("audit rows = %d, want only the hourly save's", len(rows))
	}

	// The same refusals for the instance default.
	cur := f.readUpdateDefaults(t)
	for name, c := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"default every half hour": {map[string]any{"schedule": "*/30 * * * *"}, "schedule_too_frequent"},
		"default nonsense":        {map[string]any{"schedule": "nonsense"}, "invalid_schedule"},
	} {
		rec := f.req(t, http.MethodPut, "/api/settings/update-checks", updateDefaultsJSON(cur, c.fields))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != c.want {
			t.Errorf("%s: %d %s (%s), want 422 %s", name, rec.code, rec.body, rec.ptyp, c.want)
		}
	}
	stale := cur
	stale.Version = 7
	if rec := f.req(t, http.MethodPut, "/api/settings/update-checks", updateDefaultsJSON(stale, map[string]any{"enabled": false})); rec.code != http.StatusConflict || rec.ptyp != "settings_changed" {
		t.Errorf("a default saved at version 7: %d %s (%s), want 409 settings_changed", rec.code, rec.body, rec.ptyp)
	}
	if got := f.readUpdateDefaults(t); got != cur {
		t.Errorf("defaults = %+v, want them untouched: %+v", got, cur)
	}
	if rows := f.auditRows(t, "updates.defaults_changed"); len(rows) != 0 {
		t.Errorf("default audit rows = %+v, want none", rows)
	}
}

// A save that changes nothing answers 200 and writes no audit row, for an environment and for the default.
func TestUpdatePolicyNoChangeNoAudit(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)

	// Nothing set on an environment that sets nothing.
	rec := f.putUpdatePolicy(t, e, updatePolicyJSON(0, nil))
	if rec.code != http.StatusOK {
		t.Fatalf("an empty save: %d %s", rec.code, rec.body)
	}
	if got := decode[updateCheckStatusAnswer](t, rec); got.Version != 0 {
		t.Errorf("version = %d after saving nothing, want 0", got.Version)
	}
	// A change, then the same change again.
	body := updatePolicyJSON(0, map[string]any{"schedule": "0 3 * * *", "preset": "daily"})
	if rec := f.putUpdatePolicy(t, e, body); rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	again := updatePolicyJSON(1, map[string]any{"schedule": "0 3 * * *", "preset": "daily"})
	rec = f.putUpdatePolicy(t, e, again)
	if rec.code != http.StatusOK || decode[updateCheckStatusAnswer](t, rec).Version != 1 {
		t.Fatalf("the same save again: %d %s, want 200 and the version still 1", rec.code, rec.body)
	}
	// A schedule with no preset is the custom preset, so saying custom is the same save.
	if rec := f.putUpdatePolicy(t, e, updatePolicyJSON(1, map[string]any{"schedule": "0 3 * * *", "preset": "custom"})); rec.code != http.StatusOK {
		t.Fatalf("a changed preset: %d %s", rec.code, rec.body)
	}
	if rows := f.auditRows(t, "updates.policy_changed"); len(rows) != 2 || rows[0].Details["version"] != "1" || rows[1].Details["version"] != "2" {
		t.Fatalf("audit rows = %+v, want one for the schedule and one for its new preset", rows)
	}

	cur := f.readUpdateDefaults(t)
	if rec := f.req(t, http.MethodPut, "/api/settings/update-checks", updateDefaultsJSON(cur, nil)); rec.code != http.StatusOK || decode[updateDefaultsAnswer](t, rec).Version != cur.Version {
		t.Fatalf("the defaults saved as read: %d %s, want 200 and the version unchanged", rec.code, rec.body)
	}
	if rows := f.auditRows(t, "updates.defaults_changed"); len(rows) != 0 {
		t.Errorf("default audit rows = %+v, want none", rows)
	}
}

type updateSchedulePreviewAnswer struct {
	Valid    bool     `json:"valid"`
	Problem  string   `json:"problem"`
	NextRuns []string `json:"nextRuns"`
	Timezone string   `json:"timezone"`
}

// The preview answers what a schedule would do before it is saved: the next three checks with this environment's
// offset, or why it is not accepted. It writes nothing.
func TestUpdateSchedulePreview(t *testing.T) {
	t.Parallel()
	f, e := updatePolicyEnv(t)
	awaitOnline(t, f.mon, e.ID)
	preview := func(spec string) (*envRec, updateSchedulePreviewAnswer) {
		t.Helper()
		b, err := json.Marshal(map[string]string{"schedule": spec})
		if err != nil {
			t.Fatal(err)
		}
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/schedule-preview", string(b))
		var got updateSchedulePreviewAnswer
		if rec.code == http.StatusOK {
			got = decode[updateSchedulePreviewAnswer](t, rec)
		}
		return rec, got
	}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want, err := schedule.NextRuns("0 3 * * *", e.ID, now, time.UTC, 3, updates.MinInterval)
	if err != nil {
		t.Fatal(err)
	}
	rec, got := preview("0 3 * * *")
	if rec.code != http.StatusOK || !got.Valid || got.Problem != "" || got.Timezone != "UTC" || len(got.NextRuns) != 3 {
		t.Fatalf("preview: %d %s, want valid with three runs in UTC", rec.code, rec.body)
	}
	var prev time.Time
	for i, raw := range got.NextRuns {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil || !at.Equal(want[i]) || at.Hour() != 3 {
			t.Errorf("run %d = %q (%v), want %v: 03:00 with this environment's offset", i, raw, err, want[i])
		}
		if i > 0 && at.Sub(prev) != 24*time.Hour {
			t.Errorf("run %d is %v after the one before, want a day", i, at.Sub(prev))
		}
		prev = at
	}

	for _, c := range []struct{ spec, problem string }{
		{"* * * * *", "too_frequent"}, {"*/30 * * * *", "too_frequent"},
		{"bad", "invalid"}, {"", "invalid"}, {"@every 2h", "invalid"}, {"0 3 * *", "invalid"},
	} {
		rec, got := preview(c.spec)
		if rec.code != http.StatusOK {
			t.Fatalf("preview of %q: %d %s", c.spec, rec.code, rec.body)
		}
		if got.Valid || got.Problem != c.problem || len(got.NextRuns) != 0 || got.Timezone == "" {
			t.Errorf("preview of %q = %+v, want not valid, %s, no runs and the zone named", c.spec, got, c.problem)
		}
	}
	if rec, _ := preview(strings.Repeat("*", 101)); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a 101 character schedule: %d, want 422", rec.code)
	}
	if got := f.readUpdateCheckStatus(t, e); got.Version != 0 || got.Schedule.Overridden {
		t.Errorf("status = %+v, want the previews to have saved nothing", got)
	}
	// A Viewer reads the environment, so may preview.
	f.addUser(t, "vera", false)
	f.addEnvRole(t, "vera", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "vera", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/updates/schedule-preview", body: `{"schedule":"0 3 * * *"}`, cookie: cookie, csrf: csrf}); rec.Code != http.StatusOK {
		t.Errorf("a viewer previewing: %d %s, want 200", rec.Code, rec.Body)
	}
}
