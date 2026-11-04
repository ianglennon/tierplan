/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: InsertImageUpdateRun :exec
-- An update, on record before it touches an engine.
INSERT INTO image_update_runs (id, env_id, bulk_id, kind, workload, workload_id, stack_id, items, state, reason, actor, job_id, revision_seq, rollback_seq, old_container, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: SetImageUpdateRunState :execrows
-- Moves a run that is not over to a state. A run that has ended is never changed by this, so a late
-- call cannot undo the final state.
UPDATE image_update_runs SET state = ?
WHERE id = ? AND finished_at = 0;

-- name: SetImageUpdateRunOldContainer :execrows
-- Records the ID of the container an update replaces, before it is stopped. Only while the run is not over.
UPDATE image_update_runs SET old_container = ?
WHERE id = ? AND finished_at = 0;

-- name: FinishImageUpdateRun :execrows
-- The end of a run: its final state, why, when and the revisions it wrote. It is written once: a run that
-- has ended is not changed, so a late writer cannot overwrite a terminal state.
UPDATE image_update_runs
SET state = ?, reason = ?, finished_at = ?, revision_seq = ?, rollback_seq = ?
WHERE id = ? AND finished_at = 0;

-- name: RunningImageUpdateRuns :many
-- The runs of one environment that are queued or running, oldest first.
SELECT id, env_id, bulk_id, kind, workload, workload_id, stack_id, items, state, reason, actor, job_id, revision_seq, rollback_seq, old_container, started_at, finished_at
FROM image_update_runs
WHERE env_id = ? AND state IN ('queued', 'running')
ORDER BY started_at, id;

-- name: GetImageUpdateRun :one
SELECT id, env_id, bulk_id, kind, workload, workload_id, stack_id, items, state, reason, actor, job_id, revision_seq, rollback_seq, old_container, started_at, finished_at
FROM image_update_runs
WHERE id = ?;

-- name: ImageUpdateRevisionSeqs :many
-- The revisions of one stack that an image update made, oldest first. History labels them "Image update".
SELECT revision_seq
FROM image_update_runs
WHERE stack_id = ? AND revision_seq > 0
ORDER BY revision_seq;
