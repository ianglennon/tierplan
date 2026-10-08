// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/backup"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
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
	deadline := time.Now().Add(20 * time.Second)
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

// Plan 03-27 task 2: the next-run rules, retention, failures and the retry.

func utc(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func TestNextRunIsTheNextInstantTheScheduleNamesInUTC(t *testing.T) {
	daily := backup.Schedule{Enabled: true, Frequency: "daily", TimeUTC: "03:00"}
	sundays := backup.Schedule{Enabled: true, Frequency: "weekly", Weekday: 0, TimeUTC: "03:00"}
	fridays := backup.Schedule{Enabled: true, Frequency: "weekly", Weekday: 5, TimeUTC: "23:30"}
	off := backup.Schedule{Enabled: false, Frequency: "daily", TimeUTC: "03:00"}
	// 2026-10-07 is a Wednesday.
	for _, c := range []struct {
		name string
		s    backup.Schedule
		now  time.Time
		want time.Time
		ok   bool
	}{
		{"daily, a minute before", daily, utc(2026, 10, 7, 2, 59), utc(2026, 10, 7, 3, 0), true},
		{"daily, on the minute", daily, utc(2026, 10, 7, 3, 0), utc(2026, 10, 8, 3, 0), true},
		{"daily, after it", daily, utc(2026, 10, 7, 9, 15), utc(2026, 10, 8, 3, 0), true},
		{"daily, across a month end", daily, utc(2026, 10, 31, 4, 0), utc(2026, 11, 1, 3, 0), true},
		{"weekly Sunday, on a Wednesday", sundays, utc(2026, 10, 7, 12, 0), utc(2026, 10, 11, 3, 0), true},
		{"weekly Sunday, on the Sunday before the time", sundays, utc(2026, 10, 11, 2, 0), utc(2026, 10, 11, 3, 0), true},
		{"weekly Sunday, on the Sunday at the time", sundays, utc(2026, 10, 11, 3, 0), utc(2026, 10, 18, 3, 0), true},
		{"weekly Friday, on a Wednesday", fridays, utc(2026, 10, 7, 12, 0), utc(2026, 10, 9, 23, 30), true},
		{"weekly, given in another zone, still counts in UTC", sundays, time.Date(2026, 10, 11, 4, 30, 0, 0, time.FixedZone("x", 2*3600)), utc(2026, 10, 11, 3, 0), true},
		{"off", off, utc(2026, 10, 7, 12, 0), time.Time{}, false},
	} {
		got, ok := backup.NextRun(c.s, c.now)
		if ok != c.ok || !got.Equal(c.want) || (ok && got.Location() != time.UTC) {
			t.Errorf("%s: NextRun = %v, %v; want %v, %v (UTC)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// scheduled runs the scheduler once at 03:00 on 2026-10-08 and waits for the run.
func (f *schedFixture) runOnce(t *testing.T) {
	t.Helper()
	stop := f.run(t)
	defer stop()
	tm := f.clock.next(t)
	f.clock.set(utc(2026, 10, 8, 3, 0))
	tm.c <- f.clock.Now()
	f.runsUntil(t, 1)
}

func (f *schedFixture) names(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing was ever written
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestPruneKeepsTheNewestBackupsAndTouchesNoOtherFile(t *testing.T) {
	f := newSchedFixture(t, utc(2026, 10, 8, 1, 0))
	in := dailyAt3(exportPass)
	in.Keep = 3
	if _, err := f.saveSchedule(t, in); err != nil {
		t.Fatal(err)
	}
	others := []string{
		"bayplan-20260901T030000Z.db", "bayplan-20260902T030000Z.db", "bayplan-20260903T030000Z.db",
		"prerestore-20260901T030000Z.db", "prerestore-20260902T030000Z.db", "notes.txt", "bayplan-backup-20261001-0300.bayplan.bak",
	}
	for _, n := range others {
		writeFile(t, f.dir, n, "keep me")
	}
	for _, d := range []string{"20261001", "20261002", "20261003", "20261004", "20261005"} {
		writeFile(t, f.dir, "bayplan-backup-"+d+"-0300.bayplan", "old")
	}
	f.runOnce(t)

	got := f.names(t)
	want := append([]string{
		"bayplan-backup-20261004-0300.bayplan", "bayplan-backup-20261005-0300.bayplan", "bayplan-backup-20261008-0300.bayplan",
	}, others...)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the folder holds\n  %v\nwant\n  %v", got, want)
	}
}

func TestPruneRunsOnlyAfterABackupSucceeded(t *testing.T) {
	f := newSchedFixture(t, utc(2026, 10, 8, 1, 0))
	in := dailyAt3(exportPass)
	in.Keep = 1
	if _, err := f.saveSchedule(t, in); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE backup_schedule SET passphrase_sealed = 'not a sealed value'`)
	writeFile(t, f.dir, "bayplan-backup-20261001-0300.bayplan", "the only good one")
	writeFile(t, f.dir, "bayplan-backup-20261002-0300.bayplan", "another")
	f.runOnce(t)
	if got := f.names(t); len(got) != 2 {
		t.Fatalf("a failed run deleted backups: %v", got)
	}
}

type failureCase struct {
	name, stage string
	break_      func(t *testing.T, f *schedFixture)
	reason      string
}

func TestRunFailureIsRecordedAuditedAndLeavesNoPartialFile(t *testing.T) {
	for _, c := range []failureCase{
		{"the saved passphrase is below the minimum", "export", func(t *testing.T, f *schedFixture) {
			sealed, err := f.live.Seal(vault.ContextBackupSchedule, []byte("short"))
			if err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE backup_schedule SET passphrase_sealed = ?`, sealed)
		}, "shorter than the minimum"},
		{"the audit chain does not verify", "export", func(t *testing.T, f *schedFixture) {
			f.exec(t, `UPDATE audit_log SET action = 'x' || substr(action, 2) WHERE seq = 1`)
		}, "does not verify"},
		{"the saved passphrase cannot be opened", "passphrase", func(t *testing.T, f *schedFixture) {
			f.exec(t, `UPDATE backup_schedule SET passphrase_sealed = 'not a sealed value'`)
		}, "passphrase"},
		{"the file cannot be given its name", "write", func(t *testing.T, f *schedFixture) {
			writeFile(t, filepath.Join(f.dir, "bayplan-backup-20261008-0300.bayplan"), "in-the-way", "x")
		}, "name the backup file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSchedFixture(t, utc(2026, 10, 8, 1, 0))
			if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
				t.Fatal(err)
			}
			c.break_(t, f)
			stop := f.run(t)
			defer stop()
			tm := f.clock.next(t)
			f.clock.set(utc(2026, 10, 8, 3, 0))
			tm.c <- f.clock.Now()
			f.runsUntil(t, 1)

			var ok, size int64
			var file, reason string
			if err := f.st.Read.QueryRow(`SELECT ok, file_name, size_bytes, error FROM backup_runs`).Scan(&ok, &file, &size, &reason); err != nil {
				t.Fatal(err)
			}
			if ok != 0 || file != "" || size != 0 || !strings.Contains(reason, c.reason) {
				t.Errorf("run = ok %d file %q size %d error %q; want a failed run whose error has %q", ok, file, size, reason, c.reason)
			}
			var actor, result, details string
			if err := f.st.Read.QueryRow(`SELECT actor_id, result, details FROM audit_log WHERE action = 'backup.failed'`).Scan(&actor, &result, &details); err != nil {
				t.Fatalf("no backup.failed entry: %v", err)
			}
			var d map[string]string
			if err := json.Unmarshal([]byte(details), &d); err != nil || d["stage"] != c.stage || d["error"] == "" {
				t.Errorf("backup.failed details = %v, %v; want stage %q and the error", d, err, c.stage)
			}
			if actor != "system:backup" || result != "failure" || strings.Contains(details, exportPass) {
				t.Errorf("actor %q result %q details %s", actor, result, details)
			}
			var n int
			if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'backup.scheduled_run'`).Scan(&n); err != nil || n != 0 {
				t.Errorf("backup.scheduled_run entries = %d, %v after a failed run", n, err)
			}
			for _, name := range f.names(t) {
				if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
					t.Errorf("a partial file %q was left behind", name)
				}
			}
			// A failure does not stop the scheduler: tomorrow's run is planned.
			if next := f.clock.next(t); next.d != 24*time.Hour {
				t.Errorf("the next timer is for %v, want a day", next.d)
			}
		})
	}
}

// holdExport keeps the one export slot busy until release is closed.
type holdingSink struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *holdingSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return len(p), nil
}

func (f *schedFixture) holdExport(t *testing.T) (release func()) {
	t.Helper()
	sink := &holdingSink{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		_, _ = backup.Export(context.Background(), f.st, f.live, f.exportFixture.dir, sink, []byte(exportPass), backup.ExportOptions{})
		close(done)
	}()
	select {
	case <-sink.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the held export never started writing")
	}
	var once sync.Once
	release = func() { once.Do(func() { close(sink.release); <-done }) }
	t.Cleanup(release)
	return release
}

func TestRunFailureIsNotDeclaredWhileAnotherBackupIsBeingPreparedBecauseItIsRetriedAMinuteLater(t *testing.T) {
	f := newSchedFixture(t, utc(2026, 10, 8, 1, 0))
	if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	release := f.holdExport(t)
	stop := f.run(t)
	defer stop()
	tm := f.clock.next(t)
	f.clock.set(utc(2026, 10, 8, 3, 0))
	tm.c <- f.clock.Now()

	retry := f.clock.next(t)
	if retry.d != time.Minute {
		t.Fatalf("after finding the export slot busy the timer is for %v, want a minute", retry.d)
	}
	var failed int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'backup.failed'`).Scan(&failed); err != nil || failed != 0 {
		t.Fatalf("backup.failed entries = %d, %v while the first attempt is only waiting", failed, err)
	}

	release()
	f.clock.set(utc(2026, 10, 8, 3, 1))
	retry.c <- f.clock.Now()
	f.runsUntil(t, 1)
	var runs, ok int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*), SUM(ok) FROM backup_runs`).Scan(&runs, &ok); err != nil || runs != 1 || ok != 1 {
		t.Fatalf("runs = %d (ok %d), %v; want the one run to have succeeded on retry", runs, ok, err)
	}
	// It is still named for the instant it was due.
	if got := f.names(t); len(got) != 1 || got[0] != "bayplan-backup-20261008-0300.bayplan" {
		t.Fatalf("the folder holds %v", got)
	}
}

func TestRunFailureIsDeclaredWhenTheRetryFindsTheExportSlotBusyToo(t *testing.T) {
	f := newSchedFixture(t, utc(2026, 10, 8, 1, 0))
	if _, err := f.saveSchedule(t, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	f.holdExport(t)
	stop := f.run(t)
	defer stop()
	tm := f.clock.next(t)
	f.clock.set(utc(2026, 10, 8, 3, 0))
	tm.c <- f.clock.Now()
	retry := f.clock.next(t)
	f.clock.set(utc(2026, 10, 8, 3, 1))
	retry.c <- f.clock.Now()
	f.runsUntil(t, 1)

	var ok int64
	var reason string
	if err := f.st.Read.QueryRow(`SELECT ok, error FROM backup_runs`).Scan(&ok, &reason); err != nil || ok != 0 || !strings.Contains(reason, "another backup is being prepared") {
		t.Fatalf("run = ok %d, error %q, %v", ok, reason, err)
	}
}

func TestTheSavedPassphraseStillOpensAfterAKeyRotation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := store.Migrate(ctx, st, store.MigrateOptions{AppVersion: "test", DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	old, err := vault.LoadOrCreate("", dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, old, slog.New(slog.DiscardHandler), time.Now)
	if _, err := backup.SaveSchedule(ctx, st, old, aud, adminActor, dailyAt3(exportPass)); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.RotateKey(ctx, st, dir, old, audit.SystemCLI); err != nil {
		t.Fatal(err)
	}
	key, err := vault.ReadKeyFile(dir, vault.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := vault.New(key, "file")
	if err != nil {
		t.Fatal(err)
	}

	clock := newSchedClock(utc(2026, 10, 8, 1, 0))
	sch := &backup.Scheduler{
		Store: st, Vault: rotated, Audit: audit.NewWriter(st, rotated, slog.New(slog.DiscardHandler), time.Now),
		Dir: filepath.Join(t.TempDir(), "backups"), DataDir: dir, Now: clock.Now, NewTimer: clock.NewTimer, Log: slog.New(slog.DiscardHandler),
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { sch.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	tm := clock.next(t)
	clock.set(utc(2026, 10, 8, 3, 0))
	tm.c <- clock.Now()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var ok sql.NullInt64
		_ = st.Read.QueryRow(`SELECT MAX(ok) FROM backup_runs WHERE finished_at IS NOT NULL`).Scan(&ok)
		if ok.Valid {
			if ok.Int64 != 1 {
				t.Fatal("the run after the rotation failed")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run after the rotation never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTheSavedPassphraseTravelsInABackupSealedUnderTheBackupsOwnKey(t *testing.T) {
	f := newExportFixture(t)
	if _, err := f.saveSchedule(t, dailyAt3("the schedule passphrase")); err != nil {
		t.Fatal(err)
	}
	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	o := openBackup(t, file, exportPass)
	key, err := base64.StdEncoding.DecodeString(o.manifest.ExportKey)
	if err != nil {
		t.Fatal(err)
	}
	export, err := vault.New(key, "backup-export")
	if err != nil {
		t.Fatal(err)
	}
	var sealed string
	if err := o.db.QueryRow(`SELECT passphrase_sealed FROM backup_schedule WHERE id = 1`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if plain, err := export.Open(vault.ContextBackupSchedule, sealed); err != nil || string(plain) != "the schedule passphrase" {
		t.Fatalf("the passphrase in the backup = %q, %v", plain, err)
	}
}
