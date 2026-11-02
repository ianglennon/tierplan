// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
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
