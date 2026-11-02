// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Crash evidence (plan 11-13, JAN-10, D-06, RESEARCH F8, Pitfall 5): a container that exited with a
// code other than 0 has its facts and the masked, capped tail of its log saved before it is removed.

// lines makes n log lines of size bytes each, numbered so the newest is recognisable.
func lines(n, size int) []string {
	out := make([]string, 0, n)
	for i := range n {
		l := fmt.Sprintf("line %04d ", i)
		out = append(out, l+strings.Repeat("x", size-len(l)))
	}
	return out
}

func TestBuildEvidenceMasksAndCaps(t *testing.T) {
	t.Parallel()

	t.Run("the last 200 of 500 lines", func(t *testing.T) {
		t.Parallel()
		ev := BuildEvidence(EvidenceSource{Lines: lines(500, 20)})
		if len(ev.Lines) != EvidenceLines || ev.State != EvidenceSaved || !strings.HasPrefix(ev.Lines[0], "line 0300 ") || !strings.HasPrefix(ev.Lines[199], "line 0499 ") {
			t.Fatalf("evidence = %d lines %q state %s, want the newest 200 (300..499) saved", len(ev.Lines), ev.Lines[:1], ev.State)
		}
	})

	t.Run("the newest lines that fit 32 KiB", func(t *testing.T) {
		t.Parallel()
		ev := BuildEvidence(EvidenceSource{Lines: lines(200, 1024)})
		bytes := 0
		for _, l := range ev.Lines {
			bytes += len(l) + 1
		}
		// 31 lines of 1024 bytes and their line breaks fit 32768 bytes; a 32nd does not.
		if len(ev.Lines) != 31 || bytes > MaxEvidenceBytes || !strings.HasPrefix(ev.Lines[30], "line 0199 ") {
			t.Fatalf("evidence = %d lines in %d bytes, want the newest 31 within %d", len(ev.Lines), bytes, MaxEvidenceBytes)
		}
	})

	t.Run("every line is masked and cut", func(t *testing.T) {
		t.Parallel()
		src := EvidenceSource{
			End:     ContainerEnd{ExitCode: 1, Error: "start failed with token=abc12345"},
			Lines:   []string{"login password=hunter2", "env value s3cr3t-env here", "stack value STACKVAL99 here", strings.Repeat("y", 10000)},
			Secrets: []string{"s3cr3t-env"},
			Mask:    func(s string) string { return strings.ReplaceAll(s, "STACKVAL99", "<hidden>") },
		}
		ev := BuildEvidence(src)
		joined := strings.Join(ev.Lines, "\n") + ev.Error
		for _, leaked := range []string{"hunter2", "s3cr3t-env", "STACKVAL99", "abc12345"} {
			if strings.Contains(joined, leaked) {
				t.Errorf("the evidence holds %q: %q %q", leaked, ev.Lines, ev.Error)
			}
		}
		if n := utf8.RuneCountInString(ev.Lines[3]); n != 2048 {
			t.Errorf("a 10,000 character line was kept as %d characters, want 2048", n)
		}
		if ev.Lines[0] != "login password=<hidden>" || ev.Error != "start failed with token=<hidden>" {
			t.Errorf("lines[0] = %q error = %q, want the pairs masked", ev.Lines[0], ev.Error)
		}
	})

	t.Run("the facts come from the end", func(t *testing.T) {
		t.Parallel()
		started := t0.Add(-time.Hour)
		ev := BuildEvidence(EvidenceSource{
			End:         ContainerEnd{ExitCode: 137, OOMKilled: true, StartedAt: started, FinishedAt: t0, Image: "nginx:1", ImageID: "sha256:abc"},
			ImageDigest: "nginx@sha256:def", Node: "n1",
		})
		if ev.ExitCode != 137 || !ev.OOMKilled || !ev.StartedAt.Equal(started) || !ev.FinishedAt.Equal(t0) || ev.Image != "nginx:1" || ev.ImageID != "sha256:abc" || ev.ImageDigest != "nginx@sha256:def" || ev.Node != "n1" {
			t.Fatalf("evidence = %+v, want the facts of the end, the digest and the node", ev)
		}
		if ev.State != EvidenceEmpty || len(ev.Lines) != 0 {
			t.Fatalf("state = %s lines %q, want empty: the container wrote no log", ev.State, ev.Lines)
		}
	})
}

// startAll starts a run for everything removable and waits for its job.
func startAll(t *testing.T, run *Runner, hub *jobs.Hub) (string, jobs.Job) {
	t.Helper()
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	return started.RunID, job
}

func TestEvidenceNotForCleanExit(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	runID, _ := startAll(t, run, hub)
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1", "c2", "c3"}) {
		t.Fatalf("removed = %v, want c1 c2 c3", got)
	}
	if n := recordOf(run).evidenceCount(); n != 0 {
		t.Fatalf("%d evidence rows for containers that exited 0, want none", n)
	}
	if len(eng.evidenceCalls) != 0 {
		t.Fatalf("the engine was asked for the logs of %v, want no container", eng.evidenceCalls)
	}
	if _, err := recordOf(run).GetEvidence(testsupport.Context(t), "env-1", runID, 1); !errors.Is(err, ErrEvidenceNotFound) {
		t.Fatalf("GetEvidence = %v, want ErrEvidenceNotFound", err)
	}
}

func TestEvidenceSavedBeforeTheRemove(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.exitCodes["c2"] = 1
	eng.sources["c2"] = EvidenceSource{End: ContainerEnd{ExitCode: 1}, Lines: []string{"boom"}}
	run, _, hub := newRunner(t, eng)
	var savedWhenRemoved int
	eng.onRemove = func(id string) {
		if id == "c2" {
			savedWhenRemoved = recordOf(run).evidenceCount()
		}
	}
	runID, _ := startAll(t, run, hub)
	if savedWhenRemoved != 1 {
		t.Fatalf("%d pieces of evidence were saved when c2 was removed, want 1: evidence comes first", savedWhenRemoved)
	}
	// c2 is the second item the run attempted.
	ev, err := recordOf(run).GetEvidence(testsupport.Context(t), "env-1", runID, 2)
	if err != nil || ev.ExitCode != 1 || ev.State != EvidenceSaved || !slices.Equal(ev.Lines, []string{"boom"}) || ev.Node != "n1" {
		t.Fatalf("evidence of item 2 = %+v, %v; want exit 1 with its line, on node n1", ev, err)
	}
}

func TestEvidencePerRunCap(t *testing.T) {
	t.Parallel()
	const n = MaxEvidencePerRun + 3 // 201 removable containers beside the two newest
	eng := newFakeEngine(n)
	eng.exitCodes = map[string]int{}
	eng.sources = map[string]EvidenceSource{}
	for i := 1; i <= n-2; i++ {
		id := fmt.Sprintf("c%d", i)
		eng.exitCodes[id] = 2
		eng.sources[id] = EvidenceSource{End: ContainerEnd{ExitCode: 2}, Lines: []string{"crash of " + id}}
	}
	run, _, hub := newRunner(t, eng)
	runID, job := startAll(t, run, hub)

	if job.State != jobs.JobDone || len(eng.removedIDs()) != n-2 {
		t.Fatalf("job %s removed %d, want every one of the %d removed: the cap never keeps a container", job.State, len(eng.removedIDs()), n-2)
	}
	if len(eng.evidenceCalls) != MaxEvidencePerRun {
		t.Fatalf("the engine was asked for the logs of %d containers, want %d: past the cap no log is read", len(eng.evidenceCalls), MaxEvidencePerRun)
	}
	ctx := testsupport.Context(t)
	// The planner orders the plan by name, not by number, so find the items by what they hold.
	var withLog, capped int
	for seq := 1; seq <= n-2; seq++ {
		ev, err := recordOf(run).GetEvidence(ctx, "env-1", runID, seq)
		if err != nil {
			t.Fatalf("no evidence for item %d: %v", seq, err)
		}
		switch ev.State {
		case EvidenceSaved:
			withLog++
		case EvidenceCapReached:
			capped++
			if len(ev.Lines) != 0 || ev.ExitCode != 2 {
				t.Errorf("the item past the cap = %+v, want its facts and no log", ev)
			}
		}
	}
	if withLog != MaxEvidencePerRun || capped != 1 {
		t.Fatalf("%d items with a log and %d past the cap, want %d and 1", withLog, capped, MaxEvidencePerRun)
	}
}

func TestEvidenceLogsUnavailable(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(4)
	eng.exitCodes["c1"] = 1
	// c1's log driver cannot be read back.
	eng.sources["c1"] = EvidenceSource{End: ContainerEnd{ExitCode: 1}, LogsUnavailable: true}
	run, _, hub := newRunner(t, eng)
	runID, job := startAll(t, run, hub)
	if job.State != jobs.JobDone || !slices.Equal(eng.removedIDs(), []string{"c1", "c2"}) {
		t.Fatalf("job %s removed %v, want c1 and c2 removed whatever the logs said", job.State, eng.removedIDs())
	}
	ev, err := recordOf(run).GetEvidence(testsupport.Context(t), "env-1", runID, 1)
	if err != nil || ev.State != EvidenceUnavailable || len(ev.Lines) != 0 || ev.ExitCode != 1 {
		t.Fatalf("evidence = %+v, %v; want unavailable with the exit code", ev, err)
	}
}

func TestEvidenceNothingReadableStillRemoves(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(4)
	eng.exitCodes["c1"] = 9
	eng.evidenceErr = errors.New("the node did not answer")
	run, _, hub := newRunner(t, eng)
	runID, job := startAll(t, run, hub)
	if job.State != jobs.JobDone || !slices.Equal(eng.removedIDs(), []string{"c1", "c2"}) {
		t.Fatalf("job %s removed %v, want both removed", job.State, eng.removedIDs())
	}
	ev, err := recordOf(run).GetEvidence(testsupport.Context(t), "env-1", runID, 1)
	if err != nil || ev.State != EvidenceUnavailable || ev.ExitCode != 9 {
		t.Fatalf("evidence = %+v, %v; want what the fresh inspect said, with the log unavailable", ev, err)
	}
}

func TestEvidenceEmptyLog(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(3)
	eng.exitCodes["c1"] = 3
	run, _, hub := newRunner(t, eng)
	runID, _ := startAll(t, run, hub)
	ev, err := recordOf(run).GetEvidence(testsupport.Context(t), "env-1", runID, 1)
	if err != nil || ev.State != EvidenceEmpty || len(ev.Lines) != 0 || ev.ExitCode != 3 {
		t.Fatalf("evidence = %+v, %v; want empty with the exit code", ev, err)
	}
}

func TestEvidenceUnsavedBlocksRemove(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.exitCodes["c1"] = 137
	run, _, hub := newRunner(t, eng)
	recordOf(run).evidenceErr = errors.New("database is locked")
	runID, job := startAll(t, run, hub)

	if got := eng.removeCalls(); !slices.Equal(got, []string{"c2", "c3"}) {
		t.Fatalf("the engine was asked to remove %v, want c1 never: its evidence was not saved", got)
	}
	rec, items, err := recordOf(run).GetRun(testsupport.Context(t), "env-1", runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Ref != "c1" || items[0].Outcome != OutcomeError || items[0].Code != CodeEvidenceUnsaved || items[0].HasEvidence {
		t.Fatalf("items = %+v, want c1 an error with the code evidence_unsaved", items)
	}
	if rec.Counts.Failed != 1 || rec.Counts.RemovedContainers != 2 || rec.State != StatePartial {
		t.Fatalf("run = %+v, want one failure, two removals and a partial run", rec)
	}
	if job.Items[0].Code != CodeEvidenceUnsaved || job.Items[0].State != jobs.ItemFailed {
		t.Fatalf("job item = %+v, want c1 failed with evidence_unsaved", job.Items[0])
	}
}

// SQLite: the evidence round-trips, the run lists which items have it, and it goes with its run.
func TestEvidenceCascades(t *testing.T) {
	t.Parallel()
	st := storetest.New(t)
	ctx := testsupport.Context(t)
	if _, err := st.Write.ExecContext(ctx, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-1', 'agent', 'e', '', 1, 1, 1), ('env-2', 'agent', 'f', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	s := &SQLStore{St: st}
	if err := s.CreateRun(ctx, Run{ID: "run-1", EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", State: StateRunning, StartedAt: t0, EstBytes: -1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddItems(ctx, "run-1", []RunItem{
		{Seq: 1, Type: KindContainer, Ref: "c1", Outcome: OutcomeRemoved, Size: -1},
		{Seq: 2, Type: KindContainer, Ref: "c2", Outcome: OutcomeRemoved, Size: -1},
	}); err != nil {
		t.Fatal(err)
	}
	in := Evidence{
		ExitCode: 137, Error: "oom", OOMKilled: true, StartedAt: t0.Add(-time.Hour), FinishedAt: t0, Image: "nginx:1", ImageID: "sha256:a",
		ImageDigest: "nginx@sha256:b", Node: "n1", Lines: []string{"one", "two", "three"}, State: EvidenceSaved,
	}
	if err := s.SaveEvidence(ctx, "run-1", 1, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEvidence(ctx, "env-1", "run-1", 1)
	if err != nil || !evidenceEqual(got, in) {
		t.Fatalf("GetEvidence = %+v, %v; want %+v", got, err, in)
	}
	// Only the item that has evidence says so, and no query for the items reads the log.
	_, items, err := s.GetRun(ctx, "env-1", "run-1")
	if err != nil || len(items) != 2 || !items[0].HasEvidence || items[1].HasEvidence {
		t.Fatalf("items = %+v, %v; want only item 1 flagged", items, err)
	}
	// Another environment, another item and another run have none.
	for _, c := range []struct {
		env, run string
		seq      int
	}{{"env-2", "run-1", 1}, {"env-1", "run-1", 2}, {"env-1", "run-9", 1}} {
		if _, err := s.GetEvidence(ctx, c.env, c.run, c.seq); !errors.Is(err, ErrEvidenceNotFound) {
			t.Errorf("GetEvidence(%s, %s, %d) = %v, want ErrEvidenceNotFound", c.env, c.run, c.seq, err)
		}
	}
	// A run with no evidence-less state, no times: the zero times stay zero.
	empty := Evidence{ExitCode: 1, State: EvidenceEmpty}
	if err := s.SaveEvidence(ctx, "run-1", 2, empty); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetEvidence(ctx, "env-1", "run-1", 2); err != nil || !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() || got.Lines != nil {
		t.Fatalf("GetEvidence = %+v, %v; want zero times and no lines", got, err)
	}

	// Deleting the run deletes its evidence (the prune of a run, D-16).
	if _, err := st.Write.ExecContext(ctx, `DELETE FROM janitor_runs WHERE id = 'run-1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM janitor_evidence`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d evidence rows left after the run was deleted (%v), want none", n, err)
	}
}

func evidenceEqual(a, b Evidence) bool {
	return a.ExitCode == b.ExitCode && a.Error == b.Error && a.OOMKilled == b.OOMKilled &&
		a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt) && a.Image == b.Image &&
		a.ImageID == b.ImageID && a.ImageDigest == b.ImageDigest && a.Node == b.Node && a.State == b.State &&
		slices.Equal(a.Lines, b.Lines)
}
