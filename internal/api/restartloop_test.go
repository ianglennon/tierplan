// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
)

// Muting a restart loop and the per-environment threshold (plan 06-35, RES-13, D-14, D-16).
// The services, tasks and clock are loopEnv's; every write is the real route over the real
// store.

type loopCaller func(method, path, body string) *envRec

// loopAs signs a user with the role in and returns a caller for the environment's routes.
func loopAs(t *testing.T, f *envFix, e envBody, user, role string) loopCaller {
	t.Helper()
	f.addUser(t, user, false)
	f.addEnvRole(t, user, e.ID, role)
	cookie, csrf := f.signIn(t, user, false)
	return func(method, path, body string) *envRec {
		rec := f.call(t, authReq{method: method, path: "/api/env/" + e.ID + "/" + path, body: body, cookie: cookie, csrf: csrf})
		ptyp := ""
		if rec.Code >= 400 {
			ptyp = problemType(t, rec)
		}
		return &envRec{rec.Code, rec.Body.Bytes(), ptyp}
	}
}

// auditRows are the audit rows of one action, oldest first, with their details.
func (f *envFix) auditRows(t *testing.T, action string) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var raw string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &raw); err != nil {
			t.Fatal(err)
		}
		r.Details = map[string]string{}
		if err := json.Unmarshal([]byte(raw), &r.Details); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type muteRows struct {
	ID, Name, MutedBy string
}

func (f *envFix) mutes(t *testing.T, e envBody) []muteRows {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT target_id, name, muted_by FROM restart_loop_mutes WHERE env_id = ? ORDER BY target_id`, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []muteRows
	for rows.Next() {
		var m muteRows
		if err := rows.Scan(&m.ID, &m.Name, &m.MutedBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestRestartLoopMuteMutesAFlaggedServiceAuditsItAndTheListAndDetailShowIt(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")

	rec := op(http.MethodPut, "services/svc-web/restart-loop-mute", "")
	if rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 1 || got[0].ID != "svc-web" || got[0].Name != "web" || got[0].MutedBy != "id-loop-operator" {
		t.Fatalf("mutes = %+v, want web, by the operator", got)
	}
	rows := f.auditRows(t, audit.ActionRestartLoopMuted)
	if len(rows) != 1 || rows[0].TargetID != "svc-web" || rows[0].EnvID != e.ID || rows[0].Result != string(audit.ResultSuccess) ||
		rows[0].Details["name"] != "web" || rows[0].Details["kind"] != "service" {
		t.Fatalf("audit rows = %+v, want one restart_loop.muted for web with its name and kind", rows)
	}

	// The list and the detail say so at once, without waiting for the next cluster read.
	_, list := listServicesQuery(t, f, e, "")
	for _, it := range list.Items {
		if it.Name == "web" && (it.RestartLoop == nil || it.RestartLoop.State != "muted" || it.RestartLoop.Count != 3) {
			t.Errorf("web in the list = %+v, want a muted loop that still counts its 3 failures", it.RestartLoop)
		}
	}
	detail := decode[struct {
		Muted       bool             `json:"muted"`
		RestartLoop *restartLoopBody `json:"restartLoop"`
	}](t, f.get(t, e, "services/svc-web"))
	if !detail.Muted || detail.RestartLoop == nil || detail.RestartLoop.State != "muted" {
		t.Errorf("web detail = muted %v, loop %+v, want muted", detail.Muted, detail.RestartLoop)
	}
	if other := decode[struct {
		Muted bool `json:"muted"`
	}](t, f.get(t, e, "services/svc-api")); other.Muted {
		t.Error("api reads as muted though nobody muted it")
	}
}

func TestRestartLoopMuteTwiceChangesNothingAndWritesNoSecondRowOrAudit(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	for i := range 2 {
		if rec := op(http.MethodPut, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
			t.Fatalf("mute %d = %d %s, want 200 both times", i+1, rec.code, rec.body)
		}
	}
	if got := f.mutes(t, e); len(got) != 1 {
		t.Errorf("mutes = %+v, want one row", got)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopMuted); len(rows) != 1 {
		t.Errorf("audit rows = %+v, want only the first mute recorded", rows)
	}
}

func TestRestartLoopMuteTakesTheServiceByNameToo(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	if rec := op(http.MethodPut, "services/web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Fatalf("mute by name = %d %s", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 1 || got[0].ID != "svc-web" {
		t.Errorf("mutes = %+v, want the row keyed by the service ID", got)
	}
}

func TestRestartLoopMuteOfAnUnknownServiceIs404AndWritesNothing(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	rec := op(http.MethodPut, "services/svc-nope/restart-loop-mute", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Fatalf("mute = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rec := op(http.MethodDelete, "services/svc-nope/restart-loop-mute", ""); rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Errorf("unmute = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none", got)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopMuted); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a service that is not there", rows)
	}
}

func TestRestartLoopUnmuteRemovesTheMuteAuditsItAndRepeatingItChangesNothing(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	if rec := op(http.MethodPut, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if rec := op(http.MethodDelete, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Fatalf("unmute = %d %s", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none", got)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopUnmuted); len(rows) != 1 || rows[0].TargetID != "svc-web" || rows[0].Details["name"] != "web" {
		t.Fatalf("audit rows = %+v, want one restart_loop.unmuted for web", rows)
	}
	if rec := op(http.MethodDelete, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Errorf("second unmute = %d %s, want 200", rec.code, rec.body)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopUnmuted); len(rows) != 1 {
		t.Errorf("audit rows = %+v, want the no-op not recorded", rows)
	}
	detail := decode[struct {
		Muted       bool             `json:"muted"`
		RestartLoop *restartLoopBody `json:"restartLoop"`
	}](t, f.get(t, e, "services/svc-web"))
	if detail.Muted || detail.RestartLoop == nil || detail.RestartLoop.State != "flagged" {
		t.Errorf("web detail after the unmute = muted %v, loop %+v, want flagged again", detail.Muted, detail.RestartLoop)
	}
}

func TestRestartLoopMuteNeedsServicesControlAndTheMuteIsPerEnvironment(t *testing.T) {
	f, e := loopEnv(t)
	viewer := loopAs(t, f, e, "loop-viewer", "viewer")
	if rec := viewer(http.MethodPut, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusForbidden {
		t.Errorf("a Viewer's mute = %d %s, want 403", rec.code, rec.body)
	}
	if rec := viewer(http.MethodDelete, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusForbidden {
		t.Errorf("a Viewer's unmute = %d %s, want 403", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none from a Viewer", got)
	}
	// A mute row of another environment's service ID does not mute this one's.
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, name, kind, enabled, created_at, updated_at) VALUES ('other-env', 'other', 'local', 0, 1, 1)`); err == nil {
		if _, err := f.st.Write.Exec(`INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at) VALUES ('other-env', 'service', 'svc-web', 'web', 'x', 1)`); err != nil {
			t.Fatal(err)
		}
		_, list := listServicesQuery(t, f, e, "")
		for _, it := range list.Items {
			if it.Name == "web" && it.RestartLoop != nil && it.RestartLoop.State == "muted" {
				t.Error("a mute of another environment muted this one's service")
			}
		}
	}
}

type loopSettings struct {
	Failures      int `json:"failures"`
	WindowMinutes int `json:"windowMinutes"`
	HistoryLimit  int `json:"historyLimit"`
}

func TestRestartLoopSettingsAnswerTheDefaultsAndTheHistoryLimitOfTheLastRead(t *testing.T) {
	f, e := loopEnv(t)
	admin := loopAs(t, f, e, "loop-admin", "admin")
	listServicesQuery(t, f, e, "") // the first cluster read, which learns Docker's history limit
	rec := admin(http.MethodGet, "restart-loop-settings", "")
	if rec.code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.code, rec.body)
	}
	got := decode[loopSettings](t, rec)
	if got.Failures != 3 || got.WindowMinutes != 10 || got.HistoryLimit != 5 {
		t.Errorf("settings = %+v, want 3 in 10 minutes and the engine's history limit 5", got)
	}
}

func TestRestartLoopSettingsChangeIsStoredUsedAndAuditedWithOldAndNewValues(t *testing.T) {
	f, e := loopEnv(t)
	admin := loopAs(t, f, e, "loop-admin", "admin")
	rec := admin(http.MethodPut, "restart-loop-settings", `{"failures":2,"windowMinutes":20}`)
	if rec.code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.code, rec.body)
	}
	if got := decode[loopSettings](t, rec); got.Failures != 2 || got.WindowMinutes != 20 {
		t.Errorf("answer = %+v, want the new values", got)
	}
	var failures, window int
	if err := f.st.Read.QueryRow(`SELECT restart_loop_failures, restart_loop_window_minutes FROM environments WHERE id = ?`, e.ID).Scan(&failures, &window); err != nil {
		t.Fatal(err)
	}
	if failures != 2 || window != 20 {
		t.Errorf("stored = %d in %d minutes, want 2 in 20", failures, window)
	}
	rows := f.auditRows(t, audit.ActionEnvironmentRestartLoopSettingsChanged)
	if len(rows) != 1 || rows[0].EnvID != e.ID || rows[0].TargetID != e.ID {
		t.Fatalf("audit rows = %+v, want one environment.restart_loop_settings_changed", rows)
	}
	for k, want := range map[string]string{"failures": "2", "window_minutes": "20", "previous_failures": "3", "previous_window_minutes": "10"} {
		if rows[0].Details[k] != want {
			t.Errorf("details[%s] = %q, want %q", k, rows[0].Details[k], want)
		}
	}
}

func TestRestartLoopSettingsSavedWithNothingChangedIs200AndNotAudited(t *testing.T) {
	f, e := loopEnv(t)
	admin := loopAs(t, f, e, "loop-admin", "admin")
	rec := admin(http.MethodPut, "restart-loop-settings", `{"failures":3,"windowMinutes":10}`)
	if rec.code != http.StatusOK {
		t.Fatalf("put = %d %s", rec.code, rec.body)
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentRestartLoopSettingsChanged); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a save that changed nothing", rows)
	}
}

func TestRestartLoopSettingsRefuseValuesOutsideTheBounds(t *testing.T) {
	f, e := loopEnv(t)
	admin := loopAs(t, f, e, "loop-admin", "admin")
	for _, tc := range []struct{ body, code string }{
		{`{"failures":1,"windowMinutes":10}`, "failures_invalid"},
		{`{"failures":51,"windowMinutes":10}`, "failures_invalid"},
		{`{"failures":3,"windowMinutes":0}`, "window_invalid"},
		{`{"failures":3,"windowMinutes":121}`, "window_invalid"},
	} {
		rec := admin(http.MethodPut, "restart-loop-settings", tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.code {
			t.Errorf("%s = %d %s (%s), want 422 %s", tc.body, rec.code, rec.body, rec.ptyp, tc.code)
		}
	}
	for _, body := range []string{`{"failures":2,"windowMinutes":1}`, `{"failures":50,"windowMinutes":120}`} {
		if rec := admin(http.MethodPut, "restart-loop-settings", body); rec.code != http.StatusOK {
			t.Errorf("%s = %d %s, want the bounds themselves accepted", body, rec.code, rec.body)
		}
	}
	if rows := f.auditRows(t, audit.ActionEnvironmentRestartLoopSettingsChanged); len(rows) != 2 {
		t.Errorf("audit rows = %d, want only the two accepted changes recorded", len(rows))
	}
}

func TestRestartLoopSettingsNeedEnvironmentsConfigure(t *testing.T) {
	f, e := loopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	if rec := op(http.MethodPut, "restart-loop-settings", `{"failures":2,"windowMinutes":5}`); rec.code != http.StatusForbidden {
		t.Errorf("an Operator's PUT = %d %s, want 403", rec.code, rec.body)
	}
	if rec := op(http.MethodGet, "restart-loop-settings", ""); rec.code != http.StatusForbidden {
		t.Errorf("an Operator's GET = %d %s, want 403", rec.code, rec.body)
	}
	var failures int
	if err := f.st.Read.QueryRow(`SELECT restart_loop_failures FROM environments WHERE id = ?`, e.ID).Scan(&failures); err != nil || failures != 3 {
		t.Errorf("stored failures = %d (%v), want 3 untouched", failures, err)
	}
}

// A new threshold is what the next cluster read uses, for the service already flagged too.
func TestRestartLoopSettingsChangeWhatTheNextReadFlags(t *testing.T) {
	f, e := loopEnv(t)
	admin := loopAs(t, f, e, "loop-admin", "admin")
	// api has two failures; with a threshold of 2 it is a loop, and web stays one.
	if rec := admin(http.MethodPut, "restart-loop-settings", `{"failures":2,"windowMinutes":10}`); rec.code != http.StatusOK {
		t.Fatal(rec.code, string(rec.body))
	}
	if err := f.clusters.Refresh(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := f.clusters.Cached(e.ID)
	var names []string
	for _, l := range st.RestartLoops {
		names = append(names, l.Name)
	}
	if len(names) != 2 || !strings.Contains(strings.Join(names, ","), "api") {
		t.Errorf("loops after the change = %v, want api as well as web", names)
	}
}
