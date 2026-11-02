// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The runner (plan 11-04, JAN-09, D-08, D-14): the engine is a fake behind the port, the lock and
// the jobs hub are the real ones.

// fixedClock is a clock that stands still at t0, so the containers of these tests are two days old.
type fixedClock struct{}

func (fixedClock) Now() time.Time                       { return t0 }
func (fixedClock) After(time.Duration) <-chan time.Time { return nil }

// fakeEngine serves a snapshot of one slot of exited containers and records what is removed.
type fakeEngine struct {
	snap Snapshot
	// gate, when set, holds Snapshot until it is closed or the context ends; entered is closed
	// when Snapshot is first called.
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once

	// snapErr, when set, is what Snapshot answers.
	snapErr error

	mu        sync.Mutex
	removeErr map[string]error
	removed   []string
	// calls are the IDs RemoveContainer was asked for, whatever it answered.
	calls []string
	// onRemove, when set, is called with the ID of each container the engine is asked to remove,
	// before it answers: a test reads the record of the run there.
	onRemove func(id string)
	// onRemoveImage is the same for images.
	onRemoveImage func(id string)
	// exitCodes are the exit codes Inspect reports, by container ID; a container not in it exited 0.
	exitCodes map[string]int
	// sources are what Evidence answers, by container ID; a container not in it answers an empty source.
	sources map[string]EvidenceSource
	// evidenceErr is what Evidence answers when set.
	evidenceErr error
	// evidenceCalls are the IDs Evidence was asked for.
	evidenceCalls []string
}

func newFakeEngine(n int) *fakeEngine {
	var cs []Container
	for i := range n {
		cs = append(cs, task(fmt.Sprintf("c%d", i+1), "1", i))
	}
	return &fakeEngine{
		snap: swarmOf(cs...), entered: make(chan struct{}), removeErr: map[string]error{},
		exitCodes: map[string]int{}, sources: map[string]EvidenceSource{},
	}
}

func (e *fakeEngine) Snapshot(ctx context.Context, _ string) (Snapshot, error) {
	e.once.Do(func() { close(e.entered) })
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
	}
	if e.snapErr != nil {
		return Snapshot{}, e.snapErr
	}
	return e.snap, nil
}

func (e *fakeEngine) Inspect(_ context.Context, _, _ string, ids []string) (map[string]ContainerEnd, error) {
	out := map[string]ContainerEnd{}
	for _, id := range ids {
		out[id] = ContainerEnd{FinishedAt: t0.Add(-48 * time.Hour), SizeRw: -1, ExitCode: e.exitCodes[id]}
	}
	return out, nil
}

func (e *fakeEngine) Evidence(_ context.Context, _, _, id string) (EvidenceSource, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evidenceCalls = append(e.evidenceCalls, id)
	if e.evidenceErr != nil {
		return EvidenceSource{}, e.evidenceErr
	}
	src, ok := e.sources[id]
	if !ok {
		src = EvidenceSource{End: ContainerEnd{ExitCode: e.exitCodes[id]}}
	}
	return src, nil
}

func (e *fakeEngine) RemoveContainer(_ context.Context, _, _, id string) error {
	if e.onRemove != nil {
		e.onRemove(id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, id)
	if err := e.removeErr[id]; err != nil {
		return err
	}
	e.removed = append(e.removed, id)
	return nil
}

// RemoveImage records the removal in the same order as RemoveContainer, as "image:"+id.
func (e *fakeEngine) RemoveImage(_ context.Context, _, _, id string) error {
	if e.onRemoveImage != nil {
		e.onRemoveImage(id)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, "image:"+id)
	if err := e.removeErr["image:"+id]; err != nil {
		return err
	}
	e.removed = append(e.removed, "image:"+id)
	return nil
}

func (e *fakeEngine) removeCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

func (e *fakeEngine) removedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.removed)
}

func newRunner(t *testing.T, eng Engine) (*Runner, *stackops.Locks, *jobs.Hub) {
	t.Helper()
	locks := stackops.NewLocks()
	hub := jobs.New(jobs.Options{})
	t.Cleanup(hub.Close)
	return &Runner{Engine: eng, Locks: locks, Jobs: hub, Clock: fixedClock{}, Store: newMemStore()}, locks, hub
}

// memStore is a RunStore in memory that keeps the batches it was given, so a test sees how the
// items were written.
type memStore struct {
	mu      sync.Mutex
	runs    map[string]Run
	items   map[string][]RunItem
	batches [][]RunItem
	// evidence is the evidence saved, by run and sequence number; evidenceErr is what SaveEvidence
	// answers when set, and saves records the order the sequence numbers were saved in.
	evidence    map[string]Evidence
	evidenceErr error
	saves       []string
	// pruned are the prunes asked for, pruneErr what they answer when set, and onPrune is called with
	// the lock held when one is asked for.
	pruned   []prunedEnv
	pruneErr error
	onPrune  func(m *memStore, envID string)
}

// prunedEnv is a prune a memStore was asked for.
type prunedEnv struct {
	env  string
	keep Retention
	now  time.Time
}

func (m *memStore) PruneRuns(_ context.Context, envID string, keep Retention, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruned = append(m.pruned, prunedEnv{envID, keep, now})
	if m.onPrune != nil {
		m.onPrune(m, envID)
	}
	return 0, m.pruneErr
}

func (m *memStore) PruneAll(_ context.Context, keep Retention, now time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruned = append(m.pruned, prunedEnv{"", keep, now})
	return 0, m.pruneErr
}

func (m *memStore) prunes() []prunedEnv {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.pruned)
}

func newMemStore() *memStore {
	return &memStore{runs: map[string]Run{}, items: map[string][]RunItem{}, evidence: map[string]Evidence{}}
}

func evidenceKey(runID string, seq int) string { return fmt.Sprintf("%s/%d", runID, seq) }

func (m *memStore) SaveEvidence(_ context.Context, runID string, seq int, ev Evidence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.evidenceErr != nil {
		return m.evidenceErr
	}
	m.evidence[evidenceKey(runID, seq)] = ev
	m.saves = append(m.saves, evidenceKey(runID, seq))
	return nil
}

func (m *memStore) GetEvidence(_ context.Context, envID, runID string, seq int) (Evidence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	ev, has := m.evidence[evidenceKey(runID, seq)]
	if !ok || run.EnvID != envID || !has {
		return Evidence{}, ErrEvidenceNotFound
	}
	return ev, nil
}

// evidenceCount is how many pieces of evidence were saved.
func (m *memStore) evidenceCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.evidence)
}

func (m *memStore) CreateRun(_ context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	return nil
}

func (m *memStore) SetState(_ context.Context, runID, state, jobID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok || !run.FinishedAt.IsZero() {
		return nil
	}
	run.State, run.JobID = state, jobID
	m.runs[runID] = run
	return nil
}

func (m *memStore) AddItems(_ context.Context, runID string, items []RunItem) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[runID] = append(m.items[runID], items...)
	m.batches = append(m.batches, slices.Clone(items))
	return nil
}

func (m *memStore) FinishRun(_ context.Context, run Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	was, ok := m.runs[run.ID]
	if !ok {
		return ErrRunNotFound
	}
	run.EnvID, run.Trigger, run.Actor, run.JobID, run.StartedAt = was.EnvID, was.Trigger, was.Actor, was.JobID, was.StartedAt
	m.runs[run.ID] = run
	return nil
}

func (m *memStore) GetRun(_ context.Context, envID, runID string) (Run, []RunItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok || run.EnvID != envID {
		return Run{}, nil, ErrRunNotFound
	}
	return run, slices.Clone(m.items[runID]), nil
}

func (m *memStore) ListRuns(_ context.Context, envID string, limit int) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, run := range m.runs {
		if run.EnvID == envID {
			out = append(out, run)
		}
	}
	slices.SortFunc(out, func(a, b Run) int {
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) CountRuns(_ context.Context, envID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, run := range m.runs {
		if run.EnvID == envID {
			n++
		}
	}
	return n, nil
}

// recordOf is the store of a runner made by newRunner.
func recordOf(r *Runner) *memStore {
	m, ok := r.Store.(*memStore)
	if !ok {
		panic("the runner's store is not a memStore")
	}
	return m
}

func ticked(ids ...string) []ItemRef {
	var out []ItemRef
	for _, id := range ids {
		out = append(out, ItemRef{Type: KindContainer, NodeID: "n1-id", Ref: id})
	}
	return out
}

func TestRunnerHoldsLockOnlyWhileRunning(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // c1..c3 are beyond the two newest
	eng.gate = make(chan struct{})
	run, locks, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)

	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	testsupport.Await(t, "the run to read the environment", eng.entered)
	held, ok := locks.Held("env-1")
	if !ok || held.Kind != stackops.HolderKindJanitor || held.By != "ada" || held.JobID != started.JobID || held.RunID != started.RunID || held.StackID != "" || held.StackName != "" {
		t.Fatalf("held = %+v %v, want a janitor holder by ada with the run and its job and no stack", held, ok)
	}
	// A second run, or a deploy, finds the environment taken.
	_, err = run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "bob", Owner: "user:u2", Ticked: ticked("c2")})
	var locked *LockedError
	if !errors.As(err, &locked) || locked.Holder.JobID != started.JobID {
		t.Fatalf("a second Start = %v, want a LockedError naming the first run's job", err)
	}
	if _, _, ok := locks.TryAcquire("env-2", stackops.Holder{Kind: "deploy"}); !ok {
		t.Fatal("a run on env-1 held the lock of env-2")
	}

	close(eng.gate)
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone {
		t.Fatalf("job = %s items %+v, want done with one removal", job.State, job.Items)
	}
	if held, ok := locks.Held("env-1"); ok {
		t.Fatalf("the job reads finished and the lock is still held by %+v", held)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("removed = %v, want c1 only", got)
	}
}

func TestRunnerJobOwner(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	locks := stackops.NewLocks()
	hub := jobs.New(jobs.Options{MaxRunningPerUser: 1})
	t.Cleanup(hub.Close)
	run := &Runner{Engine: eng, Locks: locks, Jobs: hub, Clock: fixedClock{}, Store: newMemStore()}
	ctx := testsupport.Context(t)

	// The person already has the most jobs they may have running.
	if _, err := hub.Start(jobs.Spec{Kind: "pull", Owner: "user:u1"}, func(ctx context.Context, _ *jobs.Reporter) { <-ctx.Done() }); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")}); !errors.Is(err, jobs.ErrTooMany) {
		t.Fatalf("Clean now with the cap reached = %v, want jobs.ErrTooMany", err)
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("a run the hub refused kept the lock")
	}

	// A scheduled run is owned by the environment, so that cap never blocks it.
	owner := "system:janitor:env-1"
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: owner, All: true})
	if err != nil {
		t.Fatalf("a scheduled run: %v", err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Owner != owner || job.Kind != JobKind || job.EnvID != "env-1" || job.Meta["runId"] != started.RunID || job.Meta["trigger"] != TriggerSchedule {
		t.Fatalf("job = %+v, want kind %s owned by %s with the run's ID and trigger", job, JobKind, owner)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1", "c2", "c3"}) {
		t.Fatalf("a run for everything removed %v, want c1 c2 c3", got)
	}
}

func TestRunnerRemovesOnlyTickedAndRemovableEachOnce(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	// c1 twice, c3, c5 (one of the two newest: not removable), a ref nobody has, and c2 on the wrong node.
	req := ticked("c1", "c3", "c1", "c5", "nope")
	req = append(req, ItemRef{Type: KindContainer, NodeID: "other", Ref: "c2"}, ItemRef{Type: KindImage, NodeID: "n1-id", Ref: "c2"})
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: req})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.removedIDs(); !slices.Equal(got, []string{"c1", "c3"}) {
		t.Fatalf("removed = %v, want c1 and c3 once each, in the plan's order", got)
	}
	if len(job.Items) != 2 || job.State != jobs.JobDone {
		t.Fatalf("job = %s items %+v, want done with the two removals", job.State, job.Items)
	}
}

func TestRunnerReportsWhatItDidNotRemove(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.removeErr["c1"] = ErrNoLongerStale
	eng.removeErr["c2"] = ErrGone
	eng.removeErr["c3"] = fmt.Errorf("%w: boom", ErrNodeOffline)
	run, locks, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"container/n1-id/c1": {jobs.ItemFailed, codeNoLongerStale},
		"container/n1-id/c2": {jobs.ItemDone, codeGone}, // already gone: a skip, not a removal
		"container/n1-id/c3": {jobs.ItemFailed, codeNodeOffline},
	}
	for _, it := range job.Items {
		if w := want[it.ID]; it.State != w[0] || it.Code != w[1] {
			t.Errorf("item %s = %s/%s, want %s/%s", it.ID, it.State, it.Code, w[0], w[1])
		}
	}
	if len(job.Items) != 3 {
		t.Errorf("items = %+v, want one for each ticked removal", job.Items)
	}
	if _, held := locks.Held("env-1"); held {
		t.Error("the lock is still held after a run that removed nothing")
	}
}

func TestRunnerFreesTheLockWhenItCannotPlan(t *testing.T) {
	t.Parallel()
	run, locks, hub := newRunner(t, newFakeEngine(5))
	run.Policy = func(context.Context, string) (Policy, error) { return Policy{}, errors.New("no policy") }
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.JobFailed || len(job.Items) != 1 || job.Items[0].Code != codePlanFailed {
		t.Fatalf("job = %s items %+v, want failed with the plan step failed", job.State, job.Items)
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("the lock is still held after a run that could not plan")
	}
}

func TestRunnerRecordsTheRunAndItsItems(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c2", "c1")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	// The job reads finished, so the run is finished on record.
	got, items, err := recordOf(run).GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Trigger != TriggerManual || got.Actor != "u1" || got.State != StateCompleted || got.Reason != "" || got.JobID != job.ID ||
		got.StartedAt != t0 || got.FinishedAt != t0 || got.Counts != (Counts{RemovedContainers: 2}) || got.EstBytes != -1 {
		t.Fatalf("run = %+v, want a completed manual run of u1 that removed 2 containers, started and finished at t0, in job %s", got, job.ID)
	}
	if len(items) != 2 || items[0].Seq != 1 || items[0].Ref != "c1" || items[1].Seq != 2 || items[1].Ref != "c2" {
		t.Fatalf("items = %+v, want c1 and c2 in the order they were attempted", items)
	}
	for _, it := range items {
		if it.Outcome != OutcomeRemoved || it.Code != "" || it.Type != KindContainer || it.NodeID != "n1-id" || it.Node != "n1" ||
			it.Reason.Code != ReasonTaskBeyondKeep || it.Reason.Service != "web" {
			t.Errorf("item = %+v, want a clean removal of a container of n1 selected as task_beyond_keep of web", it)
		}
	}
}

// runsNow copies the runs on record at this moment.
func (m *memStore) runsNow() []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Run, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, r)
	}
	return out
}

// JAN-10, empty: a run with nothing to remove is still a run on record.
func TestRunnerAllEmptyPlan(t *testing.T) {
	t.Parallel()
	run, _, hub := newRunner(t, newFakeEngine(0))
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	got, items, err := recordOf(run).GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.Reason != "" || got.Counts != (Counts{}) || got.EstBytes != -1 || got.FinishedAt.IsZero() ||
		got.Trigger != TriggerSchedule || got.Actor != "TierPlan" || len(items) != 0 {
		t.Fatalf("run = %+v items %+v, want a completed scheduled run by TierPlan with zero counts and no items", got, items)
	}
}

// T-11-12: the run is on record, running, when the first removal is asked of the engine.
func TestRunnerRunRowBeforeRemove(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	rs := recordOf(run)
	var (
		mu     sync.Mutex
		before []Run
		asked  int
	)
	eng.onRemove = func(string) {
		mu.Lock()
		defer mu.Unlock()
		if asked == 0 {
			before = rs.runsNow()
		}
		asked++
	}
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c1", "c2")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := hub.Wait(ctx, started.JobID)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked != 2 {
		t.Fatalf("the engine was asked to remove %d containers, want 2", asked)
	}
	if len(before) != 1 {
		t.Fatalf("%d runs on record when the first removal was asked, want 1", len(before))
	}
	r := before[0]
	if r.ID != started.RunID || r.State != StateRunning || r.Trigger != TriggerManual || r.Actor != "u1" || r.EnvID != "env-1" ||
		r.JobID != job.ID || !r.FinishedAt.IsZero() || r.StartedAt != t0 {
		t.Fatalf("run before the first removal = %+v, want it running with its trigger, actor, environment, job and start", r)
	}
}

// A store that cannot record the run means nothing is started, and nothing is removed.
func TestRunnerNotRecordedNotStarted(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, locks, _ := newRunner(t, eng)
	run.Store = failingStore{run.Store}
	_, err := run.Start(testsupport.Context(t), RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: ticked("c1")})
	if !errors.Is(err, errNoStore) {
		t.Fatalf("Start = %v, want the store's error", err)
	}
	if _, held := locks.Held("env-1"); held {
		t.Error("a run that could not be recorded kept the lock")
	}
	if got := eng.removedIDs(); len(got) != 0 {
		t.Errorf("removed = %v, a run that is not on record removed something", got)
	}
}

var errNoStore = errors.New("the database is gone")

// failingStore cannot create a run.
type failingStore struct{ RunStore }

func (failingStore) CreateRun(context.Context, Run) error { return errNoStore }

func TestRunnerPartialWhenItemErrors(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.removeErr["c1"] = errors.New("the engine said no")
	run, _, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c1", "c2")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	got, items, err := recordOf(run).GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePartial || got.Reason != "" || got.Counts != (Counts{RemovedContainers: 1, Failed: 1}) {
		t.Fatalf("run = %+v, want partial with one removed and one failed", got)
	}
	if len(items) != 2 || items[0].Ref != "c1" || items[0].Outcome != OutcomeError || items[0].Code != codeActionFailed ||
		items[1].Ref != "c2" || items[1].Outcome != OutcomeRemoved {
		t.Fatalf("items = %+v, want c1 recorded as an error and c2 as removed", items)
	}
}

// Every way an item can end is recorded, and counted where the record says it is.
func TestRunnerRecordsEachOutcome(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(7) // the two newest are kept, so c1 to c5 are removable
	eng.removeErr["c1"] = ErrNoLongerStale
	eng.removeErr["c2"] = ErrInUse
	eng.removeErr["c3"] = ErrGone
	eng.removeErr["c4"] = fmt.Errorf("%w: down", ErrNodeOffline)
	eng.removeErr["c5"] = &CodedError{Code: CodeAgentPolicyDenied, Err: errors.New("refused")}
	run, _, hub := newRunner(t, eng)
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3", "c4", "c5")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	got, items, err := recordOf(run).GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		outcome Outcome
		code    string
	}{
		{OutcomeSkippedProtected, codeNoLongerStale}, {OutcomeRefusedInUse, codeInUse}, {OutcomeSkippedProtected, codeGone},
		// c4's node did not answer, so nothing is sent for c5, which is on the same node: the engine never hears of it.
		{OutcomeSkippedNodeOffline, codeNodeOffline}, {OutcomeSkippedNodeOffline, codeNodeOffline},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %+v, want %d", items, len(want))
	}
	for i, w := range want {
		if items[i].Outcome != w.outcome || items[i].Code != w.code {
			t.Errorf("item %d (%s) = %s/%q, want %s/%q", i, items[i].Ref, items[i].Outcome, items[i].Code, w.outcome, w.code)
		}
	}
	// The one already gone is not a removal of this run's, and a node that did not answer makes the run partial, not failed.
	if got.State != StatePartial || got.Counts != (Counts{Kept: 2, SkippedOffline: 2}) {
		t.Fatalf("run = %+v, want partial with 2 kept and 2 skipped offline", got)
	}
	if calls := eng.removeCalls(); !slices.Equal(calls, []string{"c1", "c2", "c3", "c4"}) {
		t.Fatalf("the engine was asked to remove %v, want c1 to c4 once each and nothing for c5", calls)
	}
}

func TestRunnerItemsBatched(t *testing.T) {
	t.Parallel()
	run, _, hub := newRunner(t, newFakeEngine(122)) // the two newest are kept
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	rs := recordOf(run)
	got, items, err := rs.GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateCompleted || got.Counts.RemovedContainers != 120 || len(items) != 120 {
		t.Fatalf("run = %+v with %d items, want 120 containers removed and recorded", got, len(items))
	}
	for i, it := range items {
		if it.Seq != i+1 {
			t.Fatalf("item %d has seq %d, want %d", i, it.Seq, i+1)
		}
	}
	var sizes []int
	rs.mu.Lock()
	for _, b := range rs.batches {
		sizes = append(sizes, len(b))
	}
	rs.mu.Unlock()
	if !slices.Equal(sizes, []int{50, 50, 20}) {
		t.Fatalf("items were written in batches of %v, want 50, 50 and 20", sizes)
	}
}

// auditLog is an AuditSink that keeps what it was given.
type auditLog struct {
	mu      sync.Mutex
	entries []audit.Entry
}

func (a *auditLog) Record(_ context.Context, e audit.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditLog) rows() []audit.Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.entries)
}

// audited gives the runner an audit log it can be asked about.
func audited(r *Runner) *auditLog {
	a := &auditLog{}
	r.Audit = a
	return a
}

// finished starts a run, waits for its job and answers the run on record and its items.
func finished(t *testing.T, run *Runner, hub *jobs.Hub, req RunRequest) (Run, []RunItem) {
	t.Helper()
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	got, items, err := recordOf(run).GetRun(ctx, req.EnvID, started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return got, items
}

// A run that cannot read the environment at all ends failed with the reason failed, removes nothing
// and has no item, and says so in the audit log (D-16).
func TestRunnerSnapshotFailure(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.snapErr = errors.New("no manager answered")
	run, locks, hub := newRunner(t, eng)
	log := audited(run)
	got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c1")})
	if got.State != StateFailed || got.Reason != ReasonRunFailed || got.Counts != (Counts{}) || got.EstBytes != -1 || got.FinishedAt.IsZero() {
		t.Fatalf("run = %+v, want failed with the reason failed, no counts and an end", got)
	}
	if len(items) != 0 || len(eng.removeCalls()) != 0 {
		t.Fatalf("items = %+v, removals asked = %v, want none of either", items, eng.removeCalls())
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("the lock is still held")
	}
	rows := log.rows()
	if len(rows) != 1 || rows[0].Result != audit.ResultFailure || rows[0].Details["outcome"] != StateFailed || rows[0].Details["reason"] != ReasonRunFailed {
		t.Fatalf("audit = %+v, want one failure row for the failed run", rows)
	}
}

// When no node reported there is nothing to judge: the run is skipped with the reason
// no_node_reporting, which is not a failure (D-16).
func TestRunnerNoNodeReporting(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	eng.snap = Snapshot{Swarm: true, NotReporting: []string{"n1", "n2"}, Unreachable: []Node{{ID: "n1-id", Name: "n1"}, {ID: "n2-id", Name: "n2"}}, ReadAt: t0}
	run, locks, hub := newRunner(t, eng)
	log := audited(run)
	got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	if got.State != StateSkipped || got.Reason != ReasonNoNodeReporting || got.Counts != (Counts{}) || got.FinishedAt.IsZero() {
		t.Fatalf("run = %+v, want skipped with the reason no_node_reporting", got)
	}
	if len(items) != 0 || len(eng.removeCalls()) != 0 {
		t.Fatalf("items = %+v, removals asked = %v, want none of either", items, eng.removeCalls())
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("the lock is still held")
	}
	if rows := log.rows(); len(rows) != 1 || rows[0].Result != audit.ResultSuccess || rows[0].Details["outcome"] != StateSkipped {
		t.Fatalf("audit = %+v, want one success row for the skipped run", rows)
	}
}

// Each finished run leaves exactly one row in the audit log with the keys the audit package pins:
// the user for Clean now, the system for the schedule, the environment as its target, the counts
// and no name of any item (AUTH-08, D-16).
func TestRunnerSummaryAudit(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(7)
	eng.removeErr["c2"] = ErrInUse
	run, _, hub := newRunner(t, eng)
	log := audited(run)

	got, _ := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3")})
	rows := log.rows()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly one for one run", len(rows))
	}
	e := rows[0]
	if e.Action != audit.ActionJanitorCleaned || e.Actor != (audit.Actor{Type: audit.ActorUser, ID: "u1", Name: "Ada"}) ||
		e.TargetType != "environment" || e.TargetID != "env-1" || e.EnvID != "env-1" || e.Result != audit.ResultSuccess {
		t.Fatalf("entry = %+v, want janitor.cleaned by the user, on the environment, a success", e)
	}
	want := map[string]string{
		"run": got.ID, "trigger": TriggerManual, "outcome": StateCompleted, "reason": "", "removed_containers": "2", "removed_images": "0",
		"removed_networks": "0", "kept": "1", "failed": "0", "skipped_offline": "0", "est_bytes": "unknown",
	}
	if len(e.Details) != len(want) {
		t.Errorf("details = %v, want exactly the keys %v", e.Details, want)
	}
	for k, v := range want {
		if e.Details[k] != v {
			t.Errorf("details[%s] = %q, want %q", k, e.Details[k], v)
		}
	}
	for k, v := range e.Details {
		if k != "run" && (v == "c1" || v == "c2" || v == "c3" || strings.Contains(v, "task-c") || strings.Contains(v, "web.")) {
			t.Errorf("details[%s] = %q names an item", k, v)
		}
	}

	// The schedule's own run is the system's, and a run that ends partial is a failure row.
	eng.removeErr["c4"] = errors.New("boom")
	got, _ = finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	rows = log.rows()
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want one more for the second run", len(rows))
	}
	e = rows[1]
	if e.Actor != (audit.Actor{Type: audit.ActorSystem, ID: "system:janitor", Name: "system:janitor"}) || e.Result != audit.ResultFailure ||
		e.Details["outcome"] != StatePartial || e.Details["trigger"] != TriggerSchedule || e.Details["failed"] != "1" || e.Details["run"] != got.ID {
		t.Fatalf("entry = %+v, want the system's janitor.cleaned, a failure, partial with 1 failed", e)
	}
}

// A run that is cancelled while it works still writes its final state and its audit row, and frees the
// lock (Pitfall 7).
func TestRunnerFinalStateOnCancel(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(7)
	run, locks, hub := newRunner(t, eng)
	log := audited(run)
	jobIDs := make(chan string, 1)
	eng.onRemove = func(id string) {
		if id == "c1" {
			_ = hub.Cancel(<-jobIDs)
		}
	}
	ctx := testsupport.Context(t)
	started, err := run.Start(ctx, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3")})
	if err != nil {
		t.Fatal(err)
	}
	jobIDs <- started.JobID
	if _, err := hub.Wait(ctx, started.JobID); err != nil {
		t.Fatal(err)
	}
	got, items, err := recordOf(run).GetRun(ctx, "env-1", started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StatePartial || got.FinishedAt.IsZero() || got.Counts.RemovedContainers != 1 {
		t.Fatalf("run = %+v, want partial, ended, with the one removal that happened", got)
	}
	if len(items) != 1 || items[0].Ref != "c1" || !slices.Equal(eng.removeCalls(), []string{"c1"}) {
		t.Fatalf("items = %+v calls = %v, want only c1 attempted", items, eng.removeCalls())
	}
	if _, held := locks.Held("env-1"); held {
		t.Fatal("the lock is still held after a cancelled run")
	}
	if rows := log.rows(); len(rows) != 1 || rows[0].Details["outcome"] != StatePartial {
		t.Fatalf("audit = %+v, want one row for the cancelled run", rows)
	}
}

// A ticked item the fresh plan does not remove is on record as skipped_protected no_longer_stale and
// nothing is sent for it; one on a node that did not report is skipped_node_offline (D-14, D-08).
func TestRunnerTickedItemsNotOnThePlan(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // c4 and c5 are the two newest
	eng.snap.Unreachable = []Node{{ID: "n2-id", Name: "n2"}}
	eng.snap.NotReporting = []string{"n2"}
	run, _, hub := newRunner(t, eng)
	req := ticked("c1", "c5", "nope")
	req = append(req, ItemRef{Type: KindContainer, NodeID: "n2-id", Ref: "d1"}, ItemRef{Type: KindContainer, NodeID: "n2-id", Ref: "d1"})
	got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: req})
	if !slices.Equal(eng.removeCalls(), []string{"c1"}) {
		t.Fatalf("the engine was asked to remove %v, want c1 only", eng.removeCalls())
	}
	type row struct {
		ref, name, node string
		outcome         Outcome
		code            string
	}
	var have []row
	for _, it := range items {
		have = append(have, row{it.Ref, it.Name, it.Node, it.Outcome, it.Code})
	}
	want := []row{
		{"c5", "web.1.task-c5", "n1", OutcomeSkippedProtected, codeNoLongerStale},
		{"nope", "nope", "n1", OutcomeSkippedProtected, codeNoLongerStale},
		{"d1", "d1", "n2", OutcomeSkippedNodeOffline, codeNodeOffline},
		{"c1", "web.1.task-c1", "n1", OutcomeRemoved, ""},
	}
	if !slices.Equal(have, want) {
		t.Fatalf("items = %+v, want %+v", have, want)
	}
	if got.State != StatePartial || got.Counts != (Counts{RemovedContainers: 1, Kept: 2, SkippedOffline: 1}) {
		t.Fatalf("run = %+v, want partial with 1 removed, 2 kept and 1 skipped offline", got)
	}
}

// The networks stage (plan 11-08, JAN-04, Pitfall 3): after the containers, from a fresh check, and
// each network by the one removal path behind the Networks port.

// stage records the order in which the engine and the networks were asked, across both.
type stage struct {
	mu  sync.Mutex
	log []string
}

func (s *stage) add(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, what)
}

func (s *stage) events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

// fakeNetworks serves one report and records what it is asked.
type fakeNetworks struct {
	st        *stage
	rep       NetworkReport
	repErr    error
	removeErr map[string]error

	mu         sync.Mutex
	reports    int
	removed    []string
	principals []string
}

func (n *fakeNetworks) Report(context.Context, string) (NetworkReport, error) {
	n.st.add("report")
	n.mu.Lock()
	n.reports++
	n.mu.Unlock()
	return n.rep, n.repErr
}

func (n *fakeNetworks) Remove(_ context.Context, _ string, by *rbac.Principal, row NetworkRow) error {
	n.st.add("network:" + row.ID)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.removed = append(n.removed, row.ID)
	n.principals = append(n.principals, string(by.Type)+":"+by.ID)
	return n.removeErr[row.ID]
}

func (n *fakeNetworks) removeCalls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.removed)
}

// withNetworks gives the runner a Networks that serves rows from one node, with a report that is
// complete, and the engine a hook that records its removals in the same log.
func withNetworks(run *Runner, eng *fakeEngine, rows ...NetworkRow) *fakeNetworks {
	st := &stage{}
	eng.onRemove = func(id string) { st.add("container:" + id) }
	n := &fakeNetworks{st: st, rep: NetworkReport{Complete: true, Rows: rows}, removeErr: map[string]error{}}
	run.Networks = n
	return n
}

// oldNetwork is a network of the node n1 that its stack left behind.
func oldNetwork(id string) NetworkRow {
	return NetworkRow{
		ID: id, Name: "net-" + id, NodeID: "n1-id", Node: "n1", Scope: "local", Class: netspace.ClassOrphaned,
		FormerOwner: "shop", FormerOwnerKind: "stack", IdleSince: t0.Add(-48 * time.Hour),
	}
}

func tickedNetworks(ids ...string) []ItemRef {
	var out []ItemRef
	for _, id := range ids {
		out = append(out, ItemRef{Type: KindNetwork, NodeID: "n1-id", Ref: id})
	}
	return out
}

func itemOutcomes(items []RunItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, string(it.Type)+" "+it.Ref+" "+string(it.Outcome)+" "+it.Code)
	}
	return out
}

// A network is judged only once the run's own containers are gone: the report is read after the
// last container removal, and the network is removed after the report.
func TestRunnerNetworksAfterContainers(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // c1..c3 are beyond the two newest
	run, locks, hub := newRunner(t, eng)
	nets := withNetworks(run, eng, oldNetwork("a"))
	by := &rbac.Principal{Type: rbac.PrincipalUser, ID: "u1", Name: "Ada"}
	req := RunRequest{
		EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", ActorID: "u1", Owner: "user:u1", Principal: by,
		Ticked: append(ticked("c1", "c2"), tickedNetworks("a")...),
	}
	got, items := finished(t, run, hub, req)
	if want := []string{"container:c1", "container:c2", "report", "network:a"}; !slices.Equal(nets.st.events(), want) {
		t.Fatalf("events = %v, want %v: the containers, then a fresh report, then the network", nets.st.events(), want)
	}
	if nets.reports != 1 || !slices.Equal(nets.principals, []string{"user:u1"}) {
		t.Errorf("reports = %d principals = %v, want one report and the removal as the user who pressed Clean now", nets.reports, nets.principals)
	}
	if got.State != StateCompleted || got.Counts != (Counts{RemovedContainers: 2, RemovedNetworks: 1}) {
		t.Fatalf("run = %+v, want completed with 2 containers and 1 network removed", got)
	}
	if want := []string{"container c1 removed ", "container c2 removed ", "network a removed "}; !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	job := jobOf(t, hub, "env-1")
	if job.State != jobs.JobDone || len(job.Items) != 3 || job.Items[2].Kind != "network" || job.Items[2].State != jobs.ItemDone {
		t.Fatalf("job = %s items %+v, want a done job whose third item is the done network", job.State, job.Items)
	}
	if _, held := locks.Held("env-1"); held {
		t.Error("the lock is still held once the job reads finished")
	}
}

// The job lists one item for every planned removal as soon as the plan is known, before the first
// removal is sent, so progress reads "Removing {done} of {total}" from the start (D-14, JAN-09). Who
// started the run is in its Meta for the jobs list.
func TestRunnerJobListsEveryRemovalBeforeTheFirst(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // the newest two of the slot are kept, so three are removable
	run, _, hub := newRunner(t, eng)
	var atFirst jobs.Job
	var once sync.Once
	eng.onRemove = func(string) {
		once.Do(func() { atFirst = jobOf(t, hub, "env-1") })
	}
	finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "ada", Owner: "user:u1", Ticked: ticked("c1", "c2", "c3")})
	if len(atFirst.Items) != 3 {
		t.Fatalf("items at the first removal = %+v, want all 3 already listed", atFirst.Items)
	}
	for _, it := range atFirst.Items {
		if it.Kind != string(KindContainer) || it.Label == "" || (it.State != jobs.ItemWaiting && it.State != jobs.ItemRunning) {
			t.Errorf("item at the first removal = %+v, want a container that has not ended", it)
		}
	}
	if atFirst.Meta[MetaBy] != "ada" || atFirst.Meta[MetaTrigger] != TriggerManual || atFirst.Meta[MetaRunID] == "" {
		t.Errorf("meta = %v, want the run, its trigger and who started it", atFirst.Meta)
	}
}

// jobOf is the one job of the environment the hub holds.
func jobOf(t *testing.T, hub *jobs.Hub, envID string) jobs.Job {
	t.Helper()
	found := hub.List(func(j jobs.Job) bool { return j.EnvID == envID })
	if len(found) != 1 {
		t.Fatalf("jobs of %s = %+v, want exactly one", envID, found)
	}
	return found[0]
}

// A run that ticked no network reads none, and one whose snapshot found no node reads none either.
func TestRunnerNetworksReadOnlyWhenWanted(t *testing.T) {
	t.Parallel()
	t.Run("no network ticked", func(t *testing.T) {
		t.Parallel()
		eng := newFakeEngine(5)
		run, _, hub := newRunner(t, eng)
		nets := withNetworks(run, eng, oldNetwork("a"))
		finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: ticked("c1")})
		if nets.reports != 0 || len(nets.removeCalls()) != 0 {
			t.Fatalf("reports = %d removals = %v, want the networks left unread", nets.reports, nets.removeCalls())
		}
	})
	t.Run("no node reported", func(t *testing.T) {
		t.Parallel()
		eng := newFakeEngine(5)
		eng.snap = Snapshot{Swarm: true, NotReporting: []string{"n1"}, Unreachable: []Node{{ID: "n1-id", Name: "n1"}}, ReadAt: t0}
		run, _, hub := newRunner(t, eng)
		nets := withNetworks(run, eng, oldNetwork("a"))
		got, _ := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
		if got.State != StateSkipped || nets.reports != 0 {
			t.Fatalf("run = %+v reports = %d, want a skipped run that judged no network", got, nets.reports)
		}
	})
	t.Run("all, as the system", func(t *testing.T) {
		t.Parallel()
		eng := newFakeEngine(2) // nothing beyond keep-N
		run, _, hub := newRunner(t, eng)
		nets := withNetworks(run, eng, oldNetwork("a"), oldNetwork("b"))
		got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
		if !slices.Equal(nets.removeCalls(), []string{"a", "b"}) || !slices.Equal(nets.principals, []string{"system:system:janitor", "system:system:janitor"}) {
			t.Fatalf("removed = %v as %v, want both networks as system:janitor", nets.removeCalls(), nets.principals)
		}
		if got.State != StateCompleted || got.Counts.RemovedNetworks != 2 || len(items) != 2 {
			t.Fatalf("run = %+v items = %v, want 2 networks removed", got, itemOutcomes(items))
		}
	})
}

// A ticked network the fresh check no longer finds orphaned or unused is left, with nothing sent.
func TestRunnerNetworkNoLongerOrphaned(t *testing.T) {
	t.Parallel()
	inUse := oldNetwork("a")
	inUse.Class = netspace.ClassInUse
	owned := oldNetwork("b")
	owned.Class = netspace.ClassOwned
	kept := oldNetwork("c")
	kept.Class, kept.KeptByRow = netspace.ClassKept, true
	eng := newFakeEngine(2)
	run, _, hub := newRunner(t, eng)
	nets := withNetworks(run, eng, inUse, owned, kept, oldNetwork("d"))
	got, items := finished(t, run, hub, RunRequest{
		EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: tickedNetworks("a", "b", "c", "d", "gone"),
	})
	if !slices.Equal(nets.removeCalls(), []string{"d"}) {
		t.Fatalf("removed = %v, want only d: the others are not orphaned or unused any more", nets.removeCalls())
	}
	want := []string{
		"network a skipped_protected no_longer_stale", "network b skipped_protected no_longer_stale",
		"network c skipped_protected no_longer_stale", "network gone skipped_protected no_longer_stale", "network d removed ",
	}
	if !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	if got.State != StateCompleted || got.Counts != (Counts{RemovedNetworks: 1, Kept: 4}) {
		t.Fatalf("run = %+v, want completed with 1 removed and 4 kept", got)
	}
}

// Each answer of the removal path is a recorded outcome, and none is tried again.
func TestRunnerNetworkOutcomes(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err     error
		want    string
		counts  Counts
		partial bool
	}{
		"in use again":      {ErrInUse, "network a refused_in_use in_use", Counts{Kept: 1}, false},
		"not orphaned":      {ErrNoLongerStale, "network a skipped_protected no_longer_stale", Counts{Kept: 1}, false},
		"gone":              {ErrGone, "network a skipped_protected gone", Counts{}, false},
		"node offline":      {ErrNodeOffline, "network a skipped_node_offline node_offline", Counts{SkippedOffline: 1}, true},
		"inspect too large": {&CodedError{Code: CodeInspectTooLarge, Err: errors.New("over 64 KB")}, "network a error inspect_too_large", Counts{Failed: 1}, true},
		"other failure":     {errors.New("boom"), "network a error action_failed", Counts{Failed: 1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eng := newFakeEngine(2)
			run, _, hub := newRunner(t, eng)
			nets := withNetworks(run, eng, oldNetwork("a"))
			nets.removeErr["a"] = tc.err
			got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: tickedNetworks("a")})
			if !slices.Equal(itemOutcomes(items), []string{tc.want}) || got.Counts != tc.counts {
				t.Fatalf("items = %v counts = %+v, want %q and %+v", itemOutcomes(items), got.Counts, tc.want, tc.counts)
			}
			if !slices.Equal(nets.removeCalls(), []string{"a"}) {
				t.Errorf("removals = %v, want one and only one", nets.removeCalls())
			}
			if wantState := map[bool]string{true: StatePartial, false: StateCompleted}[tc.partial]; got.State != wantState {
				t.Errorf("state = %s, want %s", got.State, wantState)
			}
		})
	}
}

// A node that stops answering after the run began: no network is removable, and a ticked network of
// that node, like a swarm-wide one, is skipped with nothing sent.
func TestRunnerNetworksWhenANodeIsMissing(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(2)
	eng.snap.Nodes = append(eng.snap.Nodes, Node{ID: "n2-id", Name: "n2"})
	run, _, hub := newRunner(t, eng)
	swarmWide := NetworkRow{ID: "s", Name: "shared", Node: "Cluster", Scope: "swarm", Class: netspace.ClassCantTell}
	nets := withNetworks(run, eng, swarmWide, oldNetwork("a"))
	nets.rep.Complete, nets.rep.WithheldNodes = false, []string{"n2"}
	// The network of n2 is not in the report at all: n2 did not answer.
	refs := append(tickedNetworks("a"), ItemRef{Type: KindNetwork, NodeID: "n2-id", Ref: "b"}, ItemRef{Type: KindNetwork, Ref: "s"})
	got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: refs})
	if len(nets.removeCalls()) != 0 {
		t.Fatalf("removed = %v, want nothing: a node is not reporting", nets.removeCalls())
	}
	want := []string{
		"network a skipped_node_offline node_offline", "network b skipped_node_offline node_offline", "network s skipped_node_offline node_offline",
	}
	if !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	if items[1].Node != "n2" || items[2].Name != "shared" {
		t.Errorf("items = %+v, want the missing node and the swarm network named", items)
	}
	if got.State != StatePartial || got.Counts.SkippedOffline != 3 {
		t.Fatalf("run = %+v, want partial with 3 skipped offline", got)
	}
}

// A check that fails leaves the ticked networks unremoved and the run partial, but the containers
// it already removed stay removed.
func TestRunnerNetworkReportFails(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5)
	run, _, hub := newRunner(t, eng)
	nets := withNetworks(run, eng, oldNetwork("a"))
	nets.repErr = errors.New("the orphan check failed")
	got, items := finished(t, run, hub, RunRequest{
		EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1", Ticked: append(ticked("c1"), tickedNetworks("a")...),
	})
	if len(nets.removeCalls()) != 0 || !slices.Equal(eng.removedIDs(), []string{"c1"}) {
		t.Fatalf("network removals = %v container removals = %v, want only c1", nets.removeCalls(), eng.removedIDs())
	}
	if want := []string{"container c1 removed ", "network a error plan_failed"}; !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	if got.State != StatePartial || got.Counts != (Counts{RemovedContainers: 1, Failed: 1}) {
		t.Fatalf("run = %+v, want partial with 1 removed and 1 failed", got)
	}
	t.Run("a run for everything says it was cut short", func(t *testing.T) {
		t.Parallel()
		eng := newFakeEngine(2)
		run, _, hub := newRunner(t, eng)
		nets := withNetworks(run, eng)
		nets.repErr = errors.New("the orphan check failed")
		got, _ := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
		if got.State != StatePartial {
			t.Fatalf("run = %+v, want partial: the networks were never judged", got)
		}
	})
}

// A run removes its containers first, then its images, then its networks (D-07). The images were
// judged with the run's first read, so one the run's own container removals free is judged by the
// next run, never by this run's stale answer.
func TestRunnerOrderContainersImagesNetworks(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(5) // c1..c3 are beyond the two newest
	eng.snap.ServicesRead = true
	eng.snap.Images = []Image{
		{ID: "sha256:aa", NodeID: "n1-id", Node: "n1", Size: 10, Created: t0.Add(-72 * time.Hour)},
		{ID: "sha256:bb", NodeID: "n1-id", Node: "n1", Size: 20, Created: t0.Add(-96 * time.Hour)},
	}
	run, _, hub := newRunner(t, eng)
	nets := withNetworks(run, eng, oldNetwork("a"))
	eng.onRemoveImage = func(id string) { nets.st.add("image:" + id) }
	req := RunRequest{
		EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true,
	}
	got, items := finished(t, run, hub, req)
	want := []string{"container:c1", "container:c2", "container:c3", "image:sha256:bb", "image:sha256:aa", "report", "network:a"}
	if !slices.Equal(nets.st.events(), want) {
		t.Fatalf("events = %v, want %v: containers, then images (the older first), then a fresh report and the network", nets.st.events(), want)
	}
	if got.State != StateCompleted || got.Counts != (Counts{RemovedContainers: 3, RemovedImages: 2, RemovedNetworks: 1}) {
		t.Fatalf("run = %+v, want completed with 3 containers, 2 images and 1 network removed", got)
	}
	if got.EstBytes != 30 {
		t.Errorf("estimate = %d, want the 30 bytes of the two images", got.EstBytes)
	}
	if len(items) != 6 || items[3].Type != KindImage || items[4].Type != KindImage || items[5].Type != KindNetwork {
		t.Fatalf("items = %v, want three containers, two images, then the network", itemOutcomes(items))
	}
}

// A ticked image the fresh plan no longer removes is left with nothing sent, and says so in words
// about an image.
func TestRunnerTickedImageNoLongerDangling(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(2)
	eng.snap.ServicesRead = true
	eng.snap.Images = []Image{{ID: "sha256:aa", NodeID: "n1-id", Node: "n1", RepoTags: []string{"app:1"}, Created: t0.Add(-72 * time.Hour)}}
	run, _, hub := newRunner(t, eng)
	got, items := finished(t, run, hub, RunRequest{
		EnvID: "env-1", Trigger: TriggerManual, Actor: "Ada", Owner: "user:u1",
		Ticked: []ItemRef{{Type: KindImage, NodeID: "n1-id", Ref: "sha256:aa"}},
	})
	if len(eng.removeCalls()) != 0 {
		t.Fatalf("calls = %v, want nothing sent for an image that has a tag", eng.removeCalls())
	}
	if want := []string{"image sha256:aa skipped_protected no_longer_stale"}; !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	if items[0].Node != "n1" || items[0].Name != "aa" || got.Counts.Kept != 1 {
		t.Errorf("item = %+v counts = %+v, want the image named on its node and counted as kept", items[0], got.Counts)
	}
}

// The words of an image the engine refused: in use is kept, and a failure is an error with a code.
func TestRunnerImageRefusals(t *testing.T) {
	t.Parallel()
	eng := newFakeEngine(2)
	eng.snap.ServicesRead = true
	eng.snap.Images = []Image{
		{ID: "sha256:aa", NodeID: "n1-id", Node: "n1", Created: t0.Add(-72 * time.Hour)},
		{ID: "sha256:bb", NodeID: "n1-id", Node: "n1", Created: t0.Add(-71 * time.Hour)},
		{ID: "sha256:cc", NodeID: "n1-id", Node: "n1", Created: t0.Add(-70 * time.Hour)},
	}
	eng.removeErr["image:sha256:aa"] = ErrInUse
	eng.removeErr["image:sha256:bb"] = ErrGone
	eng.removeErr["image:sha256:cc"] = &CodedError{Code: "action_failed", Err: errors.New("boom")}
	run, _, hub := newRunner(t, eng)
	got, items := finished(t, run, hub, RunRequest{EnvID: "env-1", Trigger: TriggerSchedule, Actor: "TierPlan", Owner: "system:janitor:env-1", All: true})
	want := []string{
		"image sha256:aa refused_in_use in_use", "image sha256:bb skipped_protected gone", "image sha256:cc error action_failed",
	}
	if !slices.Equal(itemOutcomes(items), want) {
		t.Fatalf("items = %v, want %v", itemOutcomes(items), want)
	}
	if got.State != StatePartial {
		t.Errorf("state = %s, want partial: one image failed", got.State)
	}
}
