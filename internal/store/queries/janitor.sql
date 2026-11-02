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
