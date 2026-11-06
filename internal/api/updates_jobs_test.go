// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The header's Jobs indicator lists image updates (plan 12-16, IMG-06, T-12-43): those running or ended in the last 10 minutes, on
// the environments the caller may read, newest first, and nothing else.

type updateJobRow struct {
	ID        string `json:"id"`
	EnvID     string `json:"envId"`
	EnvName   string `json:"envName"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt"`
	Bulk      string `json:"bulk"`
}

// hubClock is a clock a test moves.
type hubClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *hubClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *hubClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (f *envFix) listUpdateJobs(t *testing.T, cookie, csrf string) ([]updateJobRow, int) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/update-jobs", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	return answerOf[struct {
		Jobs []updateJobRow `json:"jobs"`
	}](t, rec).Jobs, rec.Code
}

func TestListImageUpdateJobsFiltersEnvironments(t *testing.T) {
	t.Parallel()
	clk := &hubClock{t: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)}
	f := newEnvFix(t, swarmManagerEngine(plainService("web", 'a')))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f.jobHub = jobs.New(jobs.Options{Now: clk.now})
	t.Cleanup(f.jobHub.Close)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	viewer := f.caller(t, e, "viewer-1", "viewer")
	f.addUser(t, "outsider", false)
	outCookie, outCSRF := f.signIn(t, "outsider", false)

	meta := func(workload, env string) map[string]string {
		return map[string]string{updates.MetaWorkload: workload, updates.MetaKind: "service", updates.MetaEnvName: env, updates.MetaBy: "ann"}
	}
	finished := func(kind, envID string, m map[string]string) string {
		j, err := f.jobHub.Start(jobs.Spec{Kind: kind, EnvID: envID, Owner: "user:ann", Meta: m}, func(context.Context, *jobs.Reporter) {})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.jobHub.Wait(context.Background(), j.ID); err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	running := func(envID string, m map[string]string) string {
		j, err := f.jobHub.Start(jobs.Spec{Kind: updates.JobKind, EnvID: envID, Owner: "user:bob", Meta: m}, func(ctx context.Context, _ *jobs.Reporter) {
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}

	// An old finished update, then a finished one, a running one, one of an environment the viewer has no role on, and a stack
	// job, which belongs to another list.
	old := finished(updates.JobKind, e.ID, meta("old", "home"))
	clk.advance(11 * time.Minute)
	done := finished(updates.JobKind, e.ID, meta("api", "home"))
	clk.advance(time.Second)
	run := running(e.ID, meta("web", "home"))
	finished(updates.JobKind, "env-of-someone-else", meta("secret-service", "elsewhere"))
	finished("stack_deploy", e.ID, meta("a-stack", "home"))

	rows, code := f.listUpdateJobs(t, viewer.cookie, viewer.csrf)
	if code != http.StatusOK || len(rows) != 2 {
		t.Fatalf("viewer: %d %+v, want the running update and the finished one", code, rows)
	}
	if rows[0].ID != run || rows[0].Workload != "web" || rows[0].State != "running" || rows[0].EndedAt != "" || rows[0].EnvID != e.ID ||
		rows[0].EnvName != "home" || rows[0].Kind != "service" || rows[0].StartedAt == "" {
		t.Errorf("first = %+v, want web's running update, newest first", rows[0])
	}
	if rows[1].ID != done || rows[1].State != "done" || rows[1].EndedAt == "" {
		t.Errorf("second = %+v, want the finished update of api", rows[1])
	}
	for _, r := range rows {
		if r.ID == old || r.Workload == "secret-service" || r.Workload == "a-stack" {
			t.Errorf("row %+v is listed, want it absent", r)
		}
	}

	// Someone with no role on the environment is told nothing about it: the list is empty, not refused.
	if rows, code := f.listUpdateJobs(t, outCookie, outCSRF); code != http.StatusOK || len(rows) != 0 {
		t.Errorf("outsider: %d %+v, want 200 and no jobs", code, rows)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/update-jobs"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}
