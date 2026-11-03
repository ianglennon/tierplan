// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
