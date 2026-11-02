// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// sweepClock stands still at one instant, so a run's age is a number the test chose.
type sweepClock struct{ t time.Time }

func (c sweepClock) Now() time.Time                       { return c.t }
func (c sweepClock) After(time.Duration) <-chan time.Time { return nil }

// A server that stopped in the middle of a clean-up leaves a run "waiting" or "running" in its
// database. The next start ends each as failed with the reason server_restarted, audits it, keeps
// what it had recorded, leaves every other run alone and prunes the history to the retention
// (plan 11-14, JAN-10, D-16, T-11-41, T-11-42).
func TestJanitorStartupSweep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	clock := sweepClock{now}
	ctx := testsupport.Context(t)

	// The first process builds the database; the rows are what it left behind when it stopped.
	first, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler), WithJanitorClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	seed := func(query string, args ...any) {
		t.Helper()
		if _, err := first.Store().Write.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-j', 'agent', 'Janitor env', '', 1, 1, 1)`)
	run := func(id, state string, started time.Time, finished int64) {
		seed(`INSERT INTO janitor_runs (id, env_id, trigger, actor, state, started_at, finished_at) VALUES (?, 'env-j', 'manual', 'u1', ?, ?, ?)`,
			id, state, started.UnixNano(), finished)
	}
	run("r-running", janitor.StateRunning, now.Add(-time.Hour), 0)
	run("r-waiting", janitor.StateWaiting, now.Add(-30*time.Minute), 0)
	run("r-done", janitor.StateCompleted, now.Add(-2*time.Hour), now.Add(-time.Hour).UnixNano())
	seed(`INSERT INTO janitor_run_items (run_id, seq, type, ref, reason, outcome, est_bytes) VALUES
		('r-running', 1, 'container', 'c1', 'task_beyond_keep', 'removed', 40),
		('r-running', 2, 'container', 'c2', 'task_beyond_keep', 'error', -1)`)
	// 150 older runs, a day apart from 100 days ago: far more than the newest 100 allow.
	for i := range 150 {
		started := now.Add(-time.Duration(100+i) * 24 * time.Hour)
		run(fmt.Sprintf("old-%03d", i), janitor.StateCompleted, started, started.Add(time.Minute).UnixNano())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	srv, err := Build(ctx, testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dir}), slog.New(slog.DiscardHandler), WithJanitorClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	runs := &janitor.SQLStore{St: srv.Store()}

	for id, wantItems := range map[string]int{"r-running": 2, "r-waiting": 0} {
		got, items, err := runs.GetRun(ctx, "env-j", id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if got.State != janitor.StateFailed || got.Reason != janitor.ReasonServerRestarted || !got.FinishedAt.Equal(now) || len(items) != wantItems {
			t.Errorf("%s = %+v with %d items, want failed with server_restarted, finished at the start and %d items", id, got, len(items), wantItems)
		}
	}
	if got, _, _ := runs.GetRun(ctx, "env-j", "r-running"); got.Counts.RemovedContainers != 1 || got.Counts.Failed != 1 || got.EstBytes != 40 {
		t.Errorf("r-running counts = %+v est %d, want what its two items give", got.Counts, got.EstBytes)
	}
	if got, _, err := runs.GetRun(ctx, "env-j", "r-done"); err != nil || got.State != janitor.StateCompleted || got.Reason != "" {
		t.Errorf("r-done = %+v, %v, want it untouched", got, err)
	}

	// The history is pruned to the newest 100: the 3 recent runs and the 97 newest old ones.
	list, err := runs.ListRuns(ctx, "env-j", 1000)
	if err != nil || len(list) != 100 || list[99].ID != "old-096" {
		last := ""
		if len(list) > 0 {
			last = list[len(list)-1].ID
		}
		t.Fatalf("history = %d runs ending at %s (%v), want the newest 100 ending at old-096", len(list), last, err)
	}

	// One audit row for each swept run, by the system, failed, with the reason.
	rows, err := srv.Store().Read.QueryContext(ctx, `SELECT actor_type, actor_id, result, reason, env_id, details FROM audit_log WHERE action = 'janitor.cleaned' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var actorType, actorID, result, reason, env, details string
		if err := rows.Scan(&actorType, &actorID, &result, &reason, &env, &details); err != nil {
			t.Fatal(err)
		}
		var d map[string]string
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		if actorType != "system" || actorID != janitor.SystemActor || result != "failure" || reason != janitor.ReasonServerRestarted || env != "env-j" ||
			d["outcome"] != "failed" || d["reason"] != janitor.ReasonServerRestarted {
			t.Errorf("audit row = %s/%s %s %q %s %v, want janitor.cleaned by system:janitor, a failure with server_restarted", actorType, actorID, result, reason, env, d)
		}
		seen[d["run"]] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen["r-running"] || !seen["r-waiting"] {
		t.Errorf("audited runs = %v, want r-running and r-waiting once each", seen)
	}
}
