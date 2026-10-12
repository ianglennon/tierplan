// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/jobs"
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

func drain(t *testing.T, ch <-chan jobs.Event) []jobs.Event {
	t.Helper()
	var out []jobs.Event
	timeout := time.After(5 * time.Second)
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
		<-release
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
	h := jobs.New(jobs.Options{Coalesce: 60 * time.Millisecond})
	t.Cleanup(h.Close)
	started := make(chan struct{})
	release := make(chan struct{})
	job, err := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		close(started)
		for i := int64(1); i <= 200; i++ {
			r.Update("n1", func(it *jobs.Item) { it.State, it.Current, it.Total = jobs.ItemRunning, i, 200 })
		}
		<-release
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
	time.Sleep(150 * time.Millisecond) // the flush of the folded updates
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
	job, _ := h.Start(twoItems("u"), func(ctx context.Context, r *jobs.Reporter) {
		<-step
		r.Finish("n1", jobs.ItemDone, "", "", nil)
		<-step
		r.Finish("n2", jobs.ItemDone, "", "", nil)
	})
	step <- struct{}{}
	// n1 is finished: the snapshot shows it, and after 0 only later events follow.
	var snap jobs.Job
	var ch <-chan jobs.Event
	var cancel func()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		snap, ch, cancel, err = h.Subscribe(context.Background(), job.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Items[0].State == jobs.ItemDone {
			break
		}
		cancel()
		if time.Now().After(deadline) {
			t.Fatal("n1 never finished")
		}
		time.Sleep(time.Millisecond)
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

func waitFinished(t *testing.T, h *jobs.Hub, id string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := h.Get(id); ok && j.Finished() {
			return j
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the job never finished")
	return jobs.Job{}
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
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	got, err := h.Wait(ctx, job.ID)
	if err != nil || !got.Finished() {
		t.Fatalf("Wait = %+v, %v, want the finished job", got, err)
	}
	if _, err := h.Wait(ctx, "nope"); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("Wait of an unknown job = %v", err)
	}
	// A job that does not end: Wait gives up with the context and still returns the job.
	hold2, _ := h.Start(twoItems("v"), hold(make(chan struct{})))
	short, stop2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop2()
	got, err = h.Wait(short, hold2.ID)
	if !errors.Is(err, context.DeadlineExceeded) || got.ID != hold2.ID || got.Finished() {
		t.Fatalf("Wait of a running job = %+v, %v", got, err)
	}
}
