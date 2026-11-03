// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The run history is kept while a run started within the retention days or is among the newest
// retention runs of its environment, whichever keeps more (plan 11-14, JAN-10, D-16). Every test is on
// a real SQLite store: the rule is SQL.

const day = 24 * time.Hour

// retentionFix is a store with two environments.
type retentionFix struct {
	st  *store.Store
	s   *SQLStore
	now time.Time
}

func newRetentionFix(t *testing.T) retentionFix {
	t.Helper()
	st := storetest.New(t)
	if _, err := st.Write.ExecContext(testsupport.Context(t),
		`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1), ('env-2', 'agent', 'f', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	return retentionFix{st: st, s: &SQLStore{St: st}, now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
}

// add puts a run on record, finished unless its state is waiting or running.
func (f retentionFix) add(t *testing.T, env, id, state string, started time.Time) {
	t.Helper()
	ctx := testsupport.Context(t)
	run := Run{ID: id, EnvID: env, Trigger: TriggerSchedule, Actor: SystemActor, State: state, StartedAt: started, EstBytes: -1}
	if err := f.s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if state != StateRunning && state != StateWaiting {
		run.FinishedAt = started.Add(time.Minute)
		if err := f.s.FinishRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
}

// ids are the IDs of an environment's runs on record, in the order the history lists them.
func (f retentionFix) ids(t *testing.T, env string) []string {
	t.Helper()
	runs, err := f.s.ListRuns(testsupport.Context(t), env, 100000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func (f retentionFix) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRowContext(testsupport.Context(t), `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func runName(i int) string { return fmt.Sprintf("run-%03d", i) }

// 150 runs over 200 days: those within 90 days are 68 of them and the newest 100 are more, so 100
// survive. 150 runs over 20 days are all within 90 days, so all survive. Whichever keeps more.
func TestRetentionWhicheverKeepsMore(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	step := 200 * day / 150
	for i := range 150 { // run-000 is the newest
		f.add(t, "env-1", runName(i), StateCompleted, f.now.Add(-time.Duration(i)*step))
		f.add(t, "env-2", "two-"+runName(i), StateCompleted, f.now.Add(-time.Duration(i)*20*day/150))
	}
	// The oldest run has an item and its evidence, which go with it; so does the newest, which stays.
	for _, id := range []string{runName(0), runName(149)} {
		if err := f.s.AddItems(ctx, id, []RunItem{{Seq: 1, Type: KindContainer, Ref: "c1", Outcome: OutcomeRemoved, Size: -1}}); err != nil {
			t.Fatal(err)
		}
		if err := f.s.SaveEvidence(ctx, id, 1, Evidence{ExitCode: 1, State: EvidenceEmpty}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := f.s.PruneAll(ctx, DefaultRetention, f.now)
	if err != nil || n != 50 {
		t.Fatalf("PruneAll = %d, %v; want 50 runs of env-1 pruned", n, err)
	}
	got := f.ids(t, "env-1")
	if len(got) != 100 || got[0] != runName(0) || got[99] != runName(99) {
		t.Fatalf("env-1 keeps %d runs from %s to %s, want the newest 100 (run-000 to run-099): they are more than the 68 within 90 days", len(got), got[0], got[len(got)-1])
	}
	if len(f.ids(t, "env-2")) != 150 {
		t.Errorf("env-2 keeps %d runs, want all 150: every one started within 90 days", len(f.ids(t, "env-2")))
	}
	// 100 + 150 runs; the items and evidence of the pruned run went with it, the other's stayed.
	if f.count(t, "janitor_runs") != 250 || f.count(t, "janitor_run_items") != 1 || f.count(t, "janitor_evidence") != 1 {
		t.Errorf("runs/items/evidence = %d/%d/%d, want 250/1/1", f.count(t, "janitor_runs"), f.count(t, "janitor_run_items"), f.count(t, "janitor_evidence"))
	}

	// A second prune deletes nothing.
	if n, err := f.s.PruneRuns(ctx, "env-1", DefaultRetention, f.now); err != nil || n != 0 {
		t.Errorf("a second PruneRuns = %d, %v; want nothing", n, err)
	}
	// Within the days wins when it is more: 40 runs in 20 days with room for only 10 are all within 90 days.
	g := newRetentionFix(t)
	for i := range 40 {
		g.add(t, "env-1", runName(i), StateCompleted, g.now.Add(-time.Duration(i)*day/2))
	}
	if n, err := g.s.PruneRuns(ctx, "env-1", Retention{Days: 90, Runs: 10}, g.now); err != nil || n != 0 {
		t.Errorf("PruneRuns with 40 runs in 20 days and Runs 10 = %d, %v; want none: all are within 90 days", n, err)
	}
}

// A run that started exactly the retention days ago is kept, one a nanosecond older is not unless it is
// among the newest; the 100th newest run is kept and the 101st is pruned once it is older than the days.
func TestRetentionBoundary(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	cutoff := f.now.Add(-90 * day)
	f.add(t, "env-1", "new-1", StateCompleted, f.now.Add(-time.Hour))
	f.add(t, "env-1", "new-2", StateCompleted, f.now.Add(-2*time.Hour))
	f.add(t, "env-1", "exactly", StateCompleted, cutoff)
	f.add(t, "env-1", "one-ns-older", StateCompleted, cutoff.Add(-time.Nanosecond))
	if n, err := f.s.PruneRuns(ctx, "env-1", Retention{Days: 90, Runs: 2}, f.now); err != nil || n != 1 {
		t.Fatalf("PruneRuns = %d, %v; want 1 run pruned", n, err)
	}
	if got := f.ids(t, "env-1"); !slices.Equal(got, []string{"new-1", "new-2", "exactly"}) {
		t.Fatalf("kept %v, want the run that started exactly 90 days ago kept and the one a nanosecond older pruned", got)
	}

	// The 100th newest is kept and the 101st is pruned when it is older than 90 days.
	g := newRetentionFix(t)
	for i := range 101 {
		g.add(t, "env-1", runName(i), StateCompleted, g.now.Add(-91*day-time.Duration(i)*time.Hour))
	}
	if n, err := g.s.PruneRuns(ctx, "env-1", DefaultRetention, g.now); err != nil || n != 1 {
		t.Fatalf("PruneRuns = %d, %v; want the 101st pruned", n, err)
	}
	if got := g.ids(t, "env-1"); len(got) != 100 || got[99] != runName(99) {
		t.Fatalf("kept %d runs ending at %s, want 100 ending at %s", len(got), got[len(got)-1], runName(99))
	}
	// The 101st run is kept when it is within 90 days, however many there are.
	h := newRetentionFix(t)
	for i := range 101 {
		h.add(t, "env-1", runName(i), StateCompleted, h.now.Add(-time.Duration(i+1)*day/2))
	}
	if n, err := h.s.PruneRuns(ctx, "env-1", DefaultRetention, h.now); err != nil || n != 0 {
		t.Fatalf("PruneRuns = %d, %v; want nothing: all 101 started within 90 days", n, err)
	}
}

// Runs that began at the same instant are ordered by ID, newest ID first, in the history and in the
// prune's newest N alike.
func TestRetentionTieOrder(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	old := f.now.Add(-30 * day)
	for _, id := range []string{"b", "d", "a", "c"} {
		f.add(t, "env-1", id, StateCompleted, old)
	}
	listed, err := f.s.ListRuns(ctx, "env-1", 2)
	if err != nil || len(listed) != 2 || listed[0].ID != "d" || listed[1].ID != "c" {
		t.Fatalf("ListRuns(2) = %+v, %v; want d then c", listed, err)
	}
	if n, err := f.s.PruneRuns(ctx, "env-1", Retention{Days: 1, Runs: 2}, f.now); err != nil || n != 2 {
		t.Fatalf("PruneRuns = %d, %v; want a and b pruned", n, err)
	}
	if got := f.ids(t, "env-1"); !slices.Equal(got, []string{"d", "c"}) {
		t.Fatalf("kept %v, want the very runs the list shows first: d and c", got)
	}
}

// A run that is waiting or running is never deleted, however old, and does not use up a place of the
// newest N unless it is among them.
func TestRetentionNeverActive(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	ancient := f.now.Add(-400 * day)
	f.add(t, "env-1", "waits", StateWaiting, ancient)
	f.add(t, "env-1", "runs", StateRunning, ancient.Add(time.Hour))
	for _, st := range []string{StateCompleted, StatePartial, StateSkipped, StateFailed} {
		f.add(t, "env-1", "old-"+st, st, ancient.Add(2*time.Hour))
	}
	f.add(t, "env-1", "recent", StateCompleted, f.now.Add(-day))
	if n, err := f.s.PruneAll(ctx, Retention{Days: 90, Runs: 1}, f.now); err != nil || n != 4 {
		t.Fatalf("PruneAll = %d, %v; want the four finished old runs pruned", n, err)
	}
	if got := f.ids(t, "env-1"); !slices.Equal(got, []string{"recent", "runs", "waits"}) {
		t.Fatalf("kept %v, want the recent run and the two active ones", got)
	}
}

// One environment's prune leaves the others alone, and PruneAll does each by its own count.
func TestPruneRunsOfOneEnvironment(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	for i := range 5 {
		f.add(t, "env-1", runName(i), StateCompleted, f.now.Add(-time.Duration(100+i)*day))
		f.add(t, "env-2", "two-"+runName(i), StateCompleted, f.now.Add(-time.Duration(100+i)*day))
	}
	if n, err := f.s.PruneRuns(ctx, "env-1", Retention{Days: 90, Runs: 2}, f.now); err != nil || n != 3 {
		t.Fatalf("PruneRuns = %d, %v; want 3", n, err)
	}
	if len(f.ids(t, "env-1")) != 2 || len(f.ids(t, "env-2")) != 5 {
		t.Fatalf("env-1 keeps %d and env-2 keeps %d, want 2 and 5", len(f.ids(t, "env-1")), len(f.ids(t, "env-2")))
	}
	if n, err := f.s.PruneAll(ctx, Retention{Days: 90, Runs: 3}, f.now); err != nil || n != 2 {
		t.Fatalf("PruneAll = %d, %v; want env-2's 2 pruned and env-1's 2 kept", n, err)
	}
	if len(f.ids(t, "env-1")) != 2 || len(f.ids(t, "env-2")) != 3 {
		t.Fatalf("env-1 keeps %d and env-2 keeps %d, want 2 and 3", len(f.ids(t, "env-1")), len(f.ids(t, "env-2")))
	}
}

// A retention that would keep nothing is a bug in the caller, never a request to delete the history.
func TestRetentionRefusesToKeepNothing(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	f.add(t, "env-1", "old", StateCompleted, f.now.Add(-400*day))
	for _, r := range []Retention{{Days: 0, Runs: 100}, {Days: 90, Runs: 0}, {Days: -1, Runs: -1}} {
		if _, err := f.s.PruneRuns(ctx, "env-1", r, f.now); !errors.Is(err, ErrRetentionTooLow) {
			t.Errorf("PruneRuns(%+v) = %v, want ErrRetentionTooLow", r, err)
		}
		if _, err := f.s.PruneAll(ctx, r, f.now); !errors.Is(err, ErrRetentionTooLow) {
			t.Errorf("PruneAll(%+v) = %v, want ErrRetentionTooLow", r, err)
		}
	}
	if len(f.ids(t, "env-1")) != 1 {
		t.Fatal("a refused prune deleted a run")
	}
}

func TestRetentionRanges(t *testing.T) {
	t.Parallel()
	if DefaultRetention != (Retention{Days: 90, Runs: 100}) {
		t.Errorf("DefaultRetention = %+v, want 90 days and 100 runs", DefaultRetention)
	}
	if MinRetentionDays != 7 || MaxRetentionDays != 3650 || MinRetentionRuns != 10 || MaxRetentionRuns != 1000 {
		t.Errorf("ranges = %d-%d days, %d-%d runs; want 7-3650 and 10-1000", MinRetentionDays, MaxRetentionDays, MinRetentionRuns, MaxRetentionRuns)
	}
	for r, want := range map[Retention]bool{
		{90, 100}: true, {7, 10}: true, {3650, 1000}: true, {6, 100}: false, {3651, 100}: false, {90, 9}: false, {90, 1001}: false,
	} {
		if r.InRange() != want {
			t.Errorf("%+v.InRange() = %v, want %v", r, r.InRange(), want)
		}
	}
}

// After each run finishes, the run's environment is pruned by the default retention, once the run is
// on record as finished (T-11-41).
func TestRunnerPrunesItsEnvironmentWhenARunFinishes(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	mem := recordOf(run)
	var wasFinished bool
	mem.onPrune = func(m *memStore, envID string) {
		for _, r := range m.runs {
			wasFinished = wasFinished || (r.EnvID == envID && !r.FinishedAt.IsZero())
		}
	}
	finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	got := mem.prunes()
	if len(got) != 1 || got[0].env != "env-1" || got[0].keep != DefaultRetention || !got[0].now.Equal(t0) {
		t.Fatalf("prunes = %+v, want one of env-1 by the default retention at the run's clock", got)
	}
	if !wasFinished {
		t.Error("the environment was pruned before the run was on record as finished")
	}

	// A run that fails to plan finishes too, and prunes; a prune that fails leaves the run as it ended.
	mem.pruneErr = errors.New("disk full")
	eng.snapErr = errors.New("no engine")
	got2, _ := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	if got2.State != StateFailed || len(mem.prunes()) != 2 {
		t.Fatalf("run = %+v after %d prunes, want failed on record and a prune tried", got2, len(mem.prunes()))
	}
}

// After a run the environment is pruned by the retention the policy stores, not by the built-in one
// (plan 11-21, D-16). A retention that cannot be read prunes nothing: it is not guessed.
func TestRunnerUsesStoredRetention(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	mem := recordOf(run)
	stored := Retention{Days: 30, Runs: 12}
	run.Retention = func(context.Context) (Retention, error) { return stored, nil }
	req := RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")}
	finished(t, run, hub, req)
	if got := mem.prunes(); len(got) != 1 || got[0].keep != stored {
		t.Fatalf("prunes = %+v, want one by the stored retention %+v", got, stored)
	}

	// Saving another retention reaches the next run.
	stored = Retention{Days: 365, Runs: 500}
	finished(t, run, hub, req)
	if got := mem.prunes(); len(got) != 2 || got[1].keep != stored {
		t.Fatalf("prunes = %+v, want the second by %+v", got, stored)
	}

	// A retention that cannot be read is not replaced by a guess: the run has ended and nothing is pruned.
	run.Retention = func(context.Context) (Retention, error) { return Retention{}, errors.New("database is locked") }
	got, _ := finished(t, run, hub, req)
	if got.FinishedAt.IsZero() || len(mem.prunes()) != 2 {
		t.Errorf("run %+v after %d prunes, want it finished and no third prune", got, len(mem.prunes()))
	}
}

// seedHistory puts runs on record: env-1 has 130 completed runs five days apart, the oldest 650 days back,
// env-2 has 25 twelve days apart, and env-1 has one run that has been running for 400 days.
func (f retentionFix) seedHistory(t *testing.T) {
	t.Helper()
	for i := range 130 {
		f.add(t, "env-1", runName(i), StateCompleted, f.now.Add(-time.Duration(i)*5*day))
	}
	for i := range 25 {
		f.add(t, "env-2", "two-"+runName(i), StateCompleted, f.now.Add(-time.Duration(i)*12*day))
	}
	f.add(t, "env-1", "stuck", StateRunning, f.now.Add(-400*day))
}

// save runs SetDefaults on a transaction of its own; it commits when commit is true.
func (f retentionFix) saveRetention(t *testing.T, ps PolicyStore, r Retention, commit bool) (int64, error) {
	t.Helper()
	ctx := testsupport.Context(t)
	pol, _, version, err := ps.Defaults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := ps.SetDefaults(ctx, f.st.Q.WithTx(tx), pol, r, version, "ada", f.now)
	if err != nil || !commit {
		_ = tx.Rollback()
		return removed, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return removed, nil
}

// The count a preview gives is what the save deletes, the save deletes it with the runs' items and evidence
// and never a run that is working, and a save that does not commit deletes nothing and keeps the old retention
// (plan 11-21, JAN-10, D-16, T-11-55).
func TestPolicyStoreRetentionPreviewMatchesSave(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	ps := PolicyStore{St: f.st}
	f.seedHistory(t)
	// An item and evidence on the oldest completed run of env-1, which a lower retention deletes.
	oldest := runName(129)
	if err := f.s.AddItems(ctx, oldest, []RunItem{{Seq: 1, Type: KindContainer, Ref: "c1", Outcome: OutcomeRemoved, Size: -1}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SaveEvidence(ctx, oldest, 1, Evidence{ExitCode: 1, State: EvidenceEmpty}); err != nil {
		t.Fatal(err)
	}

	// 30 days or the newest 10: env-1 keeps its newest 10 (the 7 within 30 days are among them) and env-2 its
	// newest 10 (3 are within 30 days); the stuck run is never counted.
	want := Retention{Days: 30, Runs: 10}
	runs, envs, err := ps.RetentionPreview(ctx, want, f.now)
	if err != nil || runs != 120+15 || envs != 2 {
		t.Fatalf("RetentionPreview = %d runs of %d environments, %v; want 135 of 2", runs, envs, err)
	}
	// Previewing deletes nothing.
	if n := f.count(t, "janitor_runs"); n != 130+25+1 {
		t.Fatalf("runs = %d after a preview, want all 156", n)
	}

	// A save that does not commit leaves the runs and the old retention.
	if removed, err := f.saveRetention(t, ps, want, false); err != nil || removed != 135 {
		t.Fatalf("an uncommitted SetDefaults = %d, %v, want 135 removed inside the transaction", removed, err)
	}
	if n := f.count(t, "janitor_runs"); n != 156 {
		t.Errorf("runs = %d after a rolled-back save, want all 156", n)
	}
	if r, err := ps.Retention(ctx); err != nil || r != DefaultRetention {
		t.Errorf("retention = %+v, %v after a rolled-back save, want the old one", r, err)
	}

	removed, err := f.saveRetention(t, ps, want, true)
	if err != nil || int(removed) != runs {
		t.Fatalf("SetDefaults removed %d, %v; the preview said %d", removed, err, runs)
	}
	if f.count(t, "janitor_runs") != 156-135 || len(f.ids(t, "env-1")) != 10+1 || len(f.ids(t, "env-2")) != 10 {
		t.Errorf("runs left = %d (env-1 %d, env-2 %d), want 21: 10 + the stuck one, and 10", f.count(t, "janitor_runs"), len(f.ids(t, "env-1")), len(f.ids(t, "env-2")))
	}
	if !slices.Contains(f.ids(t, "env-1"), "stuck") {
		t.Error("the run that is still running was deleted")
	}
	if f.count(t, "janitor_run_items") != 0 || f.count(t, "janitor_evidence") != 0 {
		t.Errorf("items/evidence = %d/%d, want the deleted run's gone with it", f.count(t, "janitor_run_items"), f.count(t, "janitor_evidence"))
	}
	if r, err := ps.Retention(ctx); err != nil || r != want {
		t.Errorf("retention = %+v, %v, want %+v", r, err, want)
	}
	// The same retention again has nothing left to delete.
	if runs, envs, err := ps.RetentionPreview(ctx, want, f.now); err != nil || runs != 0 || envs != 0 {
		t.Errorf("RetentionPreview of the stored retention = %d/%d, %v, want 0/0", runs, envs, err)
	}
}

// Raising either limit deletes nothing, even where the history holds runs the old retention would no longer
// keep, and the preview says so (plan 11-21, D-16).
func TestPolicyStoreRetentionRaiseDeletesNothing(t *testing.T) {
	t.Parallel()
	f := newRetentionFix(t)
	ctx := testsupport.Context(t)
	ps := PolicyStore{St: f.st}
	f.seedHistory(t) // env-1 holds 30 runs the default 90 days or 100 runs has stopped keeping
	for name, r := range map[string]Retention{
		"days up":       {Days: 100, Runs: 100},
		"runs up":       {Days: 90, Runs: 101},
		"both up":       {Days: 365, Runs: 500},
		"the same ones": DefaultRetention,
	} {
		if runs, envs, err := ps.RetentionPreview(ctx, r, f.now); err != nil || runs != 0 || envs != 0 {
			t.Errorf("%s: RetentionPreview = %d/%d, %v, want 0/0", name, runs, envs, err)
		}
	}
	removed, err := f.saveRetention(t, ps, Retention{Days: 365, Runs: 500}, true)
	if err != nil || removed != 0 || f.count(t, "janitor_runs") != 156 {
		t.Fatalf("raising: removed %d, %v with %d runs left, want nothing deleted", removed, err, f.count(t, "janitor_runs"))
	}
	// A retention outside the ranges is refused, not previewed.
	if _, _, err := ps.RetentionPreview(ctx, Retention{Days: 6, Runs: 10}, f.now); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("RetentionPreview(6 days) = %v, want ErrOutOfRange", err)
	}
}
