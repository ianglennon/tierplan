// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/vault"
)

// The sources of a backup run, as backup_runs records them.
const runSourceSchedule = "schedule"

// reloadDelay is how long the scheduler waits to read a schedule it could not read.
const reloadDelay = time.Minute

// NextRun is the next instant after now at which the schedule fires, in UTC. It
// reports false for a schedule that is off or cannot fire. An instant equal to now
// has already fired: the next run is a day later.
func NextRun(s Schedule, now time.Time) (time.Time, bool) {
	if !s.Enabled {
		return time.Time{}, false
	}
	hour, minute, ok := parseClock(s.TimeUTC)
	if !ok {
		return time.Time{}, false
	}
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, true
}

// Scheduler writes the scheduled backups: it waits for the next instant the saved
// schedule names, makes an encrypted backup into Dir with the saved passphrase and
// records the run. It runs in the server's process for as long as the server does;
// no cron library is involved, only a timer for the next instant (D-32).
type Scheduler struct {
	Store *store.Store
	Vault *vault.Vault
	Audit *audit.Writer
	// Dir is the folder the backups are written into (BAYPLAN_BACKUP_DIR).
	Dir string
	// DataDir is where an export keeps its private work directory, on the same
	// volume as the database.
	DataDir string
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// AppVersion, InstanceName and PublicURL are recorded in each backup. Nil means
	// empty.
	AppVersion, InstanceName, PublicURL func() string
	// Log receives what cannot be reported any other way. Nil means slog.Default().
	Log *slog.Logger
	// NewTimer starts a timer; tests replace it. Nil means time.NewTimer.
	NewTimer func(d time.Duration) (c <-chan time.Time, stop func() bool)

	once    sync.Once
	changed chan struct{}
}

func (s *Scheduler) init() {
	s.once.Do(func() {
		s.changed = make(chan struct{}, 1)
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		if s.NewTimer == nil {
			s.NewTimer = func(d time.Duration) (<-chan time.Time, func() bool) {
				t := time.NewTimer(d)
				return t.C, t.Stop
			}
		}
	})
}

// Notify tells the scheduler the saved schedule changed, so it works out the next
// run again. It never blocks.
func (s *Scheduler) Notify() {
	s.init()
	select {
	case s.changed <- struct{}{}:
	default: // one notice is already waiting
	}
}

// Run blocks until ctx is done, making a backup at each instant the saved schedule
// names. Start it in a goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	s.init()
	var last time.Time // the newest instant a backup was made for
	for {
		wait, stop, due := s.arm(ctx, last)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-s.changed:
			stop()
		case <-wait:
			stop()
			if due.IsZero() {
				continue // the schedule could not be read; look again
			}
			last = due
			s.runDue(ctx, due)
		}
	}
}

// arm loads the schedule and starts the timer for its next run. due is the instant
// the timer is for, or zero when the timer is only a pause before the schedule is
// read again; a schedule that is off has no timer at all.
func (s *Scheduler) arm(ctx context.Context, last time.Time) (wait <-chan time.Time, stop func() bool, due time.Time) {
	idle := func() bool { return true }
	sched, err := LoadSchedule(ctx, s.Store)
	if err != nil {
		if ctx.Err() != nil {
			return nil, idle, time.Time{}
		}
		s.Log.Error("scheduled backups: cannot read the schedule", "error", err)
		wait, stop = s.NewTimer(reloadDelay)
		return wait, stop, time.Time{}
	}
	now := s.Now()
	// Never plan from before the last run, so a clock that stepped back cannot
	// make the same instant fire twice.
	from := now
	if last.After(from) {
		from = last
	}
	next, ok := NextRun(sched, from)
	if !ok {
		return nil, idle, time.Time{}
	}
	wait, stop = s.NewTimer(max(next.Sub(now), 0))
	return wait, stop, next
}

// A stage names the step of a backup that failed, for the audit entry and the log.
const (
	stagePassphrase = "passphrase"
	stageExport     = "export"
	stageWrite      = "write"
)

// stageError says which step of a scheduled backup failed.
type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func fail(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage: stage, err: err}
}

// runDue makes the backup for the instant due.
func (s *Scheduler) runDue(ctx context.Context, due time.Time) {
	started := s.Now().UTC()
	id, err := uuid.NewV7()
	if err != nil {
		s.Log.Error("scheduled backup: cannot make a run ID", "error", err)
		return
	}
	if err := s.Store.Q.InsertBackupRun(ctx, sqlc.InsertBackupRunParams{ID: id.String(), Source: runSourceSchedule, StartedAt: started.UnixNano()}); err != nil {
		s.Log.Error("scheduled backup: cannot record the run", "error", err)
		return
	}
	name, size, err := s.backupTo(ctx, due)
	if err != nil {
		s.Log.Error("scheduled backup failed", "error", err)
		return
	}
	finish := sqlc.FinishBackupRunParams{
		FinishedAt: sql.NullInt64{Int64: s.Now().UTC().UnixNano(), Valid: true}, Ok: 1, FileName: name, SizeBytes: size, ID: id.String(),
	}
	if err := s.Store.Q.FinishBackupRun(ctx, finish); err != nil {
		s.Log.Error("scheduled backup: cannot record the run's end", "error", err)
	}
	if err := s.Audit.Record(ctx, audit.Entry{
		Actor:      audit.SystemBackup,
		Action:     audit.ActionBackupScheduledRun,
		TargetType: targetBackup,
		TargetID:   name,
		Result:     audit.ResultSuccess,
		Details:    map[string]string{"file": name, "size": strconv.FormatInt(size, 10)},
	}); err != nil {
		s.Log.Error("scheduled backup: cannot audit the run", "error", err)
	}
}

// targetBackup is the audit target of a backup.
const targetBackup = "backup"

// backupTo exports into a temporary file in Dir and renames it to its final name,
// so a backup that is listed or downloaded is always a whole one.
func (s *Scheduler) backupTo(ctx context.Context, due time.Time) (name string, size int64, err error) {
	pass, err := schedulePassphrase(ctx, s.Store, s.Vault)
	if err != nil {
		return "", 0, fail(stagePassphrase, err)
	}
	defer clear(pass)

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", 0, fail(stageWrite, fmt.Errorf("create the backups folder: %w", err))
	}
	tmp, err := os.CreateTemp(s.Dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return "", 0, fail(stageWrite, fmt.Errorf("create the backup file: %w", err))
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	res, err := Export(ctx, s.Store, s.Vault, s.DataDir, tmp, pass, ExportOptions{
		AppVersion: call(s.AppVersion), InstanceName: call(s.InstanceName), PublicURL: call(s.PublicURL),
		Now: s.Now,
	})
	if err != nil {
		return "", 0, fail(stageExport, err)
	}
	if err = tmp.Sync(); err != nil {
		return "", 0, fail(stageWrite, fmt.Errorf("flush the backup file: %w", err))
	}
	if err = tmp.Close(); err != nil {
		return "", 0, fail(stageWrite, fmt.Errorf("close the backup file: %w", err))
	}
	name = ScheduledName(due)
	if err = os.Rename(tmp.Name(), filepath.Join(s.Dir, name)); err != nil {
		return "", 0, fail(stageWrite, fmt.Errorf("name the backup file: %w", err))
	}
	return name, res.Size, nil
}

func call(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}

// The temporary file a scheduled backup is written to before it gets its name.
const (
	tempPrefix = ".bayplan-schedule-"
	tempSuffix = ".tmp"
)
