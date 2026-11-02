// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockertest_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

func fakeClient(f *dockertest.Fake) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: f.DialContext}}
}

func awaitCtx(t *testing.T) context.Context {
	t.Helper()
	return testsupport.Context(t)
}

func TestFakeAwaitReturnsOnceARequestHasMadeTheConditionTrue(t *testing.T) {
	f := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx := awaitCtx(t)
	done := make(chan error, 1)
	go func() {
		done <- f.Await(ctx, func() bool { return len(f.Requests()) >= 1 })
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://fake/_ping", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-done; err != nil {
		t.Fatalf("Await: %v", err)
	}
}

func TestFakeAwaitReturnsTheContextErrorWhenTheConditionNeverHolds(t *testing.T) {
	f := dockertest.NewFake(t, dockertest.FakeConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.Await(ctx, func() bool { return false }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Await = %v, want context.Canceled", err)
	}
}

func TestFakeAwaitSeesALongLivedFollowStreamOpenAndThenClosed(t *testing.T) {
	f := dockertest.NewFake(t, dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/c1"}, State: "running"}},
	})
	ctx := awaitCtx(t)
	rctx, leave := context.WithCancel(ctx)
	defer leave()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://fake/containers/c1/logs?follow=1&stdout=1", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := f.Await(ctx, func() bool { return f.LogFollows() == 1 }); err != nil {
		t.Fatalf("the follow was never seen open (LogFollows = %d): %v", f.LogFollows(), err)
	}
	leave()
	if err := f.Await(ctx, func() bool { return f.LogFollows() == 0 }); err != nil {
		t.Fatalf("the follow was never seen closed (LogFollows = %d): %v", f.LogFollows(), err)
	}
}

func listRequests(f *dockertest.Fake) int {
	n := 0
	for _, p := range f.Requests() {
		if p == "/containers/json" {
			n++
		}
	}
	return n
}

func getList(ctx context.Context, f *dockertest.Fake) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://fake/containers/json", http.NoBody)
	if err != nil {
		return nil, err
	}
	return fakeClient(f).Do(req)
}

func TestFakeListHoldRecordsAListAndAnswersOnlyOnceTheHoldIsClosed(t *testing.T) {
	hold := make(chan struct{})
	f := dockertest.NewFake(t, dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{{ID: "c1", Names: []string{"/c1"}, State: "running"}},
		ListHold:   hold,
	})
	ctx := testsupport.Context(t)
	answered := make(chan error, 1)
	go func() {
		resp, err := getList(ctx, f)
		if err == nil {
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	if err := f.Await(ctx, func() bool { return listRequests(f) == 1 }); err != nil {
		t.Fatalf("the held list was never recorded: %v", err)
	}
	// Fence: a request the fake serves after the list was recorded and held. When it has
	// answered, a list that was not held would have answered long since.
	ping, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://fake/_ping", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fakeClient(f).Do(ping)
	if err != nil {
		t.Fatalf("a probe while a list is held: %v", err)
	}
	_ = resp.Body.Close()
	select {
	case err := <-answered:
		t.Fatalf("the list answered (%v) while the hold was open", err)
	default:
	}
	close(hold)
	if err := testsupport.Await(t, "the held list to answer once the hold closed", answered); err != nil {
		t.Fatalf("the list after the hold closed: %v", err)
	}
}

func TestFakeListHoldFreesTheHandlerOfACallerThatGaveUp(t *testing.T) {
	// The hold is never closed. The fake's server closes at cleanup and waits for its
	// handlers, so a handler that ignored the request's context would hang this test.
	f := dockertest.NewFake(t, dockertest.FakeConfig{ListHold: make(chan struct{})})
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		resp, err := getList(ctx, f)
		if err == nil {
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	if err := f.Await(ctx, func() bool { return listRequests(f) == 1 }); err != nil {
		t.Fatalf("the held list was never recorded: %v", err)
	}
	cancel()
	if err := testsupport.Await(t, "the caller that gave up to return", answered); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled held list = %v, want context.Canceled", err)
	}
}
