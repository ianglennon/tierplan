/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: GetBackupSchedule :one
-- The one schedule row; no row means the schedule was never saved.
SELECT id, enabled, frequency, weekday, time_utc, keep, passphrase_sealed, updated_at, updated_by
FROM backup_schedule
WHERE id = 1;

-- name: UpsertBackupSchedule :exec
INSERT INTO backup_schedule (id, enabled, frequency, weekday, time_utc, keep, passphrase_sealed, updated_at, updated_by)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  enabled           = excluded.enabled,
  frequency         = excluded.frequency,
  weekday           = excluded.weekday,
  time_utc          = excluded.time_utc,
  keep              = excluded.keep,
  passphrase_sealed = excluded.passphrase_sealed,
  updated_at        = excluded.updated_at,
  updated_by        = excluded.updated_by;

-- name: InsertBackupRun :exec
-- A run is recorded when it starts, so a process killed mid-backup leaves a run
-- with no finish time rather than no trace.
INSERT INTO backup_runs (id, source, started_at)
VALUES (?, ?, ?);

-- name: FinishBackupRun :exec
UPDATE backup_runs
SET finished_at = ?, ok = ?, file_name = ?, size_bytes = ?, error = ?
WHERE id = ?;

-- name: LastScheduleRun :one
-- The newest finished scheduled run, successful or not.
SELECT id, source, started_at, finished_at, ok, file_name, size_bytes, error
FROM backup_runs
WHERE source = 'schedule' AND finished_at IS NOT NULL
ORDER BY finished_at DESC, started_at DESC
LIMIT 1;

-- name: LastSuccessfulScheduleRun :one
SELECT id, source, started_at, finished_at, ok, file_name, size_bytes, error
FROM backup_runs
WHERE source = 'schedule' AND ok = 1 AND finished_at IS NOT NULL
ORDER BY finished_at DESC, started_at DESC
LIMIT 1;
