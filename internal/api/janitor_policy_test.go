// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The janitor policy of an environment (plan 11-19, JAN-01, D-04, D-15, D-21): each field follows the
// instance default until the environment sets it, and a save is checked, versioned and audited.

type policyBool struct {
	Value      bool `json:"value"`
	Overridden bool `json:"overridden"`
}

type policyInt struct {
	Value      int  `json:"value"`
	Overridden bool `json:"overridden"`
}

type janitorPolicyAnswer struct {
	Version  int64      `json:"version"`
	Timezone string     `json:"timezone"`
	Enabled  policyBool `json:"enabled"`
	Schedule struct {
		Value      string `json:"value"`
		Preset     string `json:"preset"`
		Overridden bool   `json:"overridden"`
	} `json:"schedule"`
	KeepN                   policyInt  `json:"keepN"`
	GraceHours              policyInt  `json:"graceHours"`
	RemoveUnusedNetworks    policyBool `json:"removeUnusedNetworks"`
	RemoveStoppedContainers policyBool `json:"removeStoppedContainers"`
	Defaults                struct {
		Enabled                 bool   `json:"enabled"`
		Schedule                string `json:"schedule"`
		SchedulePreset          string `json:"schedulePreset"`
		KeepN                   int    `json:"keepN"`
		GraceHours              int    `json:"graceHours"`
		RemoveUnusedNetworks    bool   `json:"removeUnusedNetworks"`
		RemoveStoppedContainers bool   `json:"removeStoppedContainers"`
	} `json:"defaults"`
}

// policyJSON is a save body that sets nothing but what is given: every other field is null, so it follows the default.
func policyJSON(version int64, fields map[string]any) string {
	body := map[string]any{
		"version": version, "enabled": nil, "schedule": nil, "schedulePreset": nil, "keepN": nil, "graceHours": nil,
		"removeUnusedNetworks": nil, "removeStoppedContainers": nil,
	}
	for k, v := range fields {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (f *envFix) putJanitorPolicy(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/janitor/policy", body)
}

func (f *envFix) readJanitorPolicy(t *testing.T, e envBody) janitorPolicyAnswer {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor/policy", "")
	if rec.code != http.StatusOK {
		t.Fatalf("policy: %d %s", rec.code, rec.body)
	}
	return decode[janitorPolicyAnswer](t, rec)
}

type policyAudit struct {
	TargetType, TargetID, EnvID, Result string
	Details                             map[string]string
}

func (f *envFix) policyAudits(t *testing.T) []policyAudit {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT target_type, target_id, COALESCE(env_id, ''), result, COALESCE(details, '{}')
		FROM audit_log WHERE action = 'janitor.policy_changed' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []policyAudit
	for rows.Next() {
		var r policyAudit
		var details string
		if err := rows.Scan(&r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The tracer (plan 11-19, JAN-01, D-04, D-11): a stopped docker run container two days old is only
// reported while the environment follows the default, and turning the opt-in on through the API makes
// the next preview list it as removable. The save is one audit row.
func TestJanitorPolicyOptInChangesPreview(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine()
	cfg.Containers = []dockertest.FakeContainer{standaloneBox("scratch", 48*time.Hour, nil)}
	f, e := dashEnv(t, cfg)

	before := f.previewJanitor(t, e)
	if len(before.Items) != 0 || len(before.NotRemoved) != 1 || before.NotRemoved[0].Class != "reported" || before.NotRemoved[0].Reason.Code != "opt_in_off_containers" {
		t.Fatalf("preview before = %+v notRemoved = %+v, want the container reported as opt_in_off_containers", before.Items, before.NotRemoved)
	}
	pol := f.readJanitorPolicy(t, e)
	if pol.Version != 0 || pol.RemoveStoppedContainers.Value || pol.RemoveStoppedContainers.Overridden || !pol.Enabled.Value || pol.KeepN.Value != 2 || pol.GraceHours.Value != 24 {
		t.Fatalf("policy = %+v, want the seeded defaults, nothing overridden, version 0", pol)
	}

	rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"removeStoppedContainers": true}))
	if rec.code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.code, rec.body)
	}
	saved := decode[janitorPolicyAnswer](t, rec)
	if saved.Version != 1 || !saved.RemoveStoppedContainers.Value || !saved.RemoveStoppedContainers.Overridden || saved.KeepN.Overridden || saved.Enabled.Overridden || saved.Schedule.Overridden {
		t.Fatalf("saved = %+v, want version 1 with only the opt-in overridden", saved)
	}

	after := f.previewJanitor(t, e)
	if len(after.Items) != 1 || after.Summary.Containers != 1 || len(after.NotRemoved) != 0 {
		t.Fatalf("preview after = %+v notRemoved = %+v, want the container removable", after.Items, after.NotRemoved)
	}
	if it := after.Items[0]; it.Class != "removable" || it.Reason.Code != "stopped_standalone" || it.Ref != "scratch" {
		t.Errorf("item = %+v, want a removable stopped_standalone container", it)
	}

	rows := f.policyAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	if r := rows[0]; r.TargetType != "environment" || r.TargetID != e.ID || r.EnvID != e.ID || r.Result != "success" ||
		r.Details["fields"] != "removeStoppedContainers" || r.Details["version"] != "1" || len(r.Details) != 2 {
		t.Errorf("audit row = %+v, want the environment, success, fields removeStoppedContainers, version 1 and nothing else", r)
	}
}

// policyProblem is the problem body of a refused save.
type policyProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

// A value outside what the janitor accepts is refused with a 422 and nothing is saved: not a version, not
// an audit row.
func TestJanitorPolicyValidation(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	t.Run("refused", func(t *testing.T) {
		for name, tc := range map[string]struct {
			fields map[string]any
			want   string
		}{
			"keep above 20":           {map[string]any{"keepN": 21}, ""},
			"keep below 0":            {map[string]any{"keepN": -1}, ""},
			"grace 0":                 {map[string]any{"graceHours": 0}, ""},
			"grace above 720":         {map[string]any{"graceHours": 721}, ""},
			"every minute":            {map[string]any{"schedule": "* * * * *"}, "schedule_too_frequent"},
			"every ten minutes":       {map[string]any{"schedule": "*/10 * * * *", "schedulePreset": "custom"}, "schedule_too_frequent"},
			"not a schedule":          {map[string]any{"schedule": "nonsense"}, "invalid_schedule"},
			"six fields":              {map[string]any{"schedule": "0 0 3 * * *"}, "invalid_schedule"},
			"a preset with no cron":   {map[string]any{"schedulePreset": "daily"}, "invalid_schedule"},
			"a preset nobody knows":   {map[string]any{"schedule": "0 3 * * *", "schedulePreset": "fortnightly"}, ""},
			"a bad value beside good": {map[string]any{"enabled": false, "keepN": 3, "graceHours": 900}, ""},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				rec := f.putJanitorPolicy(t, e, policyJSON(0, tc.fields))
				if rec.code != http.StatusUnprocessableEntity {
					t.Fatalf("PUT: %d %s, want 422", rec.code, rec.body)
				}
				if tc.want != "" && rec.ptyp != tc.want {
					t.Errorf("problem type = %q, want %q", rec.ptyp, tc.want)
				}
			})
		}
	})
	// The group returns once all its parallel cases have ended.
	pol := f.readJanitorPolicy(t, e)
	if pol.Version != 0 || pol.KeepN.Overridden || pol.GraceHours.Overridden || pol.Enabled.Overridden || pol.Schedule.Overridden {
		t.Errorf("policy = %+v, want nothing saved", pol)
	}
	if rows := f.policyAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}

// A save made from a stale read is refused 409 settings_changed, names who saved last and writes nothing.
func TestJanitorPolicyStaleVersion(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	// A first save against a version the environment never had.
	if rec := f.putJanitorPolicy(t, e, policyJSON(5, map[string]any{"keepN": 4})); rec.code != http.StatusConflict || rec.ptyp != "settings_changed" {
		t.Fatalf("a first save at version 5: %d %s, want 409 settings_changed", rec.code, rec.body)
	}
	if rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"keepN": 4})); rec.code != http.StatusOK {
		t.Fatalf("first save: %d %s", rec.code, rec.body)
	}
	// The same read, saved again, is stale; so is a version from the future.
	for _, version := range []int64{0, 2} {
		rec := f.putJanitorPolicy(t, e, policyJSON(version, map[string]any{"keepN": 9}))
		if rec.code != http.StatusConflict || rec.ptyp != "settings_changed" {
			t.Fatalf("PUT at version %d: %d %s, want 409 settings_changed", version, rec.code, rec.body)
		}
		var p policyProblem
		if err := json.Unmarshal(rec.body, &p); err != nil {
			t.Fatal(err)
		}
		var name string
		if err := f.st.Read.QueryRow(`SELECT COALESCE(NULLIF(u.display_name, ''), u.username) FROM janitor_env_policy p JOIN users u ON u.id = p.updated_by WHERE p.env_id = ?`, e.ID).Scan(&name); err != nil || name == "" {
			t.Fatalf("who saved last: %q %v", name, err)
		}
		if len(p.Errors) != 1 || p.Errors[0].Message != name || !strings.Contains(p.Detail, name) {
			t.Errorf("problem = %+v, want it to name %q", p, name)
		}
	}
	pol := f.readJanitorPolicy(t, e)
	if pol.Version != 1 || pol.KeepN.Value != 4 {
		t.Errorf("policy = %+v, want the first save kept at version 1", pol)
	}
	if rows := f.policyAudits(t); len(rows) != 1 {
		t.Errorf("audit rows = %d, want only the first save's", len(rows))
	}
}

// A save that changes nothing answers 200, keeps the version and writes no audit row, whether the
// environment has a row or not.
func TestJanitorPolicyNoOp(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	rec := f.putJanitorPolicy(t, e, policyJSON(0, nil))
	if rec.code != http.StatusOK {
		t.Fatalf("an empty save: %d %s", rec.code, rec.body)
	}
	if got := decode[janitorPolicyAnswer](t, rec); got.Version != 0 {
		t.Errorf("version = %d after saving nothing, want 0", got.Version)
	}
	var rows int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_env_policy WHERE env_id = ?`, e.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("policy rows = %d (%v), want none: saving nothing creates nothing", rows, err)
	}

	save := policyJSON(0, map[string]any{"keepN": 5, "schedule": "0 4 * * 1", "schedulePreset": "weekly"})
	if rec := f.putJanitorPolicy(t, e, save); rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	again := f.putJanitorPolicy(t, e, policyJSON(1, map[string]any{"keepN": 5, "schedule": "0 4 * * 1", "schedulePreset": "weekly"}))
	if again.code != http.StatusOK {
		t.Fatalf("the same save again: %d %s", again.code, again.body)
	}
	if got := decode[janitorPolicyAnswer](t, again); got.Version != 1 || got.KeepN.Value != 5 || got.Schedule.Value != "0 4 * * 1" || got.Schedule.Preset != "weekly" {
		t.Errorf("answer = %+v, want version 1 and the stored values", got)
	}
	if rows := f.policyAudits(t); len(rows) != 1 || rows[0].Details["fields"] != "schedule,keepN" {
		t.Errorf("audit rows = %+v, want the one save, naming schedule and keepN", rows)
	}
}

// A null returns a field to the default: the answer says it is not overridden, and a default changed
// afterwards reaches the environment (D-04).
func TestJanitorPolicyNullFollowsDefault(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	if rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"keepN": 5, "graceHours": 48})); rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	pol := f.readJanitorPolicy(t, e)
	if pol.KeepN != (policyInt{5, true}) || pol.GraceHours != (policyInt{48, true}) {
		t.Fatalf("policy = %+v, want keep 5 and grace 48 both overridden", pol)
	}

	rec := f.putJanitorPolicy(t, e, policyJSON(1, map[string]any{"graceHours": 48}))
	if rec.code != http.StatusOK {
		t.Fatalf("null keepN: %d %s", rec.code, rec.body)
	}
	got := decode[janitorPolicyAnswer](t, rec)
	if got.Version != 2 || got.KeepN != (policyInt{2, false}) || got.GraceHours != (policyInt{48, true}) {
		t.Fatalf("answer = %+v, want version 2, keep back at the default 2 and not overridden, grace still 48", got)
	}
	if rows := f.policyAudits(t); len(rows) != 2 || rows[1].Details["fields"] != "keepN" || rows[1].Details["version"] != "2" {
		t.Fatalf("audit rows = %+v, want the second to name keepN at version 2", rows)
	}

	// The default moves: the environment follows it for keep-N and keeps its own grace.
	if _, err := f.st.Write.Exec(`UPDATE janitor_defaults SET keep_n = 7, grace_hours = 36 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	moved := f.readJanitorPolicy(t, e)
	if moved.KeepN != (policyInt{7, false}) || moved.GraceHours != (policyInt{48, true}) || moved.Defaults.KeepN != 7 || moved.Defaults.GraceHours != 36 {
		t.Errorf("policy = %+v, want keep 7 followed, grace 48 kept, the defaults 7 and 36", moved)
	}
}

// Only a holder of janitor.policy changes the policy: an Operator without the extra is refused and the
// policy is left as it was; an Operator with it saves; a Viewer reads (AUTH-07, T-11-48).
func TestJanitorPolicyNeedsExtra(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	path := "/api/env/" + e.ID + "/janitor/policy"
	widen := policyJSON(0, map[string]any{"removeStoppedContainers": true, "removeUnusedNetworks": true})
	as := func(user, role string, extra bool) (cookie, csrf string) {
		f.addUser(t, user, false)
		f.addEnvRole(t, user, e.ID, role)
		if extra {
			if _, err := f.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'janitor.policy')`, "ra-"+user+"-"+e.ID); err != nil {
				t.Fatal(err)
			}
		}
		return f.signIn(t, user, false)
	}
	put := func(cookie, csrf string) *httptest.ResponseRecorder {
		return f.call(t, authReq{method: http.MethodPut, path: path, body: widen, cookie: cookie, csrf: csrf})
	}

	cookie, csrf := as("olive", "operator", false)
	if rec := put(cookie, csrf); rec.Code != http.StatusForbidden {
		t.Fatalf("an operator without the extra: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie}); rec.Code != http.StatusOK {
		t.Fatalf("an operator reading the policy: %d %s, want 200", rec.Code, rec.Body)
	}
	vc, vcsrf := as("vera", "viewer", false)
	if rec := put(vc, vcsrf); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer saving: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: path, cookie: vc}); rec.Code != http.StatusOK {
		t.Fatalf("a viewer reading the policy: %d %s, want 200", rec.Code, rec.Body)
	}
	if pol := f.readJanitorPolicy(t, e); pol.Version != 0 || pol.RemoveStoppedContainers.Overridden || pol.RemoveUnusedNetworks.Overridden {
		t.Fatalf("policy = %+v after two refusals, want it untouched", pol)
	}
	if rows := f.policyAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v after two refusals, want none", rows)
	}

	tc, tcsrf := as("trent", "operator", true)
	if rec := put(tc, tcsrf); rec.Code != http.StatusOK {
		t.Fatalf("an operator with janitor.policy: %d %s, want 200", rec.Code, rec.Body)
	}
	pol := f.readJanitorPolicy(t, e)
	if pol.Version != 1 || !pol.RemoveStoppedContainers.Value || !pol.RemoveUnusedNetworks.Value {
		t.Errorf("policy = %+v, want both opt-ins on at version 1", pol)
	}
	if rows := f.policyAudits(t); len(rows) != 1 || rows[0].Details["fields"] != "removeUnusedNetworks,removeStoppedContainers" {
		t.Errorf("audit rows = %+v, want one naming both opt-ins", rows)
	}
	var actor string
	if err := f.st.Read.QueryRow(`SELECT actor_id FROM audit_log WHERE action = 'janitor.policy_changed'`).Scan(&actor); err != nil || actor != "id-trent" {
		t.Errorf("actor = %q (%v), want the person who saved, id-trent", actor, err)
	}
}

// policyClock is the scheduler's clock: it tells the time the test sets, and After hands the test the
// timer to fire, announcing that the scheduler has finished a look and is waiting for the next.
type policyClock struct {
	mu    sync.Mutex
	now   time.Time
	armed chan chan time.Time
}

func (c *policyClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *policyClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.armed <- ch
	return ch
}

func (c *policyClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Turning the janitor off through the API stops the scheduled runs at once: the status has no next run
// and a scheduler that reads the stored policy drops the environment's slot, even when its time has come,
// while Clean now still works (D-01).
func TestJanitorPolicyDisabledStopsSchedule(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine()
	gone := map[string]string{"tierplan.stack.id": "stk-removed", "com.docker.compose.project": "oldshop", "com.docker.compose.service": "web"}
	cfg.Containers = []dockertest.FakeContainer{standaloneBox("old-web", 72*time.Hour, gone)}
	f, e := dashEnv(t, cfg)
	awaitOnline(t, f.mon, e.ID)

	status := func() janitorStatus {
		rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor", "")
		if rec.code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.code, rec.body)
		}
		return decode[janitorStatus](t, rec)
	}
	if got := status(); !got.Enabled || got.NextRunAt == nil {
		t.Fatalf("status = %+v, want the janitor on with a next run", got)
	}

	// A scheduler on the stored policy, as the server's is. Its runner has nothing to run with: a run it
	// started would fail this test.
	clk := &policyClock{now: janitorNow, armed: make(chan chan time.Time, 4)}
	sched := &janitor.Scheduler{
		Envs:   func(context.Context) ([]string, error) { return []string{e.ID}, nil },
		Runner: &janitor.Runner{Policy: janitor.PolicyStore{St: f.st}.Effective}, Clock: clk, Loc: time.UTC,
	}
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	ended := make(chan struct{})
	go func() { defer close(ended); sched.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-ended })
	tick := testsupport.Await(t, "the scheduler's first look to end", clk.armed)
	if _, ok := sched.Next(e.ID); !ok {
		t.Fatal("the scheduler has no next run for an environment that follows the default, which is on")
	}

	rec := f.putJanitorPolicy(t, e, policyJSON(0, map[string]any{"enabled": false}))
	if rec.code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.code, rec.body)
	}
	if got := decode[janitorPolicyAnswer](t, rec); got.Enabled != (policyBool{false, true}) {
		t.Errorf("answer = %+v, want the janitor off and overridden", got.Enabled)
	}
	if got := status(); got.Enabled || got.NextRunAt != nil {
		t.Errorf("status = %+v, want off with no next run at once", got)
	}

	// The slot's time comes and the scheduler looks again: it starts nothing and forgets the slot.
	clk.set(janitorNow.Add(48 * time.Hour))
	tick <- clk.Now()
	testsupport.Await(t, "the scheduler's second look to end", clk.armed)
	if _, ok := sched.Next(e.ID); ok {
		t.Error("the scheduler still has a next run for a janitor that is off")
	}
	var runs int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_runs WHERE env_id = ?`, e.ID).Scan(&runs); err != nil || runs != 0 {
		t.Errorf("runs = %d (%v), want none: the scheduler started nothing", runs, err)
	}

	// Clean now is the person's own decision and is not switched off with the schedule.
	preview := f.previewJanitor(t, e)
	if len(preview.Items) != 1 {
		t.Fatalf("preview items = %+v, want the leftover of the removed stack", preview.Items)
	}
	if job, _ := f.cleanNow(t, e, preview.Items...); job.State != jobs.JobDone || len(job.Items) != 1 {
		t.Errorf("job = %s %+v, want a done job with one item: Clean now answers 202 with the janitor off", job.State, job.Items)
	}
}
