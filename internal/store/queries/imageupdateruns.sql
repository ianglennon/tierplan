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

-- name: BeginQueuedImageUpdateRun :execrows
-- A run that waited in a bulk update starts: the ID of the workload and the items it now knows (the images and digests the registry gave), the revision a
-- stack's update wrote, and the time it began. Only a run that is still queued starts; one that has ended, or was started already,
-- is left as it is, so the answer says whether this caller began it.
UPDATE image_update_runs
SET state = 'running', workload_id = ?, items = ?, revision_seq = ?, started_at = ?
WHERE id = ? AND state = 'queued' AND finished_at = 0;

-- name: SetImageUpdateRunOldContainer :execrows
-- Records the ID of the container an update replaces, before it is stopped. Only while the run is not over.
UPDATE image_update_runs SET old_container = ?
WHERE id = ? AND finished_at = 0;

-- name: FinishImageUpdateRun :execrows
-- The end of a run: its final state, why, when and the revisions it wrote. It is written once: a run that
-- has ended is not changed, so a late writer cannot overwrite a terminal state. A revision of 0 keeps the one the run
-- has, so an end that does not know the revision (a bulk update's clean-up) cannot erase the one a stack update wrote.
UPDATE image_update_runs
SET state = sqlc.arg(state), reason = sqlc.arg(reason), finished_at = sqlc.arg(finished_at),
    revision_seq = CASE WHEN CAST(sqlc.arg(revision_seq) AS INTEGER) = 0 THEN revision_seq ELSE CAST(sqlc.arg(revision_seq) AS INTEGER) END,
    rollback_seq = CASE WHEN CAST(sqlc.arg(rollback_seq) AS INTEGER) = 0 THEN rollback_seq ELSE CAST(sqlc.arg(rollback_seq) AS INTEGER) END
WHERE id = sqlc.arg(id) AND finished_at = 0;

-- name: RunningImageUpdateRuns :many
-- The runs of one environment that are queued or running, oldest first.
SELECT id, env_id, bulk_id, kind, workload, workload_id, stack_id, items, state, reason, actor, job_id, revision_seq, rollback_seq, old_container, started_at, finished_at
FROM image_update_runs
WHERE env_id = ? AND state IN ('queued', 'running')
ORDER BY started_at, id;

-- name: UnfinishedImageUpdateRuns :many
-- The runs of every environment that are queued or running and have not ended, oldest first. After a restart none of them
-- is still going: a run in this list belongs to a process that is gone, and startup recovery finds each of them here.
SELECT id, env_id, bulk_id, kind, workload, workload_id, stack_id, items, state, reason, actor, job_id, revision_seq, rollback_seq, old_container, started_at, finished_at
FROM image_update_runs
WHERE state IN ('queued', 'running') AND finished_at = 0
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
