// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/proto/testsupport"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, enabled, created_at, updated_at) VALUES ('other-env', 'agent', 'other', 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at) VALUES ('other-env', 'service', 'svc-web', 'web', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	_, list := listServicesQuery(t, f, e, "")
	for _, it := range list.Items {
		if it.Name == "web" && (it.RestartLoop == nil || it.RestartLoop.State != "flagged") {
			t.Errorf("web = %+v, want a mute of another environment to leave it flagged", it.RestartLoop)
		}
	}
}

type loopSettings struct {
	Failures      int `json:"failures"`
	WindowMinutes int `json:"windowMinutes"`
	HistoryLimit  int `json:"historyLimit"`
}

func TestRestartLoopSettingsAnswerTheDefaultsAndTheHistoryLimitOfTheLastRead(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Muting the alerts of a standalone container in a restart loop (plan 06-36, RES-13, D-15,
// D-16, A12). The container is keyed by its node and its name. Its deaths are die events put
// into the environment's feed, the cluster reader reads them, and every write is the real
// route over the real store.

const (
	loopCrashyID = "c1c1c1c1c1c1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	loopCalmID   = "c2c2c2c2c2c2bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	loopTaskID   = "c3c3c3c3c3c3cccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// containerLoopEnv is a standalone host with crashy, which died three times with exit code 3
// in the last three minutes and restarts always, calm, which died as often with exit code 0,
// and the task container of a swarm service, which died four times. Only crashy is a loop.
func containerLoopEnv(t *testing.T) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: loopCrashyID, Names: []string{"/crashy"}, Image: "busybox", State: "restarting", Status: "Restarting (3) 2 seconds ago"},
			{ID: loopCalmID, Names: []string{"/calm"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: loopTaskID, Names: []string{"/web.1.abcdef"}, Image: "nginx:1.27", State: "restarting", Status: "Restarting (1) 2 seconds ago",
				Labels: map[string]string{"com.docker.swarm.task.id": "abcdef", "com.docker.swarm.service.name": "web"}},
		},
		RestartPolicies: map[string]string{loopCrashyID: "always", loopCalmID: "always", loopTaskID: "always"},
	})
	log := slog.New(slog.DiscardHandler)
	f.hub = envs.NewHub(f.mon, f.snaps, log, f.clk.now)
	t.Cleanup(f.hub.Close)
	f.agg = envs.NewAggregator(f.svc, f.snaps, f.hub, f.clk.now)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now, Events: f.hub})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	now := f.clk.now()
	die := func(id, name, containerID, exit string, ago time.Duration) {
		at := now.Add(-ago)
		ev := dockerx.EventRow{
			ID: id, Time: at, TimeNano: at.UnixNano(), Type: "container", Action: "die", ActorID: containerID, Name: name,
			Attributes: map[string]string{"name": name, "exitCode": exit},
		}
		if err := f.hub.Await(testsupport.Context(t), func() bool { return f.hub.Inject(e.ID, ev) }); err != nil {
			t.Fatalf("waiting for the event feed to open: %v", err)
		}
	}
	for i := range 3 {
		ago := time.Duration(i+1) * time.Minute
		die(fmt.Sprintf("crashy-%d", i), "crashy", loopCrashyID, "3", ago)
		die(fmt.Sprintf("calm-%d", i), "calm", loopCalmID, "0", ago)
	}
	for i := range 4 {
		die(fmt.Sprintf("task-%d", i), "web.1.abcdef", loopTaskID, "1", time.Duration(i+1)*time.Minute)
	}
	if err := f.clusters.Refresh(context.Background(), e.ID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return f, e
}

// containerLoopBody is the restartLoop field of a container row and of the detail.
type containerLoopBody struct {
	State         string `json:"state"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"windowMinutes"`
	LastExitCode  string `json:"lastExitCode"`
}

type containerMuteAnswer struct {
	ContainerID string `json:"containerId"`
	Name        string `json:"name"`
	NodeID      string `json:"nodeId"`
	Muted       bool   `json:"muted"`
	Changed     bool   `json:"changed"`
}

func containerMutePath(e envBody, id string) string {
	return "containers/" + id + "/restart-loop-mute?nodeId=" + e.ID
}

// loopOfContainer reads the restartLoop of one container in the list and, if present, the
// detail.
func loopOfContainer(t *testing.T, f *envFix, e envBody, name string) (list *containerLoopBody, detail *containerLoopBody, muted bool) {
	t.Helper()
	var l struct {
		Items []struct {
			Name        string             `json:"name"`
			RestartLoop *containerLoopBody `json:"restartLoop"`
		} `json:"items"`
	}
	rec := f.get(t, e, "containers")
	if rec.code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.code, rec.body)
	}
	if err := json.Unmarshal(rec.body, &l); err != nil {
		t.Fatal(err)
	}
	for _, it := range l.Items {
		if it.Name == name {
			list = it.RestartLoop
		}
	}
	d := decode[struct {
		Muted       bool               `json:"muted"`
		RestartLoop *containerLoopBody `json:"restartLoop"`
	}](t, f.get(t, e, "containers/"+name+"?nodeId="+e.ID))
	return list, d.RestartLoop, d.Muted
}

func TestContainerRestartLoopMuteMutesAFlaggedContainerAuditsItAndTheListAndDetailShowIt(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")

	rec := op(http.MethodPut, containerMutePath(e, "crashy"), "")
	if rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if got := decode[containerMuteAnswer](t, rec); !got.Muted || !got.Changed || got.Name != "crashy" || got.NodeID != e.ID || got.ContainerID != loopCrashyID {
		t.Errorf("answer = %+v, want crashy muted and changed, on this node", got)
	}
	key := e.ID + "/crashy"
	if got := f.mutes(t, e); len(got) != 1 || got[0].ID != key || got[0].Name != "crashy" || got[0].MutedBy != "id-loop-operator" {
		t.Fatalf("mutes = %+v, want crashy keyed by its node and name, by the operator", got)
	}
	var kind string
	if err := f.st.Read.QueryRow(`SELECT kind FROM restart_loop_mutes WHERE env_id = ?`, e.ID).Scan(&kind); err != nil || kind != "container" {
		t.Errorf("mute kind = %q (%v), want container", kind, err)
	}
	rows := f.auditRows(t, audit.ActionRestartLoopMuted)
	if len(rows) != 1 || rows[0].TargetType != "container" || rows[0].TargetID != key || rows[0].EnvID != e.ID || rows[0].Result != string(audit.ResultSuccess) ||
		rows[0].Details["name"] != "crashy" || rows[0].Details["kind"] != "container" || rows[0].Details["node"] != e.ID {
		t.Fatalf("audit rows = %+v, want one restart_loop.muted for crashy with its name, kind container and node", rows)
	}

	// The list and the detail say so at once, without waiting for the next cluster read.
	list, detail, muted := loopOfContainer(t, f, e, "crashy")
	if list == nil || list.State != "muted" || list.Count != 3 {
		t.Errorf("crashy in the list = %+v, want a muted loop that still counts its 3 deaths", list)
	}
	if !muted || detail == nil || detail.State != "muted" {
		t.Errorf("crashy detail = muted %v, loop %+v, want muted", muted, detail)
	}
	if _, _, calmMuted := loopOfContainer(t, f, e, "calm"); calmMuted {
		t.Error("calm reads as muted though nobody muted it")
	}
}

func TestContainerRestartLoopMuteTwiceChangesNothingAndTheContainerCanBeNamedByIDOrName(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	for i, id := range []string{"crashy", loopCrashyID} {
		rec := op(http.MethodPut, containerMutePath(e, id), "")
		if rec.code != http.StatusOK {
			t.Fatalf("mute %d = %d %s, want 200 both times", i+1, rec.code, rec.body)
		}
		if changed := decode[containerMuteAnswer](t, rec).Changed; changed != (i == 0) {
			t.Errorf("mute %d changed = %v, want only the first to", i+1, changed)
		}
	}
	if got := f.mutes(t, e); len(got) != 1 || got[0].ID != e.ID+"/crashy" {
		t.Errorf("mutes = %+v, want one row keyed by the name whichever way it was asked", got)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopMuted); len(rows) != 1 {
		t.Errorf("audit rows = %+v, want only the first mute recorded", rows)
	}
}

func TestContainerRestartLoopUnmuteRemovesTheMuteAuditsItAndRepeatingItChangesNothing(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	if rec := op(http.MethodPut, containerMutePath(e, "crashy"), ""); rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if rec := op(http.MethodDelete, containerMutePath(e, "crashy"), ""); rec.code != http.StatusOK {
		t.Fatalf("unmute = %d %s", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none", got)
	}
	rows := f.auditRows(t, audit.ActionRestartLoopUnmuted)
	if len(rows) != 1 || rows[0].TargetID != e.ID+"/crashy" || rows[0].Details["kind"] != "container" || rows[0].Details["name"] != "crashy" {
		t.Fatalf("audit rows = %+v, want one restart_loop.unmuted for crashy with kind container", rows)
	}
	rec := op(http.MethodDelete, containerMutePath(e, "crashy"), "")
	if rec.code != http.StatusOK || decode[containerMuteAnswer](t, rec).Changed {
		t.Errorf("second unmute = %d %s, want 200 with changed false", rec.code, rec.body)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopUnmuted); len(rows) != 1 {
		t.Errorf("audit rows = %+v, want the no-op not recorded", rows)
	}
	list, _, muted := loopOfContainer(t, f, e, "crashy")
	if muted || list == nil || list.State != "flagged" {
		t.Errorf("crashy after the unmute = muted %v, loop %+v, want flagged again", muted, list)
	}
}

func TestContainerRestartLoopMuteOfAnUnknownContainerOrNodeIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	op := loopAs(t, f, e, "loop-operator", "operator")
	rec := op(http.MethodPut, containerMutePath(e, "nope"), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Fatalf("mute = %d %s (%s), want 404 container_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rec := op(http.MethodDelete, containerMutePath(e, "nope"), ""); rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Errorf("unmute = %d %s (%s), want 404 container_not_found", rec.code, rec.body, rec.ptyp)
	}
	rec = op(http.MethodPut, "containers/crashy/restart-loop-mute?nodeId=no-such-node", "")
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Errorf("mute on an unknown node = %d %s (%s), want 503 node_unavailable", rec.code, rec.body, rec.ptyp)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none", got)
	}
	if rows := f.auditRows(t, audit.ActionRestartLoopMuted); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}

func TestContainerRestartLoopMuteNeedsContainersControlAndTheMuteIsPerEnvironment(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	viewer := loopAs(t, f, e, "loop-viewer", "viewer")
	if rec := viewer(http.MethodPut, containerMutePath(e, "crashy"), ""); rec.code != http.StatusForbidden {
		t.Errorf("a Viewer's mute = %d %s, want 403", rec.code, rec.body)
	}
	if rec := viewer(http.MethodDelete, containerMutePath(e, "crashy"), ""); rec.code != http.StatusForbidden {
		t.Errorf("a Viewer's unmute = %d %s, want 403", rec.code, rec.body)
	}
	if got := f.mutes(t, e); len(got) != 0 {
		t.Errorf("mutes = %+v, want none from a Viewer", got)
	}
	// A mute row of another environment under the same key does not mute this one's container.
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, enabled, created_at, updated_at) VALUES ('other-env', 'agent', 'other', 0, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Write.Exec(`INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at) VALUES ('other-env', 'container', ?, 'crashy', 'x', 1)`, e.ID+"/crashy"); err != nil {
		t.Fatal(err)
	}
	if err := f.clusters.Refresh(context.Background(), e.ID); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := loopOfContainer(t, f, e, "crashy"); list == nil || list.State != "flagged" {
		t.Errorf("crashy = %+v, want a mute of another environment to leave it flagged", list)
	}
}
