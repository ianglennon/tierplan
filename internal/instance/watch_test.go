// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// watchDataDir opens the same new data directory twice, as the server and the CLI do
// in two processes, and returns the migrated first handle and the second one.
func watchDataDir(t *testing.T) (server, other *store.Store) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	server, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := store.Migrate(ctx, server, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	other, err = store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return server, other
}

// watchPut upserts one settings row through st, as another process would.
func watchPut(t *testing.T, st *store.Store, key, value string) {
	t.Helper()
	err := st.Q.UpsertSetting(context.Background(), sqlc.UpsertSettingParams{
		Key: key, Value: value, UpdatedAt: time.Now().UTC().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
}

// watchClock is a clock the test moves. After arms a timer and announces it, so a test
// that moves the clock knows the watcher is waiting on it and never sleeps to find out.
type watchClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []watchTimer
	armed  chan struct{}
}

type watchTimer struct {
	at time.Time
	ch chan time.Time
}

func newWatchClock() *watchClock {
	return &watchClock{
		now:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		armed: make(chan struct{}, 4096),
	}
}

func (c *watchClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *watchClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, watchTimer{at: c.now.Add(d), ch: ch})
	c.armed <- struct{}{}
	return ch
}

// Advance moves the clock on by d and fires every timer that is due.
func (c *watchClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}

func TestWatcherPollAppliesAPublicURLWrittenByAnotherHandle(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingPublicURL, "https://tierplan.example.com:8443")
	inst := mustLoad(t, server, config.Config{})
	if inst.PublicURL() != "https://tierplan.example.com:8443" || !inst.Secure() {
		t.Fatalf("loaded %q secure=%v", inst.PublicURL(), inst.Secure())
	}

	w := &instance.Watcher{Inst: inst, Store: server, Clock: newWatchClock(), Log: slog.New(slog.DiscardHandler)}
	if changed, err := w.Poll(context.Background()); err != nil || len(changed) != 0 {
		t.Fatalf("a poll with nothing written = %v, %v", changed, err)
	}

	watchPut(t, other, instance.SettingPublicURL, "https://other.example.com:8443")
	changed, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{instance.SettingPublicURL}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	if inst.PublicURL() != "https://other.example.com:8443" {
		t.Fatalf("PublicURL = %q", inst.PublicURL())
	}
	if !inst.Secure() || !inst.CookieScope().Secure {
		t.Fatalf("not secure after the URL changed: Secure=%v CookieScope=%+v", inst.Secure(), inst.CookieScope())
	}
	if inst.PublicOrigin() != "https://other.example.com:8443" {
		t.Fatalf("PublicOrigin = %q", inst.PublicOrigin())
	}
}

func TestWatcherRunPollsOnlyWhenTheClockFires(t *testing.T) {
	server, other := watchDataDir(t)
	watchPut(t, other, instance.SettingPublicURL, "https://tierplan.example.com:8443")
	inst := mustLoad(t, server, config.Config{})

	clock := newWatchClock()
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	defer cancel()
	done := make(chan struct{})
	w := &instance.Watcher{Inst: inst, Store: server, Clock: clock, Log: slog.New(slog.DiscardHandler)}
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	// Run is waiting on the clock, which has not moved: the write must not be seen.
	testsupport.Await(t, "the watcher to arm its first tick", clock.armed)
	watchPut(t, other, instance.SettingPublicURL, "https://other.example.com:8443")
	if inst.PublicURL() != "https://tierplan.example.com:8443" {
		t.Fatalf("applied before the clock fired: %q", inst.PublicURL())
	}

	// A step shorter than the interval fires nothing.
	clock.Advance(instance.SettingsPollInterval - time.Second)
	if inst.PublicURL() != "https://tierplan.example.com:8443" {
		t.Fatalf("applied before the interval had passed: %q", inst.PublicURL())
	}

	// The full interval fires the poll; the next arm means that poll is done.
	clock.Advance(time.Second)
	testsupport.Await(t, "the watcher to arm its next tick", clock.armed)
	if inst.PublicURL() != "https://other.example.com:8443" {
		t.Fatalf("not applied after the tick: %q", inst.PublicURL())
	}

	cancel()
	testsupport.Await(t, "Run to return", done)
}
