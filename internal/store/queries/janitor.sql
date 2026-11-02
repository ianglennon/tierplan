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
