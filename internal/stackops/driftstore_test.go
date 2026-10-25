// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

func TestDriftStoreSharesOneCheck(t *testing.T) {
	t.Parallel()
	const callers = 4
	store := stackops.NewDriftStore()
	var runs atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (stackops.DriftResult, error) {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-release
		return stackops.DriftResult{State: stackops.DriftDrifted, Revision: 7, Count: 2}, nil
	}

	results := make(chan stackops.DriftResult, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := store.Check(testsupport.Context(t), "stack-1", fn)
			if err != nil {
				t.Errorf("check: %v", err)
			}
			results <- res
		}()
	}
	testsupport.Await(t, "the check to start", started)
	testsupport.WaitFor(t, "every caller to wait on the one check", func() bool { return store.Waiting("stack-1") == callers })
	close(release)
	wg.Wait()
	close(results)

	if n := runs.Load(); n != 1 {
		t.Errorf("the check ran %d times for %d callers, want once", n, callers)
	}
	for res := range results {
		if res.Revision != 7 || res.Count != 2 {
			t.Errorf("a caller got %+v, want the shared result", res)
		}
	}
	if got, ok := store.Get("stack-1"); !ok || got.Revision != 7 {
		t.Errorf("stored = %+v %v, want the result kept", got, ok)
	}
	if store.Waiting("stack-1") != 0 {
		t.Error("the check is still counted after it finished")
	}
	// The stack is free again: the next check runs on its own.
	if _, err := store.Check(testsupport.Context(t), "stack-1", func(context.Context) (stackops.DriftResult, error) {
		runs.Add(1)
		return stackops.DriftResult{State: stackops.DriftNone}, nil
	}); err != nil || runs.Load() != 2 {
		t.Errorf("a second check = %v after %d runs, want it to run", err, runs.Load())
	}
	if got, _ := store.Get("stack-1"); got.State != stackops.DriftNone {
		t.Errorf("stored = %+v, want the newer result", got)
	}
}

func TestDriftStoreKeepsNothingOfAFailedCheckAndForgets(t *testing.T) {
	t.Parallel()
	store := stackops.NewDriftStore()
	boom := errors.New("boom")

	if _, err := store.Check(testsupport.Context(t), "s", func(context.Context) (stackops.DriftResult, error) {
		return stackops.DriftResult{}, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("check = %v, want the failure", err)
	}
	if _, ok := store.Get("s"); ok {
		t.Error("a failed check was stored")
	}
	store.Put("s", stackops.DriftResult{State: stackops.DriftNone})
	store.Forget("s")
	if _, ok := store.Get("s"); ok {
		t.Error("Forget kept the result")
	}
}

func TestDriftStoreCallerThatGivesUpLeavesTheCheckForTheOthers(t *testing.T) {
	t.Parallel()
	store := stackops.NewDriftStore()
	started, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (stackops.DriftResult, error) {
		close(started)
		<-release
		return stackops.DriftResult{Revision: 3}, nil
	}
	first, cancel := context.WithCancel(testsupport.Context(t))
	gone := make(chan error, 1)
	go func() {
		_, err := store.Check(first, "s", fn)
		gone <- err
	}()
	testsupport.Await(t, "the check to start", started)
	other := make(chan stackops.DriftResult, 1)
	go func() {
		res, _ := store.Check(testsupport.Context(t), "s", fn)
		other <- res
	}()
	testsupport.WaitFor(t, "the second caller to join", func() bool { return store.Waiting("s") == 2 })

	cancel()
	if err := testsupport.Await(t, "the first caller to give up", gone); !errors.Is(err, context.Canceled) {
		t.Errorf("first caller = %v, want its own cancellation", err)
	}
	close(release)

	if res := testsupport.Await(t, "the second caller's result", other); res.Revision != 3 {
		t.Errorf("second caller got %+v, want the check to finish for it", res)
	}
}
