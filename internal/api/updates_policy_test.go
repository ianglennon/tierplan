// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
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
