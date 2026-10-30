// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/stackops"
)

// The operation lock (plan 07-18, STK-09 concurrency): one stack operation at a time on an
// environment, with the holder named for the one who is refused.

var started = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func holder(stack string) stackops.Holder {
	return stackops.Holder{StackID: "id-" + stack, StackName: stack, Kind: "deploy", By: "ada", Started: started}
}

func TestTryAcquireOnAFreeEnvironmentGivesARelease(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	release, _, ok := l.TryAcquire("env-1", holder("web"))
	if !ok || release == nil {
		t.Fatalf("TryAcquire = %v, want the lock on a free environment", ok)
	}
	got, held := l.Held("env-1")
	if !held || got.StackName != "web" || got.By != "ada" || !got.Started.Equal(started) || got.Kind != "deploy" {
		t.Errorf("Held = %+v, %v, want the holder as it was given", got, held)
	}
}

func TestASecondTryAcquireGetsTheFirstHolderUntilItIsReleased(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	release, _, _ := l.TryAcquire("env-1", holder("web"))
	l.Attach("env-1", "job-1")

	r2, held, ok := l.TryAcquire("env-1", holder("api"))
	if ok || r2 != nil {
		t.Fatal("a second operation took the lock while one runs")
	}
	if held.StackName != "web" || held.JobID != "job-1" {
		t.Errorf("held = %+v, want web's holder with the job attached", held)
	}

	release()
	if _, still := l.Held("env-1"); still {
		t.Error("the lock is still held after its release")
	}
	if _, _, ok := l.TryAcquire("env-1", holder("api")); !ok {
		t.Error("the lock was not free after its release")
	}
}

func TestTwoEnvironmentsNeverBlockEachOther(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	if _, _, ok := l.TryAcquire("env-1", holder("web")); !ok {
		t.Fatal("env-1 refused")
	}
	if _, _, ok := l.TryAcquire("env-2", holder("web")); !ok {
		t.Error("env-2 was blocked by a lock on env-1")
	}
}

func TestReleaseTwiceDoesNotFreeANewerHoldersLock(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	first, _, _ := l.TryAcquire("env-1", holder("web"))
	first()
	_, _, ok := l.TryAcquire("env-1", holder("api"))
	if !ok {
		t.Fatal("second holder refused after the first released")
	}
	first() // a stale release, as a job that ends twice would make
	if got, held := l.Held("env-1"); !held || got.StackName != "api" {
		t.Errorf("Held = %+v, %v, want api still holding: a release frees only its own lock", got, held)
	}
}

func TestAttachOnlyNamesTheJobOfTheCurrentHolder(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	l.Attach("env-1", "job-x") // nothing holds it: nothing to attach to
	if _, held := l.Held("env-1"); held {
		t.Error("Attach made a holder out of nothing")
	}
}

func TestOnlyOneOfManyConcurrentTryAcquiresWins(t *testing.T) {
	t.Parallel()
	l := stackops.NewLocks()
	var won atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ok := l.TryAcquire("env-1", holder("web")); ok {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := won.Load(); got != 1 {
		t.Errorf("%d operations took the lock, want exactly 1", got)
	}
}
