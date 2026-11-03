// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The instance-wide janitor defaults and the retention of the run history (plan 11-21, JAN-01, JAN-10, D-04,
// D-16, D-21): every environment follows a default until it sets the field itself, and an instance admin
// changes them for all of them at once.

type janitorDefaultsAnswer struct {
	Version                 int64  `json:"version"`
	Timezone                string `json:"timezone"`
	Enabled                 bool   `json:"enabled"`
	Schedule                string `json:"schedule"`
	SchedulePreset          string `json:"schedulePreset"`
	KeepN                   int    `json:"keepN"`
	GraceHours              int    `json:"graceHours"`
	RemoveUnusedNetworks    bool   `json:"removeUnusedNetworks"`
	RemoveStoppedContainers bool   `json:"removeStoppedContainers"`
	RetentionDays           int    `json:"retentionDays"`
	RetentionRuns           int    `json:"retentionRuns"`
	FollowingEnvironments   int    `json:"followingEnvironments"`
	RemovedRuns             int    `json:"removedRuns"`
}

// defaultsJSON is a save body: the defaults as they were read, with the fields given changed.
func defaultsJSON(cur janitorDefaultsAnswer, fields map[string]any) string {
	body := map[string]any{
		"version": cur.Version, "enabled": cur.Enabled, "schedule": cur.Schedule, "schedulePreset": cur.SchedulePreset,
		"keepN": cur.KeepN, "graceHours": cur.GraceHours, "removeUnusedNetworks": cur.RemoveUnusedNetworks,
		"removeStoppedContainers": cur.RemoveStoppedContainers, "retentionDays": cur.RetentionDays, "retentionRuns": cur.RetentionRuns,
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

func (f *envFix) readJanitorDefaults(t *testing.T) janitorDefaultsAnswer {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/settings/janitor", "")
	if rec.code != http.StatusOK {
		t.Fatalf("janitor defaults: %d %s", rec.code, rec.body)
	}
	return decode[janitorDefaultsAnswer](t, rec)
}

func (f *envFix) putJanitorDefaults(t *testing.T, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPut, "/api/settings/janitor", body)
}

func (f *envFix) defaultsAudits(t *testing.T) []auditRowFull {
	t.Helper()
	return f.auditRows(t, "janitor.defaults_changed")
}

// The tracer (plan 11-21, JAN-01, D-04, D-21): two environments hold the same five exited containers of one
// slot, one follows the default keep of 2 and the other sets its own keep of 3. Changing the default to 0
// through the instance route changes the follower's preview and not the other's, writes no environment row and
// is one audit row.
func TestJanitorDefaultsKeepNFollows(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	other := s.srv.NewEnvironment(t, "Solo")
	cfg := dockertest.FakeConfig{Hostname: "solo"}
	for _, b := range exitedSlot(5) {
		cfg.Containers = append(cfg.Containers, b.fake())
	}
	s.connect(t, other, cfg, nil)
	s.whenState(t, other.ID, "online")

	preview := func(envID string) int {
		t.Helper()
		rec := s.req(t, http.MethodPost, "/api/env/"+envID+"/janitor/preview", "")
		if rec.code != http.StatusOK {
			t.Fatalf("preview of %s: %d %s", envID, rec.code, rec.body)
		}
		return len(decode[janitorPreview](t, rec).Items)
	}
	// The swarm environment sets its own keep of 3; the standalone one follows the default.
	if rec := s.req(t, http.MethodPut, "/api/env/"+s.env.ID+"/janitor/policy", policyJSON(0, map[string]any{"keepN": 3})); rec.code != http.StatusOK {
		t.Fatalf("set the override: %d %s", rec.code, rec.body)
	}
	if overrider, follower := preview(s.env.ID), preview(other.ID); overrider != 2 || follower != 3 {
		t.Fatalf("previews before = overrider %d follower %d, want 2 and 3", overrider, follower)
	}

	cur := s.readJanitorDefaults(t)
	if cur.Version != 1 || cur.KeepN != 2 || cur.GraceHours != 24 || !cur.Enabled || cur.Schedule != "0 3 * * *" || cur.SchedulePreset != "daily" ||
		cur.RemoveUnusedNetworks || cur.RemoveStoppedContainers || cur.RetentionDays != 90 || cur.RetentionRuns != 100 || cur.Timezone == "" {
		t.Fatalf("defaults = %+v, want the seeded ones at version 1", cur)
	}
	if cur.FollowingEnvironments != 2 {
		t.Errorf("following = %d, want 2: each environment follows every default but the keep the swarm one sets", cur.FollowingEnvironments)
	}
	var envRows int
	if err := s.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_env_policy`).Scan(&envRows); err != nil {
		t.Fatal(err)
	}

	rec := s.putJanitorDefaults(t, defaultsJSON(cur, map[string]any{"keepN": 0}))
	if rec.code != http.StatusOK {
		t.Fatalf("change the default: %d %s", rec.code, rec.body)
	}
	if got := decode[janitorDefaultsAnswer](t, rec); got.KeepN != 0 || got.Version != 2 || got.RemovedRuns != 0 {
		t.Errorf("answer = %+v, want keep 0 at version 2 and no run removed", got)
	}
	if overrider, follower := preview(s.env.ID), preview(other.ID); overrider != 2 || follower != 5 {
		t.Errorf("previews after = overrider %d follower %d, want 2 (unchanged) and 5 (all)", overrider, follower)
	}
	var after int
	if err := s.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_env_policy`).Scan(&after); err != nil || after != envRows {
		t.Errorf("environment policy rows = %d (%v), want %d: changing a default writes no environment", after, err, envRows)
	}
	rows := s.defaultsAudits(t)
	if len(rows) != 1 || rows[0].Details["fields"] != "keepN" || rows[0].Details["removed_runs"] != "0" || rows[0].Details["version"] != "2" || rows[0].Result != "success" {
		t.Errorf("audit rows = %+v, want one naming keepN, 0 removed runs and version 2", rows)
	}
}

type retentionPreviewAnswer struct {
	Runs         int `json:"runs"`
	Environments int `json:"environments"`
}

func (f *envFix) previewRetention(t *testing.T, days, runs int) (*envRec, retentionPreviewAnswer) {
	t.Helper()
	rec := f.req(t, http.MethodGet, fmt.Sprintf("/api/settings/janitor/retention-preview?days=%d&runs=%d", days, runs), "")
	var got retentionPreviewAnswer
	if rec.code == http.StatusOK {
		got = decode[retentionPreviewAnswer](t, rec)
	}
	return rec, got
}

// historyFix is the run history of two environments: env A (the one added) has 30 completed runs ten days
// apart and a run that has been running for 200 days, env B has 25 completed runs twelve days apart.
type historyFix struct {
	*envFix
	a, b envBody
	runs *janitor.SQLStore
}

func newHistoryFix(t *testing.T) historyFix {
	t.Helper()
	f, a := dashEnv(t, orphanEngine())
	ctx := testsupport.Context(t)
	if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-b', 'agent', 'b', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	h := historyFix{envFix: f, a: a, b: envBody{ID: "env-b"}, runs: &janitor.SQLStore{St: f.st}}
	const day = 24 * time.Hour
	for i := range 30 {
		h.add(t, a.ID, fmt.Sprintf("a-%02d", i), janitor.StateCompleted, janitorNow.Add(-time.Duration(i)*10*day))
	}
	for i := range 25 {
		h.add(t, h.b.ID, fmt.Sprintf("b-%02d", i), janitor.StateCompleted, janitorNow.Add(-time.Duration(i)*12*day))
	}
	h.add(t, a.ID, "a-stuck", janitor.StateRunning, janitorNow.Add(-200*day))
	// The oldest completed run of A has an item and its evidence, which go with it when it is pruned.
	if err := h.runs.AddItems(ctx, "a-29", []janitor.RunItem{{Seq: 1, Type: janitor.KindContainer, Ref: "c1", Outcome: janitor.OutcomeRemoved, Size: -1}}); err != nil {
		t.Fatal(err)
	}
	if err := h.runs.SaveEvidence(ctx, "a-29", 1, janitor.Evidence{ExitCode: 1, State: janitor.EvidenceEmpty}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h historyFix) add(t *testing.T, env, id, state string, started time.Time) {
	t.Helper()
	ctx := testsupport.Context(t)
	run := janitor.Run{ID: id, EnvID: env, Trigger: janitor.TriggerSchedule, Actor: janitor.SystemActor, State: state, StartedAt: started, EstBytes: -1}
	if err := h.runs.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if state != janitor.StateRunning && state != janitor.StateWaiting {
		run.FinishedAt = started.Add(time.Minute)
		if err := h.runs.FinishRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
}

func (h historyFix) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := h.st.Read.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 30 days or the newest 10: A keeps its newest 10 (the four within 30 days are among them) and B its newest 10,
// so 20 + 15 runs of two environments would go; the run that is still running is never counted.
func TestJanitorRetentionPreview(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	rec, got := h.previewRetention(t, 30, 10)
	if rec.code != http.StatusOK || got.Runs != 35 || got.Environments != 2 {
		t.Fatalf("preview 30 days / 10 runs: %d %s, want 35 runs of 2 environments", rec.code, rec.body)
	}
	// A looser number of days still counts what is beyond both: the runs older than 120 days and not among the newest 10
	// (17 of A, whose run of exactly 120 days stays, and 14 of B), and nothing when no run is that old.
	if _, got := h.previewRetention(t, 120, 10); got.Runs != 17+14 || got.Environments != 2 {
		t.Errorf("preview 120 days / 10 runs = %+v, want 31 runs of 2 environments", got)
	}
	if _, got := h.previewRetention(t, 365, 10); got.Runs != 0 || got.Environments != 0 {
		t.Errorf("preview 365 days / 10 runs = %+v, want nothing: no run is a year old", got)
	}
	// Raising either limit, or keeping the stored one, deletes nothing.
	for name, c := range map[string][2]int{"the stored one": {90, 100}, "days up": {365, 100}, "both up": {365, 500}} {
		if rec, got := h.previewRetention(t, c[0], c[1]); rec.code != http.StatusOK || got.Runs != 0 || got.Environments != 0 {
			t.Errorf("preview of %s: %d %s, want 0 runs of 0 environments", name, rec.code, rec.body)
		}
	}
	// Previewing deletes nothing and writes no audit row.
	if n := h.count(t, "janitor_runs"); n != 30+25+1 {
		t.Errorf("runs = %d after previews, want all 56", n)
	}
	if rows := h.defaultsAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none: a preview only reads", rows)
	}
	// Values outside the ranges are refused before anything is counted.
	for _, q := range []string{"days=6&runs=10", "days=3651&runs=10", "days=30&runs=9", "days=30&runs=1001", "days=30", "runs=10"} {
		if rec := h.req(t, http.MethodGet, "/api/settings/janitor/retention-preview?"+q, ""); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("preview ?%s: %d %s, want 422", q, rec.code, rec.body)
		}
	}
}

// Lowering the retention deletes exactly the runs the preview counted, with their items and evidence, in the
// transaction that stores it: a save whose audit row cannot be written leaves the runs and the old retention
// (plan 11-21, JAN-10, D-16, T-11-55).
func TestJanitorDefaultsRetentionPrunesInTx(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	cur := h.readJanitorDefaults(t)
	_, preview := h.previewRetention(t, 30, 10)
	lower := defaultsJSON(cur, map[string]any{"retentionDays": 30, "retentionRuns": 10})

	// An audit log that refuses the row takes the deletion down with it.
	if _, err := h.st.Write.Exec(`CREATE TRIGGER refuse_defaults_audit BEFORE INSERT ON audit_log WHEN NEW.action = 'janitor.defaults_changed'
		BEGIN SELECT RAISE(ABORT, 'audit log unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if rec := h.putJanitorDefaults(t, lower); rec.code != http.StatusInternalServerError {
		t.Fatalf("save with a failing audit log: %d %s, want 500", rec.code, rec.body)
	}
	if n := h.count(t, "janitor_runs"); n != 56 {
		t.Errorf("runs = %d after the failed save, want all 56: the deletion is part of the transaction", n)
	}
	if after := h.readJanitorDefaults(t); after.Version != cur.Version || after.RetentionDays != 90 || after.RetentionRuns != 100 {
		t.Errorf("defaults = %+v after the failed save, want the old retention at version %d", after, cur.Version)
	}
	if _, err := h.st.Write.Exec(`DROP TRIGGER refuse_defaults_audit`); err != nil {
		t.Fatal(err)
	}

	rec := h.putJanitorDefaults(t, lower)
	if rec.code != http.StatusOK {
		t.Fatalf("lower the retention: %d %s", rec.code, rec.body)
	}
	got := decode[janitorDefaultsAnswer](t, rec)
	if got.RemovedRuns != preview.Runs || got.RemovedRuns != 35 || got.RetentionDays != 30 || got.RetentionRuns != 10 || got.Version != cur.Version+1 {
		t.Errorf("answer = %+v, want 35 runs removed as previewed and the new retention at version %d", got, cur.Version+1)
	}
	// A keeps its newest 10 and the running one; B its newest 10.
	if n := h.count(t, "janitor_runs"); n != 10+1+10 {
		t.Errorf("runs = %d, want 21", n)
	}
	var stuck int
	if err := h.st.Read.QueryRow(`SELECT COUNT(*) FROM janitor_runs WHERE id = 'a-stuck' AND state = 'running'`).Scan(&stuck); err != nil || stuck != 1 {
		t.Errorf("the running run: %d (%v), want it kept", stuck, err)
	}
	if h.count(t, "janitor_run_items") != 0 || h.count(t, "janitor_evidence") != 0 {
		t.Errorf("items/evidence = %d/%d, want the pruned run's gone with it", h.count(t, "janitor_run_items"), h.count(t, "janitor_evidence"))
	}
	rows := h.defaultsAudits(t)
	if len(rows) != 1 || rows[0].Details["fields"] != "retentionDays,retentionRuns" || rows[0].Details["removed_runs"] != "35" || rows[0].Details["version"] != "2" {
		t.Errorf("audit rows = %+v, want one naming both retention fields and the 35 removed runs", rows)
	}
	// Saving the same retention again removes nothing and writes no second row.
	again := h.readJanitorDefaults(t)
	if rec := h.putJanitorDefaults(t, defaultsJSON(again, nil)); rec.code != http.StatusOK || decode[janitorDefaultsAnswer](t, rec).RemovedRuns != 0 {
		t.Errorf("the same save: %d %s, want 200 and nothing removed", rec.code, rec.body)
	}
	if rows := h.defaultsAudits(t); len(rows) != 1 {
		t.Errorf("audit rows = %d, want still 1", len(rows))
	}
}

// Raising the limits deletes nothing, even from a history that holds runs the old retention would not keep.
func TestJanitorDefaultsRaiseKeepsAll(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	// Runs the stored 90 days or newest 100 has stopped keeping: A gets 130 further runs, a year old and older.
	for i := range 130 {
		h.add(t, h.a.ID, fmt.Sprintf("a-old-%03d", i), janitor.StateCompleted, janitorNow.Add(-time.Duration(365+i)*24*time.Hour))
	}
	total := h.count(t, "janitor_runs")
	cur := h.readJanitorDefaults(t)
	// Each step raises from the one before, in this order.
	for _, step := range []struct {
		name   string
		fields map[string]any
	}{
		{"days up", map[string]any{"retentionDays": 120}},
		{"runs up", map[string]any{"retentionRuns": 101}},
		{"both up", map[string]any{"retentionDays": 3650, "retentionRuns": 1000}},
	} {
		name, f := step.name, step.fields
		rec := h.putJanitorDefaults(t, defaultsJSON(cur, f))
		if rec.code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.code, rec.body)
		}
		if got := decode[janitorDefaultsAnswer](t, rec); got.RemovedRuns != 0 {
			t.Errorf("%s: removed %d runs, want none", name, got.RemovedRuns)
		}
		if n := h.count(t, "janitor_runs"); n != total {
			t.Errorf("%s: %d runs left of %d, want all", name, n, total)
		}
		cur = h.readJanitorDefaults(t)
	}
	if rows := h.defaultsAudits(t); len(rows) != 3 || rows[0].Details["removed_runs"] != "0" {
		t.Errorf("audit rows = %+v, want three, each with no removed runs", rows)
	}
}

// The status route says the retention the defaults store, and so does the page that reads it.
func TestJanitorStatusReadsStoredRetention(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	awaitOnline(t, f.mon, e.ID)
	cur := f.readJanitorDefaults(t)
	if rec := f.putJanitorDefaults(t, defaultsJSON(cur, map[string]any{"retentionDays": 45, "retentionRuns": 250})); rec.code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.code, rec.body)
	}
	rec := f.req(t, http.MethodGet, "/api/env/"+e.ID+"/janitor", "")
	if rec.code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.code, rec.body)
	}
	var st struct {
		Retention struct{ Days, Runs int } `json:"retention"`
	}
	if err := json.Unmarshal(rec.body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Retention.Days != 45 || st.Retention.Runs != 250 {
		t.Errorf("status retention = %+v, want 45 days or 250 runs", st.Retention)
	}
}

// A value outside what the janitor accepts is refused with a 422 and nothing is saved: not a version, not a
// run deleted, not an audit row (plan 11-21, D-16).
func TestJanitorDefaultsValidation(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	cur := h.readJanitorDefaults(t)
	t.Run("refused", func(t *testing.T) {
		for name, tc := range map[string]struct {
			fields map[string]any
			want   string
		}{
			"retention days 6":       {map[string]any{"retentionDays": 6}, ""},
			"retention days 3651":    {map[string]any{"retentionDays": 3651}, ""},
			"retention runs 9":       {map[string]any{"retentionRuns": 9}, ""},
			"retention runs 1001":    {map[string]any{"retentionRuns": 1001}, ""},
			"keep 21":                {map[string]any{"keepN": 21}, ""},
			"keep -1":                {map[string]any{"keepN": -1}, ""},
			"grace 0":                {map[string]any{"graceHours": 0}, ""},
			"grace 721":              {map[string]any{"graceHours": 721}, ""},
			"every minute":           {map[string]any{"schedule": "* * * * *"}, "schedule_too_frequent"},
			"every ten minutes":      {map[string]any{"schedule": "*/10 * * * *", "schedulePreset": "custom"}, "schedule_too_frequent"},
			"not a schedule":         {map[string]any{"schedule": "nonsense"}, "invalid_schedule"},
			"six fields":             {map[string]any{"schedule": "0 0 3 * * *"}, "invalid_schedule"},
			"an empty schedule":      {map[string]any{"schedule": ""}, ""},
			"a preset nobody knows":  {map[string]any{"schedulePreset": "fortnightly"}, ""},
			"a bad value beside two": {map[string]any{"keepN": 3, "retentionDays": 30, "graceHours": 900}, ""},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				rec := h.putJanitorDefaults(t, defaultsJSON(cur, tc.fields))
				if rec.code != http.StatusUnprocessableEntity {
					t.Fatalf("PUT: %d %s, want 422", rec.code, rec.body)
				}
				if tc.want != "" && rec.ptyp != tc.want {
					t.Errorf("problem type = %q, want %q", rec.ptyp, tc.want)
				}
			})
		}
		// A field left out is not a default: every one is required.
		for _, field := range []string{"enabled", "schedule", "schedulePreset", "keepN", "graceHours", "removeUnusedNetworks", "removeStoppedContainers", "retentionDays", "retentionRuns", "version"} {
			t.Run("without "+field, func(t *testing.T) {
				t.Parallel()
				var body map[string]any
				if err := json.Unmarshal([]byte(defaultsJSON(cur, nil)), &body); err != nil {
					t.Fatal(err)
				}
				delete(body, field)
				b, _ := json.Marshal(body)
				if rec := h.putJanitorDefaults(t, string(b)); rec.code != http.StatusUnprocessableEntity {
					t.Errorf("PUT without %s: %d %s, want 422", field, rec.code, rec.body)
				}
			})
		}
	})
	// The group returns once all its parallel cases have ended.
	if after := h.readJanitorDefaults(t); after != cur {
		t.Errorf("defaults = %+v, want %+v: nothing saved", after, cur)
	}
	if n := h.count(t, "janitor_runs"); n != 56 {
		t.Errorf("runs = %d, want all 56: a refused save deletes nothing", n)
	}
	if rows := h.defaultsAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}

// A save made from a stale read is refused 409 settings_changed, names who saved last and writes nothing.
func TestJanitorDefaultsStaleVersion(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	cur := h.readJanitorDefaults(t)
	if rec := h.putJanitorDefaults(t, defaultsJSON(cur, map[string]any{"keepN": 4})); rec.code != http.StatusOK {
		t.Fatalf("first save: %d %s", rec.code, rec.body)
	}
	var name string
	if err := h.st.Read.QueryRow(`SELECT COALESCE(NULLIF(u.display_name, ''), u.username) FROM janitor_defaults d JOIN users u ON u.id = d.updated_by WHERE d.id = 1`).Scan(&name); err != nil || name == "" {
		t.Fatalf("who saved last: %q %v", name, err)
	}
	// The same read, saved again, is stale; so is a version from the future. A stale save that would also lower the
	// retention deletes nothing.
	for _, version := range []int64{cur.Version, cur.Version + 5} {
		stale := cur
		stale.Version = version
		rec := h.putJanitorDefaults(t, defaultsJSON(stale, map[string]any{"keepN": 9, "retentionDays": 30, "retentionRuns": 10}))
		if rec.code != http.StatusConflict || rec.ptyp != "settings_changed" {
			t.Fatalf("PUT at version %d: %d %s, want 409 settings_changed", version, rec.code, rec.body)
		}
		var p policyProblem
		if err := json.Unmarshal(rec.body, &p); err != nil {
			t.Fatal(err)
		}
		if len(p.Errors) != 1 || p.Errors[0].Message != name || !strings.Contains(p.Detail, name) {
			t.Errorf("problem = %+v, want it to name %q", p, name)
		}
	}
	after := h.readJanitorDefaults(t)
	if after.Version != cur.Version+1 || after.KeepN != 4 || after.RetentionDays != 90 || after.RetentionRuns != 100 {
		t.Errorf("defaults = %+v, want the first save kept at version %d", after, cur.Version+1)
	}
	if n := h.count(t, "janitor_runs"); n != 56 {
		t.Errorf("runs = %d, want all 56: a stale save deletes nothing", n)
	}
	if rows := h.defaultsAudits(t); len(rows) != 1 {
		t.Errorf("audit rows = %d, want only the first save's", len(rows))
	}
}

// A save that changes nothing answers 200, keeps the version and the author and writes no audit row.
func TestJanitorDefaultsNoOp(t *testing.T) {
	t.Parallel()
	f, _ := dashEnv(t, orphanEngine())
	cur := f.readJanitorDefaults(t)
	var by0 string
	var at0 int64
	if err := f.st.Read.QueryRow(`SELECT updated_by, updated_at FROM janitor_defaults WHERE id = 1`).Scan(&by0, &at0); err != nil {
		t.Fatal(err)
	}
	rec := f.putJanitorDefaults(t, defaultsJSON(cur, nil))
	if rec.code != http.StatusOK {
		t.Fatalf("the same defaults: %d %s", rec.code, rec.body)
	}
	if got := decode[janitorDefaultsAnswer](t, rec); got != cur {
		t.Errorf("answer = %+v, want what was read: %+v", got, cur)
	}
	var by1 string
	var at1 int64
	if err := f.st.Read.QueryRow(`SELECT updated_by, updated_at FROM janitor_defaults WHERE id = 1`).Scan(&by1, &at1); err != nil || by1 != by0 || at1 != at0 {
		t.Errorf("updated_by/at = %q/%d (%v), want %q/%d untouched", by1, at1, err, by0, at0)
	}
	// A change, then the same change again at its new version, is a no-op the second time.
	if rec := f.putJanitorDefaults(t, defaultsJSON(cur, map[string]any{"graceHours": 48})); rec.code != http.StatusOK {
		t.Fatalf("change: %d %s", rec.code, rec.body)
	}
	next := f.readJanitorDefaults(t)
	if rec := f.putJanitorDefaults(t, defaultsJSON(next, map[string]any{"graceHours": 48})); rec.code != http.StatusOK || decode[janitorDefaultsAnswer](t, rec).Version != next.Version {
		t.Errorf("the same change again: %d %s, want 200 at version %d", rec.code, rec.body, next.Version)
	}
	if rows := f.defaultsAudits(t); len(rows) != 1 || rows[0].Details["fields"] != "graceHours" {
		t.Errorf("audit rows = %+v, want the one change, naming graceHours", rows)
	}
}

// Only an instance admin changes what every environment follows: an Env Admin of every environment and an
// Operator holding janitor.policy are refused on the save and on the retention preview and nothing changes; a
// Viewer is refused too; an instance admin saves (plan 11-21, D-21, T-11-54).
func TestJanitorDefaultsNeedsSettingsWrite(t *testing.T) {
	t.Parallel()
	h := newHistoryFix(t)
	cur := h.readJanitorDefaults(t)
	lower := defaultsJSON(cur, map[string]any{"retentionDays": 30, "retentionRuns": 10, "removeUnusedNetworks": true})
	const preview = "/api/settings/janitor/retention-preview?days=30&runs=10"
	call := func(method, path, body, cookie, csrf string) *httptest.ResponseRecorder {
		return h.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf})
	}
	as := func(user, role string, extra, instanceAdmin bool, envID string) (cookie, csrf string) {
		h.addUser(t, user, instanceAdmin)
		if role != "" {
			h.addEnvRole(t, user, envID, role)
		}
		if extra {
			if _, err := h.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'janitor.policy')`, "ra-"+user+"-"+envID); err != nil {
				t.Fatal(err)
			}
		}
		return h.signIn(t, user, false)
	}

	for user, who := range map[string]struct {
		name  string
		role  string
		extra bool
		env   string
	}{
		"allenv":  {"an env admin of every environment", "admin", false, ""},
		"oneenv":  {"an env admin of one environment", "admin", false, h.a.ID},
		"opextra": {"an operator with janitor.policy", "operator", true, h.a.ID},
		"viewer":  {"a viewer", "viewer", false, h.a.ID},
	} {
		name := who.name
		cookie, csrf := as(user, who.role, who.extra, false, who.env)
		if rec := call(http.MethodPut, "/api/settings/janitor", lower, cookie, csrf); rec.Code != http.StatusForbidden {
			t.Errorf("%s saving: %d %s, want 403", name, rec.Code, rec.Body)
		}
		if rec := call(http.MethodGet, preview, "", cookie, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s previewing the retention: %d %s, want 403", name, rec.Code, rec.Body)
		}
		if rec := call(http.MethodGet, "/api/settings/janitor", "", cookie, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s reading the defaults: %d %s, want 403: reading needs settings.read, which is the instance admin's", name, rec.Code, rec.Body)
		}
	}
	if rec := call(http.MethodGet, "/api/settings/janitor", "", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("reading signed out: %d, want 401", rec.Code)
	}
	if after := h.readJanitorDefaults(t); after != cur {
		t.Fatalf("defaults = %+v after the refusals, want them untouched: %+v", after, cur)
	}
	if n := h.count(t, "janitor_runs"); n != 56 {
		t.Fatalf("runs = %d after the refusals, want all 56", n)
	}
	if rows := h.defaultsAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v after the refusals, want none", rows)
	}

	cookie, csrf := as("ivy", "", false, true, "")
	if rec := call(http.MethodGet, "/api/settings/janitor", "", cookie, ""); rec.Code != http.StatusOK {
		t.Errorf("an instance admin reading: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := call(http.MethodGet, preview, "", cookie, ""); rec.Code != http.StatusOK {
		t.Errorf("an instance admin previewing: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := call(http.MethodPut, "/api/settings/janitor", lower, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("an instance admin saving: %d %s, want 200", rec.Code, rec.Body)
	}
	after := h.readJanitorDefaults(t)
	if after.RetentionDays != 30 || after.RetentionRuns != 10 || !after.RemoveUnusedNetworks {
		t.Errorf("defaults = %+v, want the instance admin's change", after)
	}
	var actor string
	if err := h.st.Read.QueryRow(`SELECT actor_id FROM audit_log WHERE action = 'janitor.defaults_changed'`).Scan(&actor); err != nil || actor != "id-ivy" {
		t.Errorf("actor = %q (%v), want the person who saved, id-ivy", actor, err)
	}
}
