// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/backup"
)

// The scheduler writes the scheduled backups (plan 03-27, D-32).

// schedClock is the scheduler's clock and timers, driven by the test.
type schedClock struct {
	mu    sync.Mutex
	now   time.Time
	armed chan *schedTimer
}

// schedTimer is one timer the scheduler started.
type schedTimer struct {
	d       time.Duration
	c       chan time.Time
	mu      sync.Mutex
	stopped bool
}

func newSchedClock(now time.Time) *schedClock {
	return &schedClock{now: now, armed: make(chan *schedTimer, 16)}
}

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *schedClock) NewTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := &schedTimer{d: d, c: make(chan time.Time, 1)}
	c.armed <- t
	return t.c, func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// next waits for the scheduler to start a timer.
func (c *schedClock) next(t *testing.T) *schedTimer {
	t.Helper()
	select {
	case tm := <-c.armed:
		return tm
	case <-time.After(30 * time.Second):
		t.Fatal("the scheduler did not start a timer")
		return nil
	}
}

type schedFixture struct {
	*exportFixture
	clock *schedClock
	dir   string // the backups folder
	sch   *backup.Scheduler
}

func newSchedFixture(t *testing.T, now time.Time) *schedFixture {
	t.Helper()
	f := &schedFixture{exportFixture: newExportFixture(t), clock: newSchedClock(now)}
	f.dir = filepath.Join(t.TempDir(), "backups")
	f.sch = &backup.Scheduler{
		Store: f.st, Vault: f.live, Audit: f.aud, Dir: f.dir, DataDir: f.exportFixture.dir,
		Now: f.clock.Now, NewTimer: f.clock.NewTimer, Log: slog.New(slog.DiscardHandler),
		AppVersion: func() string { return "v9.9.9" }, InstanceName: func() string { return "Home lab" },
		PublicURL: func() string { return "https://bayplan.example" },
	}
	return f
}

// run starts the scheduler and returns a stop that waits for it to end.
func (f *schedFixture) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.sch.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the scheduler did not stop with its context")
		}
	}
}

// runsUntil polls the run history until it holds n finished runs.
func (f *schedFixture) runsUntil(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var got int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM backup_runs WHERE finished_at IS NOT NULL`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d scheduled runs finished, want %d", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSchedulerWritesADailyBackupAtTheSetTimeThatOpensWithThePassphrase(t *testing.T) {
	f := newSchedFixture(t, time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC))
	if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	stop := f.run(t)
	defer stop()

	first := f.clock.next(t)
	if first.d != 2*time.Hour {
		t.Fatalf("the first timer is for %v, want the 2h until 03:00", first.d)
	}
	f.clock.set(time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC))
	first.c <- f.clock.Now()
	f.runsUntil(t, 1)

	const name = "bayplan-backup-20261008-0300.bayplan"
	path := filepath.Join(f.dir, name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the backup is not where it should be: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	bf, err := os.Open(path) //nolint:gosec // the backup the scheduler just wrote
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bf.Close() }()
	r, err := backup.Open(bf, []byte(exportPass))
	if err != nil {
		t.Fatalf("the scheduled backup does not open with the saved passphrase: %v", err)
	}
	entry, body, err := r.Next()
	if err != nil || entry != backup.EntryManifest {
		t.Fatalf("first entry = %q, %v", entry, err)
	}
	var m backup.Manifest
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.AppVersion != "v9.9.9" || m.InstanceName != "Home lab" || m.Counts.Users != 1 {
		t.Errorf("manifest = %+v", m)
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
		t.Errorf("the folder holds %d entries, want just the backup", len(entries))
	}

	var source, file string
	var ok, size int64
	if err := f.st.Read.QueryRow(`SELECT source, ok, file_name, size_bytes FROM backup_runs`).Scan(&source, &ok, &file, &size); err != nil {
		t.Fatal(err)
	}
	if source != "schedule" || ok != 1 || file != name || size != info.Size() {
		t.Errorf("run = %s ok=%d %s %d bytes; want schedule ok=1 %s %d bytes", source, ok, file, size, name, info.Size())
	}
	var actor, target string
	if err := f.st.Read.QueryRow(`SELECT actor_id, target_id FROM audit_log WHERE action = 'backup.scheduled_run'`).Scan(&actor, &target); err != nil {
		t.Fatalf("no backup.scheduled_run entry: %v", err)
	}
	if actor != "system:backup" || target != name {
		t.Errorf("audit actor %q target %q", actor, target)
	}

	// Having run, it waits for tomorrow's.
	if second := f.clock.next(t); second.d != 24*time.Hour {
		t.Errorf("the next timer is for %v, want a day", second.d)
	}
}

func TestSchedulerDoesNothingWhileTheScheduleIsOff(t *testing.T) {
	f := newSchedFixture(t, time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC))
	stop := f.run(t)
	defer stop()
	select {
	case tm := <-f.clock.armed:
		t.Fatalf("a timer for %v was started with the schedule off", tm.d)
	case <-time.After(200 * time.Millisecond):
	}

	// Saving a schedule wakes it.
	if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	f.sch.Notify()
	if tm := f.clock.next(t); tm.d != 2*time.Hour {
		t.Fatalf("after the save the timer is for %v, want 2h", tm.d)
	}
}
