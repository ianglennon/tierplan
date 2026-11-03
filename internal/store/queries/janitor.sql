/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: InsertJanitorRun :exec
-- A run, on record before it removes anything.
INSERT INTO janitor_runs (id, env_id, trigger, actor, state, reason, job_id, started_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: SetJanitorRunState :execrows
-- Moves a run that is not over to a state and gives it its job. A run that has ended is never
-- changed by this, so a late call cannot undo the final state.
UPDATE janitor_runs SET state = ?, job_id = ?
WHERE id = ? AND finished_at = 0;

-- name: FinishJanitorRun :execrows
-- The end of a run: its final state, why, when and what it counted.
UPDATE janitor_runs
SET state = ?, reason = ?, finished_at = ?,
    removed_containers = ?, removed_images = ?, removed_networks = ?,
    kept = ?, failed = ?, skipped_offline = ?, est_bytes = ?
WHERE id = ?;

-- name: InsertJanitorRunItem :exec
INSERT INTO janitor_run_items (run_id, seq, type, node_id, node_name, ref, name, reason, reason_detail, outcome, code, est_bytes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetJanitorRun :one
-- One run of one environment: another environment's run is not found.
SELECT id, env_id, trigger, actor, state, reason, job_id, started_at, finished_at,
       removed_containers, removed_images, removed_networks, kept, failed, skipped_offline, est_bytes
FROM janitor_runs
WHERE env_id = ? AND id = ?;

-- name: ListJanitorRunItems :many
SELECT run_id, seq, type, node_id, node_name, ref, name, reason, reason_detail, outcome, code, est_bytes
FROM janitor_run_items
WHERE run_id = ?
ORDER BY seq;

-- name: InsertJanitorEvidence :exec
-- The crash evidence of a container, saved before the janitor removes it.
INSERT INTO janitor_evidence (run_id, seq, exit_code, error, oom_killed, started_at, finished_at, image, image_id, image_digest, node, log_text, log_lines, log_state)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetJanitorEvidence :one
-- The evidence of one item of one run of one environment: another environment's run has none.
SELECT e.run_id, e.seq, e.exit_code, e.error, e.oom_killed, e.started_at, e.finished_at, e.image, e.image_id, e.image_digest, e.node, e.log_text, e.log_lines, e.log_state
FROM janitor_evidence e
JOIN janitor_runs r ON r.id = e.run_id
WHERE r.env_id = ? AND e.run_id = ? AND e.seq = ?;

-- name: ListJanitorEvidenceSeqs :many
-- The items of a run that have evidence, without reading any log text.
SELECT seq FROM janitor_evidence WHERE run_id = ? ORDER BY seq;

-- name: ListJanitorRuns :many
-- The runs of one environment, newest first. The same started_at is ordered by id, the order the
-- retention's newest N uses too.
SELECT id, env_id, trigger, actor, state, reason, job_id, started_at, finished_at,
       removed_containers, removed_images, removed_networks, kept, failed, skipped_offline, est_bytes
FROM janitor_runs
WHERE env_id = ?
ORDER BY started_at DESC, id DESC
LIMIT ?;

-- name: CountJanitorRuns :one
-- How many runs an environment has on record.
SELECT COUNT(*) FROM janitor_runs WHERE env_id = ?;

-- name: PruneJanitorRuns :execrows
-- Retention of one environment's run history (D-16): a run is kept while it started at or after the
-- cutoff (now minus the retention days), or while it is among the environment's newest keep runs
-- (the order the history lists them in: started_at, then id, newest first), whichever keeps more. A
-- run that is waiting or running is never deleted. The foreign keys take the run's items and
-- evidence with it.
DELETE FROM janitor_runs
WHERE janitor_runs.env_id = sqlc.arg(env_id)
  AND janitor_runs.state NOT IN ('waiting', 'running')
  AND janitor_runs.started_at < sqlc.arg(cutoff)
  AND janitor_runs.id NOT IN (SELECT k.id FROM janitor_runs k
                              WHERE k.env_id = janitor_runs.env_id
                              ORDER BY k.started_at DESC, k.id DESC LIMIT sqlc.arg(keep));

-- name: PruneAllJanitorRuns :execrows
-- PruneJanitorRuns over every environment, each by its own newest keep runs.
DELETE FROM janitor_runs
WHERE janitor_runs.state NOT IN ('waiting', 'running')
  AND janitor_runs.started_at < sqlc.arg(cutoff)
  AND janitor_runs.id NOT IN (SELECT k.id FROM janitor_runs k
                              WHERE k.env_id = janitor_runs.env_id
                              ORDER BY k.started_at DESC, k.id DESC LIMIT sqlc.arg(keep));

-- name: MarkJanitorRunsInterrupted :many
-- Run at start, before anything can start a run: a run still waiting or running belongs to a process
-- that is gone. It ends failed with the reason server_restarted, at the sweep's time (never before it
-- started), keeps the items it had recorded, and counts them the way a finished run does. Nothing is
-- added to it. One statement, so no run is half swept.
UPDATE janitor_runs
SET state = 'failed', reason = 'server_restarted', finished_at = MAX(janitor_runs.started_at, sqlc.arg(finished_at)),
    removed_containers = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.type = 'container' AND i.outcome = 'removed'),
    removed_images = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.type = 'image' AND i.outcome = 'removed'),
    removed_networks = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.type = 'network' AND i.outcome = 'removed'),
    kept = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id
            AND (i.outcome = 'refused_in_use' OR (i.outcome = 'skipped_protected' AND i.code <> 'gone'))),
    failed = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.outcome = 'error'),
    skipped_offline = (SELECT COUNT(*) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.outcome = 'skipped_node_offline'),
    est_bytes = COALESCE((SELECT SUM(i.est_bytes) FROM janitor_run_items i WHERE i.run_id = janitor_runs.id AND i.outcome = 'removed' AND i.est_bytes >= 0), -1)
WHERE janitor_runs.state IN ('waiting', 'running')
RETURNING id, env_id, trigger, actor, state, reason, job_id, started_at, finished_at,
          removed_containers, removed_images, removed_networks, kept, failed, skipped_offline, est_bytes;

-- name: GetJanitorDefaults :one
-- The instance defaults: the one row the migration seeds.
SELECT enabled, schedule, schedule_preset, keep_n, grace_hours, remove_unused_networks,
       remove_stopped_containers, retention_days, retention_runs, version, updated_by, updated_at
FROM janitor_defaults WHERE id = 1;

-- name: GetJanitorEnvPolicy :one
-- One environment's overrides; no row means it follows every default.
SELECT env_id, enabled, schedule, schedule_preset, keep_n, grace_hours, remove_unused_networks,
       remove_stopped_containers, version, updated_by, updated_at
FROM janitor_env_policy WHERE env_id = ?;

-- name: InsertJanitorEnvPolicy :execrows
-- The first save of an environment's overrides (NULL for every field it follows), at version 1.
-- Zero rows means the environment already has a row, so the save was made from a stale read.
INSERT INTO janitor_env_policy (env_id, enabled, schedule, schedule_preset, keep_n, grace_hours,
                                remove_unused_networks, remove_stopped_containers, version, updated_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (env_id) DO NOTHING;

-- name: UpdateJanitorEnvPolicy :execrows
-- Saves an environment's overrides only when the version is the one the caller read; zero rows
-- means it was not.
UPDATE janitor_env_policy
SET enabled = ?, schedule = ?, schedule_preset = ?, keep_n = ?, grace_hours = ?,
    remove_unused_networks = ?, remove_stopped_containers = ?,
    version = version + 1, updated_by = ?, updated_at = ?
WHERE env_id = ? AND version = ?;

-- name: ListJanitorNodePolicies :many
-- The per-node overrides of one environment.
SELECT env_id, node_id, enabled, keep_n, updated_by, updated_at
FROM janitor_node_policy WHERE env_id = ? ORDER BY node_id;

-- name: UpdateJanitorDefaults :execrows
-- Saves the instance defaults and the run-history retention only when the version is the one the caller
-- read; zero rows means it was not. The row is seeded by the migration, so there is always one.
UPDATE janitor_defaults
SET enabled = ?, schedule = ?, schedule_preset = ?, keep_n = ?, grace_hours = ?,
    remove_unused_networks = ?, remove_stopped_containers = ?, retention_days = ?, retention_runs = ?,
    version = version + 1, updated_by = ?, updated_at = ?
WHERE id = 1 AND version = ?;

-- name: CountJanitorFollowers :one
-- How many environments follow at least one instance default: those with no policy row and those whose
-- row leaves any field NULL (the schedule and its preset are set together, so the schedule stands for both).
SELECT COUNT(*) FROM environments e
LEFT JOIN janitor_env_policy p ON p.env_id = e.id
WHERE p.env_id IS NULL
   OR p.enabled IS NULL OR p.schedule IS NULL OR p.keep_n IS NULL OR p.grace_hours IS NULL
   OR p.remove_unused_networks IS NULL OR p.remove_stopped_containers IS NULL;

-- name: CountJanitorRunsToPrune :one
-- How many runs PruneAllJanitorRuns (or PruneJanitorRuns, when env_id is not empty) would delete now, and
-- from how many environments, by the same rule: the answer a retention preview gives.
SELECT COUNT(*) AS runs, COUNT(DISTINCT janitor_runs.env_id) AS environments
FROM janitor_runs
WHERE (CAST(sqlc.arg(env_id) AS TEXT) = '' OR janitor_runs.env_id = sqlc.arg(env_id))
  AND janitor_runs.state NOT IN ('waiting', 'running')
  AND janitor_runs.started_at < sqlc.arg(cutoff)
  AND janitor_runs.id NOT IN (SELECT k.id FROM janitor_runs k
                              WHERE k.env_id = janitor_runs.env_id
                              ORDER BY k.started_at DESC, k.id DESC LIMIT sqlc.arg(keep));
