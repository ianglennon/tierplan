// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

func fakeClient(f *dockertest.Fake) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: f.DialContext}}
}

func awaitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
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
