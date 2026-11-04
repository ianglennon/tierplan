// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package jobs_test

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

func twoItems(owner string) jobs.Spec {
	return jobs.Spec{
		Kind: "pull", EnvID: "env-1", Owner: owner, Meta: map[string]string{"ref": "x"},
		Items: []jobs.ItemSpec{{ID: "n1", Label: "node one"}, {ID: "n2", Label: "node two"}},
	}
}

// hold is a run that waits to be released, so a test controls when a job ends.
func hold(release <-chan struct{}) func(context.Context, *jobs.Reporter) {
	return func(ctx context.Context, r *jobs.Reporter) {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
}

// gate waits for a channel the test closes, or for the end of the job's context; it is false when the
// job was cancelled first, so a run never outlives a test that failed.
func gate(ctx context.Context, ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func drain(t *testing.T, ch <-chan jobs.Event) []jobs.Event {
	t.Helper()
	var out []jobs.Event
	timeout := testsupport.Context(t).Done()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("the event channel never closed")
		}
	}
}

func TestStartReturnsARunningJobAndFinishEndsItsItems(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	release := make(chan struct{})
	job, err := h.Start(twoItems("user:a"), func(ctx context.Context, r *jobs.Reporter) {
		if !gate(ctx, release) {
			return
		}
		r.Finish("n1", jobs.ItemDone, "", "", map[string]string{"digest": "sha256:1"})
		r.Finish("n2", jobs.ItemFailed, "pull_denied", "Denied.", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobs.JobRunning || len(job.Items) != 2 || job.Items[0].State != jobs.ItemWaiting || job.ID == "" {
		t.Fatalf("started job = %+v", job)
	}
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	close(release)
	evs := drain(t, ch)
	if len(evs) != 3 || evs[2].Type != jobs.EventDone || evs[2].Job.State != jobs.JobPartial {
		t.Fatalf("events = %+v", evs)
	}
	got, ok := h.Get(job.ID)
	if !ok || got.State != jobs.JobPartial || got.FinishedAt.IsZero() || got.Items[0].Result["digest"] != "sha256:1" || got.Items[1].Code != "pull_denied" {
		t.Fatalf("job = %+v", got)
	}
}

func TestEventIDsStrictlyIncreaseAndUpdatesAreCoalesced(t *testing.T) {
	// The coalescer is a timer (time.AfterFunc), so the test runs in a synctest bubble: the
	// real 60 ms window passes on the bubble's fake clock, in no wall-clock time.
	synctest.Test(t, func(t *testing.T) {
		coalescesUpdates(t)
	})
}

func coalescesUpdates(t *testing.T) {
	h := jobs.New(jobs.Options{Coalesce: 60 * time.Millisecond})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	release := make(chan struct{})
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		close(started)
		for i := int64(1); i <= 200; i++ {
			r.Update("n1", func(it *jobs.Item) { it.State, it.Current, it.Total = jobs.ItemRunning, i, 200 })
		}
		if !gate(ctx, release) {
			return
		}
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	<-started
	synctest.Sleep(150 * time.Millisecond) // the flush of the folded updates, on the fake clock
	close(release)
	evs := drain(t, ch)
	var last uint64
	var updates int
	var final int64
	for _, ev := range evs {
		if ev.ID <= last {
			t.Errorf("event id %d after %d", ev.ID, last)
		}
		last = ev.ID
		if ev.Type == jobs.EventItem && ev.Item.ID == "n1" && ev.Item.State == jobs.ItemRunning {
			updates++
			final = ev.Item.Current
		}
	}
	// 200 updates in a few microseconds fold into the first and the flush after the gap.
	if updates < 1 || updates > 4 {
		t.Errorf("%d running events of n1, want the 200 updates folded to a few", updates)
	}
	if final != 200 {
		t.Errorf("the last published running state has Current %d, want the latest, 200", final)
	}
}

func TestSubscribeSnapshotAndResume(t *testing.T) {
	h := jobs.New(jobs.Options{Coalesce: time.Millisecond})
	t.Cleanup(h.Close)
	step := make(chan struct{})
	n1Finished := make(chan struct{})
	job, _ := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		<-step
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		close(n1Finished)
		<-step
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	step <- struct{}{}
	// n1 is finished: the snapshot shows it, and after 0 only later events follow.
	testsupport.Await(t, "the run to finish n1", n1Finished)
	snap, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Items[0].State != jobs.ItemDone {
		t.Fatalf("snapshot after n1 finished = %+v", snap.Items)
	}
	step <- struct{}{}
	evs := drain(t, ch)
	cancel()
	if len(evs) != 2 || evs[0].Item.ID != "n2" || evs[1].Type != jobs.EventDone {
		t.Fatalf("events after the snapshot = %+v", evs)
	}
	// A reader that last saw event 1 is replayed 2 and 3 only, even on a finished job.
	_, ch2, cancel2, err := h.Subscribe(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()
	replay := drain(t, ch2)
	if len(replay) != 2 || replay[0].ID != 2 || replay[1].ID != 3 {
		t.Fatalf("replay after 1 = %+v", replay)
	}
}

func TestFinishedJobIsKeptForTheRetentionThenForgotten(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := jobs.New(jobs.Options{Now: clock})
	t.Cleanup(h.Close)
	job, _ := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	waitFinished(t, h, job.ID)
	mu.Lock()
	now = now.Add(9 * time.Minute)
	mu.Unlock()
	if _, ok := h.Get(job.ID); !ok {
		t.Fatal("the job was forgotten after 9 minutes")
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if _, ok := h.Get(job.ID); ok {
		t.Fatal("the job is still there 11 minutes after it finished")
	}
	if _, _, _, err := h.Subscribe(context.Background(), job.ID, 0); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Subscribe of a forgotten job = %v, want ErrNotFound", err)
	}
}

// waitFinished waits for the job's own finish (Hub.Wait returns once it has ended).
func waitFinished(t *testing.T, h *jobs.Hub, id string) jobs.Job {
	t.Helper()
	got, err := h.Wait(testsupport.Context(t), id)
	if err != nil || !got.Finished() {
		t.Fatalf("the job never finished: %+v, %v", got, err)
	}
	return got
}

func TestTheOldestFinishedJobIsEvictedAtTheCap(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); now = now.Add(time.Second); return now }
	h := jobs.New(jobs.Options{Now: clock, MaxRetained: 200, MaxRunningPerUser: 1000, Retention: time.Hour})
	t.Cleanup(h.Close)
	var ids []string
	for range 200 {
		job, err := h.Start(jobs.Spec{Kind: "pull", Owner: "u", Items: []jobs.ItemSpec{{ID: "n", Label: "n"}}}, func(ctx context.Context, r *jobs.Reporter) {
			r.Finish("n", jobs.ItemDone, "", "", nil)
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
		waitFinished(t, h, job.ID)
	}
	if n := len(h.List(nil)); n != 200 {
		t.Fatalf("%d jobs retained, want 200", n)
	}
	extra, err := h.Start(jobs.Spec{Kind: "pull", Owner: "u"}, func(context.Context, *jobs.Reporter) {})
	if err != nil {
		t.Fatal(err)
	}
	waitFinished(t, h, extra.ID)
	if _, ok := h.Get(ids[0]); ok {
		t.Error("the oldest finished job survived the 201st")
	}
	if _, ok := h.Get(ids[1]); !ok {
		t.Error("the second oldest was evicted too")
	}
	list := h.List(nil)
	if len(list) != 200 || list[0].ID != extra.ID {
		t.Fatalf("List: %d jobs, newest %q, want 200 and %q first", len(list), list[0].ID, extra.ID)
	}
}

func TestASixthRunningJobOfOneOwnerIsRefused(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	release := make(chan struct{})
	for i := range 5 {
		if _, err := h.Start(twoItems("user:a"), hold(release)); err != nil {
			t.Fatalf("job %d: %v", i+1, err)
		}
	}
	if _, err := h.Start(twoItems("user:a"), hold(release)); !errors.Is(err, jobs.ErrTooMany) {
		t.Fatalf("sixth job = %v, want ErrTooMany", err)
	}
	if _, err := h.Start(twoItems("user:b"), hold(release)); err != nil {
		t.Fatalf("another owner's job: %v", err)
	}
	close(release)
}

func TestCancelCancelsTheRunContextAndTheItemsEndCancelled(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, _ := h.Start(twoItems("u"), hold(make(chan struct{})))
	if err := h.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	got := waitFinished(t, h, job.ID)
	if got.State != jobs.JobCancelled || got.Items[0].State != jobs.ItemCancelled {
		t.Fatalf("job = %+v", got)
	}
	if err := h.Cancel("nope"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Cancel of an unknown job = %v", err)
	}
}

func TestARunThatReturnsEarlyLeavesNoItemRunning(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, _ := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
	})
	got := waitFinished(t, h, job.ID)
	if got.State != jobs.JobPartial || got.Items[1].State != jobs.ItemFailed || got.Items[1].Code != jobs.CodeInterrupted {
		t.Fatalf("job = %+v", got)
	}
}

// A run that panics ends its job with the unfinished items interrupted and leaves the process,
// and the hub, working (WR-A06).
func TestARunThatPanicsEndsItsJobInterruptedAndTheHubKeepsWorking(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		panic("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	got := waitFinished(t, h, job.ID)
	if got.State != jobs.JobPartial || got.Items[0].State != jobs.ItemDone || got.Items[1].State != jobs.ItemFailed || got.Items[1].Code != jobs.CodeInterrupted {
		t.Fatalf("job = %+v, want the finished item kept and the other interrupted", got)
	}
	again, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := waitFinished(t, h, again.ID); got.State != jobs.JobDone {
		t.Errorf("a job after the panic = %+v, want done", got)
	}
}

func TestCloseCancelsRunningJobsAndClosesSubscriptions(t *testing.T) {
	h := jobs.New(jobs.Options{})
	job, _ := h.Start(twoItems("u"), hold(make(chan struct{})))
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	h.Close()
	drain(t, ch)
	if _, err := h.Start(twoItems("u"), hold(nil)); err == nil {
		t.Error("a closed hub started a job")
	}
}

// Cancel (plan 05-04, D-26): the items that have not finished end cancelled with a code, the
// ones that have keep their result, and cancelling a job that has ended changes nothing.

func threeItems(owner string) jobs.Spec {
	s := twoItems(owner)
	s.Items = append(s.Items, jobs.ItemSpec{ID: "n3", Label: "node three"})
	return s
}

// runOneDone finishes n1 and leaves the others to the context.
func runOneDone(started chan<- struct{}) func(context.Context, *jobs.Reporter) {
	return func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", map[string]string{"digest": "sha256:abc"})
		r.Update("n2", func(it *jobs.Item) { it.State = jobs.ItemRunning })
		r.Update("n3", func(it *jobs.Item) { it.State = jobs.ItemRunning })
		close(started)
		<-ctx.Done()
	}
}

func TestCancelEndsTheUnfinishedItemsAsCancelledWithACodeAndTheJobAsPartial(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	job, err := h.Start(threeItems("u"), runOneDone(started))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := h.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	got := waitFinished(t, h, job.ID)
	if got.State != jobs.JobPartial {
		t.Fatalf("state = %q, want partial", got.State)
	}
	if got.Items[0].State != jobs.ItemDone || got.Items[0].Result["digest"] != "sha256:abc" {
		t.Errorf("the finished item changed: %+v", got.Items[0])
	}
	for _, it := range got.Items[1:] {
		if it.State != jobs.ItemCancelled || it.Code != jobs.CodeCancelled || it.Message == "" {
			t.Errorf("item %s = %+v, want cancelled with code %q", it.ID, it, jobs.CodeCancelled)
		}
	}
}

func TestCancelOfAJobWhereNothingFinishedEndsItCancelledEvenIfOneItemFailed(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	job, _ := h.Start(threeItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemFailed, "node_unreachable", "x", nil)
		close(started)
		<-ctx.Done()
	})
	<-started
	if err := h.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	if got := waitFinished(t, h, job.ID); got.State != jobs.JobCancelled {
		t.Fatalf("state = %q, want cancelled", got.State)
	}
}

func TestCancelOfAFinishedJobIsErrFinishedAndChangesNothing(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, _ := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	before := waitFinished(t, h, job.ID)
	if err := h.Cancel(job.ID); !errors.Is(err, jobs.ErrFinished) {
		t.Fatalf("Cancel of a finished job = %v, want ErrFinished", err)
	}
	if err := h.Cancel(job.ID); !errors.Is(err, jobs.ErrFinished) {
		t.Fatalf("second Cancel = %v, want ErrFinished", err)
	}
	after, _ := h.Get(job.ID)
	if after.State != before.State || !after.FinishedAt.Equal(before.FinishedAt) || after.Items[0].State != jobs.ItemDone {
		t.Fatalf("a cancel changed a finished job: before %+v after %+v", before, after)
	}
}

func TestACancelledJobPublishesDoneOnce(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	job, _ := h.Start(threeItems("u"), runOneDone(started))
	<-started
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	_ = h.Cancel(job.ID)
	_ = h.Cancel(job.ID)
	dones := 0
	for _, ev := range drain(t, ch) {
		if ev.Type == jobs.EventDone {
			dones++
		}
	}
	if dones != 1 {
		t.Fatalf("done events = %d, want 1", dones)
	}
}

func TestWaitReturnsTheJobOnceItHasEnded(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	job, _ := h.Start(threeItems("u"), runOneDone(started))
	<-started
	_ = h.Cancel(job.ID)
	ctx := testsupport.Context(t)
	got, err := h.Wait(ctx, job.ID)
	if err != nil || !got.Finished() {
		t.Fatalf("Wait = %+v, %v, want the finished job", got, err)
	}
	if _, err := h.Wait(ctx, "nope"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Wait of an unknown job = %v", err)
	}
	// A job that does not end: Wait gives up with the context and still returns the job.
	hold2, _ := h.Start(twoItems("v"), hold(make(chan struct{})))
	// wall-clock: the job never ends, so only this context ends Wait; its length cannot change the result
	short, stop2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop2()
	got, err = h.Wait(short, hold2.ID)
	if !errors.Is(err, context.DeadlineExceeded) || got.ID != hold2.ID || got.Finished() {
		t.Fatalf("Wait of a running job = %+v, %v", got, err)
	}
}

func TestAddPutsAnItemInAJobWhileItRunsAndSubscribersSeeIt(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	begin := make(chan struct{})
	release := make(chan struct{})
	job, err := h.Start(jobs.Spec{
		Kind: "stack_deploy", Owner: "u",
		Items: []jobs.ItemSpec{{ID: "services", Label: "Deploy services", Kind: "step"}},
	}, func(ctx context.Context, r *jobs.Reporter) {
		if !gate(ctx, begin) {
			return
		}
		r.Add(jobs.ItemSpec{ID: "service:web", Label: "web", Parent: "services", Kind: "service"})
		// Adding an ID the job has changes nothing, and nothing new is published for it.
		r.Add(jobs.ItemSpec{ID: "service:web", Label: "changed", Parent: "other", Kind: "other"})
		if !gate(ctx, release) {
			return
		}
		r.Finish("service:web", jobs.ItemDone, "", "", nil)
		r.Finish("services", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(snap.Items) != 1 {
		t.Fatalf("snapshot items = %+v, want the one the job started with", snap.Items)
	}
	close(begin)
	close(release)
	evs := drain(t, ch)

	var addedEvents int
	for _, ev := range evs {
		if ev.Type == jobs.EventItem && ev.Item.ID == "service:web" && ev.Item.State == jobs.ItemWaiting {
			addedEvents++
			if ev.Item.Parent != "services" || ev.Item.Kind != "service" || ev.Item.Label != "web" {
				t.Errorf("added item = %+v, want its parent, kind and label as given", ev.Item)
			}
		}
	}
	if addedEvents != 1 {
		t.Errorf("the added item was published %d times, want once", addedEvents)
	}
	got, ok := h.Get(job.ID)
	if !ok || got.State != jobs.JobDone || len(got.Items) != 2 || got.Items[1].ID != "service:web" || got.Items[1].Parent != "services" || got.Items[1].Kind != "service" {
		t.Fatalf("job = %+v, want done with the added item after the first", got)
	}
	if got.Items[0].Kind != "step" || got.Items[0].Parent != "" {
		t.Errorf("first item = %+v, want the kind it started with", got.Items[0])
	}
}

func TestAddToAFinishedJobChangesNothing(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	var rep *jobs.Reporter
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		rep = r
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}
	rep.Add(jobs.ItemSpec{ID: "late", Label: "late"})
	got, _ := h.Get(job.ID)
	if len(got.Items) != 2 || got.State != jobs.JobDone {
		t.Errorf("job = %+v, want the two items it ended with", got)
	}
}

func TestAnAddedItemIsInTheReplayAfterAnEventAndNothingIsRepeated(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	step := make(chan struct{})
	release := make(chan struct{})
	job, err := h.Start(jobs.Spec{Kind: "k", Owner: "u", Items: []jobs.ItemSpec{{ID: "a", Label: "a"}}}, func(ctx context.Context, r *jobs.Reporter) {
		r.Add(jobs.ItemSpec{ID: "b", Label: "b", Parent: "a"})
		close(step)
		if !gate(ctx, release) {
			return
		}
		r.Finish("b", jobs.ItemDone, "", "", nil)
		r.Finish("a", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	<-step
	// A first reader saw the add (event 1); a reader that resumes after 1 sees 2 onwards, and the
	// snapshot it is sent holds the added item.
	snap, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if len(snap.Items) != 2 {
		t.Fatalf("snapshot = %+v, want both items", snap.Items)
	}
	close(release)
	evs := drain(t, ch)
	var last uint64 = 1
	for _, ev := range evs {
		if ev.ID <= last {
			t.Fatalf("event %d after %d: a resumed reader must see only newer events, once each (%+v)", ev.ID, last, evs)
		}
		last = ev.ID
	}
	if evs[0].Item == nil || evs[0].Item.ID != "b" || evs[0].Item.State != jobs.ItemDone {
		t.Errorf("first replayed event = %+v, want the finish of the added item, the first event after 1", evs[0])
	}
}

func TestOutputLinesAreEventsAndAreCleaned(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	begin, release := make(chan struct{}), make(chan struct{})
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		if !gate(ctx, begin) {
			return
		}
		r.Output("Created network web_default")
		r.Output("engine said \x1b[31mno\x1b[0m\nsecond line")
		if !gate(ctx, release) {
			return
		}
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	close(begin)
	close(release)
	evs := drain(t, ch)

	var lines []jobs.Output
	var last uint64
	for _, ev := range evs {
		if ev.ID <= last {
			t.Fatalf("event %d after %d: ids must strictly increase", ev.ID, last)
		}
		last = ev.ID
		if ev.Type == jobs.EventOutput {
			if ev.Output == nil || ev.Output.Seq != ev.ID {
				t.Fatalf("output event %+v, want an Output whose Seq is the event's ID", ev)
			}
			lines = append(lines, *ev.Output)
		}
	}
	if len(lines) != 2 || lines[0].Text != "Created network web_default" || lines[1].Text != "engine said [31mno[0msecond line" {
		t.Fatalf("output events = %+v, want the two lines, cleaned", lines)
	}
	if lines[0].At.IsZero() {
		t.Error("an output line has no time")
	}
	got, ok := h.Output(job.ID)
	if !ok || len(got) != 2 || got[0] != lines[0] || got[1] != lines[1] {
		t.Errorf("Output = %+v (%v), want the two lines as the events carried them", got, ok)
	}
}

func TestOutputKeepsTheNewestLinesInItsOwnRingAndNeverEvictsItemEvents(t *testing.T) {
	h := jobs.New(jobs.Options{Ring: 8, OutputRing: 50})
	t.Cleanup(h.Close)
	job, err := h.Start(jobs.Spec{Kind: "k", Owner: "u", Items: []jobs.ItemSpec{{ID: "a", Label: "a"}}}, func(ctx context.Context, r *jobs.Reporter) {
		r.Add(jobs.ItemSpec{ID: "b", Label: "b"}) // event 1
		for i := range 500 {
			r.Output("line " + strconv.Itoa(i))
		}
		r.Finish("b", jobs.ItemDone, "", "", nil)
		r.Finish("a", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}
	out, _ := h.Output(job.ID)
	if len(out) != 50 || out[0].Text != "line 450" || out[49].Text != "line 499" {
		t.Fatalf("output = %d lines from %q to %q, want the newest 50", len(out), head(out[0].Text, 20), head(out[len(out)-1].Text, 20))
	}
	if !slices.IsSortedFunc(out, func(a, b jobs.Output) int { return cmp.Compare(a.Seq, b.Seq) }) {
		t.Error("output is not in order")
	}
	// A reader that last saw event 1 is replayed the item events it missed, however many lines the
	// job wrote, and the retained lines, in order and once each.
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	evs := drain(t, ch)
	var ids []uint64
	var items, lines int
	for _, ev := range evs {
		ids = append(ids, ev.ID)
		switch ev.Type {
		case jobs.EventItem:
			items++
		case jobs.EventOutput:
			lines++
		}
	}
	if items != 2 || lines != 50 {
		t.Errorf("replay holds %d item events and %d output events, want both item finishes and the retained 50 lines", items, lines)
	}
	if !slices.IsSorted(ids) || len(slices.Compact(slices.Clone(ids))) != len(ids) || ids[0] <= 1 {
		t.Errorf("replay ids = %v, want only events after 1, in order, once each", ids)
	}
}

func TestOutputOfAFinishedJobIsKeptAndALateLineIsDropped(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	var rep *jobs.Reporter
	job, err := h.Start(jobs.Spec{Kind: "k", Owner: "u", Items: []jobs.ItemSpec{{ID: "a", Label: "a"}}}, func(ctx context.Context, r *jobs.Reporter) {
		rep = r
		r.Output("before")
		r.Finish("a", jobs.ItemDone, "", "", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Wait(testsupport.Context(t), job.ID); err != nil {
		t.Fatal(err)
	}
	rep.Output("after")
	out, ok := h.Output(job.ID)
	if !ok || len(out) != 1 || out[0].Text != "before" {
		t.Errorf("Output = %+v (%v), want only the line written while the job ran", out, ok)
	}
	if _, ok := h.Output("no-such-job"); ok {
		t.Error("Output of a job the hub does not have reported one")
	}
}

func TestDetailIsTheRemoteWordsCleanedAndPublishedWithTheItem(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	// The body waits for the subscriber: a job that ends first would hand it the end and no item events.
	subscribed := make(chan struct{})
	job, err := h.Start(jobs.Spec{Kind: "k", Owner: "u", Items: []jobs.ItemSpec{{ID: "a", Label: "a"}}}, func(ctx context.Context, r *jobs.Reporter) {
		<-subscribed
		r.Detail("a", "rpc error:\x1b[1m code = InvalidArgument\n"+strings.Repeat("z", 3000))
		r.Finish("a", jobs.ItemFailed, "service_rejected", "The swarm rejected the service.", nil)
		r.Detail("a", "too late") // an item that has ended is not changed
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ch, cancel, err := h.Subscribe(context.Background(), job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	close(subscribed)
	evs := drain(t, ch)
	got, _ := h.Get(job.ID)
	detail := got.Items[0].Detail
	if !strings.HasPrefix(detail, "rpc error:[1m code = InvalidArgumentzzz") || utf8.RuneCountInString(detail) != jobs.MaxDetailRunes || !strings.HasSuffix(detail, "…") {
		t.Errorf("detail = %q (%d characters), want it cleaned and clipped", head(detail, 50), utf8.RuneCountInString(detail))
	}
	var published string
	for _, ev := range evs {
		if ev.Type == jobs.EventItem {
			published = ev.Item.Detail
		}
	}
	if published != detail {
		t.Error("the finish event did not carry the detail")
	}
}

func TestASkippedItemCountsAsNeitherDoneNorFailed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ends  map[string]string
		state string
	}{
		{"a failure and the item it skipped", map[string]string{"n1": jobs.ItemFailed, "n2": jobs.ItemSkipped}, jobs.JobFailed},
		{"a finished item and the item that was skipped", map[string]string{"n1": jobs.ItemDone, "n2": jobs.ItemSkipped}, jobs.JobPartial},
		{"only skipped items", map[string]string{"n1": jobs.ItemSkipped, "n2": jobs.ItemSkipped}, jobs.JobCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := jobs.New(jobs.Options{})
			t.Cleanup(h.Close)
			job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
				for _, id := range []string{"n1", "n2"} {
					r.Finish(id, tc.ends[id], "not_run", "", nil)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.Wait(testsupport.Context(t), job.ID)
			if err != nil || got.State != tc.state {
				t.Errorf("job = %s (%v), want %s", got.State, err, tc.state)
			}
		})
	}
}

func TestASkippedItemIsFinalAndTheRunsEndLeavesItAlone(t *testing.T) {
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		r.Finish("n1", jobs.ItemSkipped, "not_run", "Not run.", nil)
		r.Finish("n1", jobs.ItemDone, "", "", nil) // already ended
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].State != jobs.ItemSkipped || got.Items[0].Code != "not_run" {
		t.Errorf("first item = %+v, want it skipped for good", got.Items[0])
	}
	// The item the run never touched is ended by the hub, as before.
	if got.Items[1].State != jobs.ItemFailed || got.Items[1].Code != jobs.CodeInterrupted {
		t.Errorf("second item = %+v, want interrupted", got.Items[1])
	}
}

// A scoped reporter lets code written for a job of one operation run as one of several in a bigger job (plan 12-30): its item IDs
// and the parents it names are prefixed, its added items belong to the item the scope is for, and the job's items stay apart.
func TestScopedReporterKeepsTheItemsOfEachScopeApart(t *testing.T) {
	t.Parallel()
	h := jobs.New(jobs.Options{})
	t.Cleanup(h.Close)
	job, err := h.Start(jobs.Spec{
		Kind: "image_update", Owner: "u",
		Items: []jobs.ItemSpec{{ID: "i0", Label: "first"}, {ID: "i1", Label: "second"}},
	}, func(ctx context.Context, r *jobs.Reporter) {
		for i, id := range []string{"i0", "i1"} {
			s := r.Scoped(id+"/", id)
			s.Add(jobs.ItemSpec{ID: "pull", Label: "Pull"})
			s.Add(jobs.ItemSpec{ID: "layer", Label: "Layer", Parent: "pull"})
			s.Update("pull", func(it *jobs.Item) { it.State = jobs.ItemRunning })
			s.Detail("pull", "detail "+id)
			s.Finish("layer", jobs.ItemDone, "", "", nil)
			state := jobs.ItemDone
			if i == 1 {
				state = jobs.ItemFailed
			}
			s.Finish("pull", state, "", "", nil)
			r.Finish(id, state, "", "", nil)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ id, parent, state, detail string }
	var rows []row
	for _, it := range got.Items {
		rows = append(rows, row{it.ID, it.Parent, it.State, it.Detail})
	}
	want := []row{
		{"i0", "", jobs.ItemDone, ""}, {"i1", "", jobs.ItemFailed, ""},
		{"i0/pull", "i0", jobs.ItemDone, "detail i0"}, {"i0/layer", "i0/pull", jobs.ItemDone, ""},
		{"i1/pull", "i1", jobs.ItemFailed, "detail i1"}, {"i1/layer", "i1/pull", jobs.ItemDone, ""},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("items = %+v\nwant %+v", rows, want)
	}
	if got.State != jobs.JobPartial {
		t.Errorf("job state = %q, want partial", got.State)
	}
}
