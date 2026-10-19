/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertStack :exec
INSERT INTO stacks (id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0);

-- name: GetStack :one
-- A stack is always looked up by its environment and ID, so an ID of another environment is not found.
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped
FROM stacks WHERE env_id = ? AND id = ?;

-- name: ListStacksByEnv :many
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped
FROM stacks WHERE env_id = ? ORDER BY name;

-- name: GetStackByName :one
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped
FROM stacks WHERE env_id = ? AND name = ?;

-- name: InsertSnapshot :exec
INSERT INTO stack_snapshots (id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetDraft :one
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at
FROM stack_snapshots WHERE stack_id = ? AND kind = 'draft';

-- name: UpdateDraftVersion :execrows
-- The optimistic save: it matches only while the draft is still at the version the caller read.
UPDATE stack_snapshots SET version = version + 1, created_by = ?, created_at = ?
WHERE id = ? AND version = ?;

-- name: DeleteSnapshotFiles :exec
DELETE FROM stack_snapshot_files WHERE snapshot_id = ?;

-- name: InsertSnapshotFile :exec
INSERT INTO stack_snapshot_files (snapshot_id, path, content, content_sealed, secret)
VALUES (?, ?, ?, ?, ?);

-- name: ListSnapshotFiles :many
SELECT snapshot_id, path, content, content_sealed, secret FROM stack_snapshot_files
WHERE snapshot_id = ? ORDER BY path;

-- name: DeleteSnapshotVars :exec
DELETE FROM stack_snapshot_vars WHERE snapshot_id = ?;

-- name: InsertSnapshotVar :exec
INSERT INTO stack_snapshot_vars (snapshot_id, name, value, value_sealed, secret)
VALUES (?, ?, ?, ?, ?);

-- name: ListSnapshotVars :many
SELECT snapshot_id, name, value, value_sealed, secret FROM stack_snapshot_vars
WHERE snapshot_id = ? ORDER BY name;

-- name: NextRevisionSeq :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) + 1 AS seq FROM stack_snapshots
WHERE stack_id = ? AND kind = 'revision';

-- name: LatestGoodRevision :one
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at
FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND outcome = 'good'
ORDER BY seq DESC LIMIT 1;

-- name: SetRevisionOutcome :execrows
UPDATE stack_snapshots SET outcome = ?, rendered = ?, pins = ? WHERE id = ? AND kind = 'revision';

-- name: InsertStackDeploy :exec
INSERT INTO stack_deploys (id, stack_id, env_id, kind, state, revision_seq, job_id, started_by, started_at)
VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?);

-- name: FinishStackDeploy :execrows
UPDATE stack_deploys SET state = ?, ended_at = ?, failed_service = ?, code = ?
WHERE id = ? AND state = 'running';

-- name: LatestStackDeploy :one
SELECT id, stack_id, env_id, kind, state, revision_seq, job_id, started_by, started_at, ended_at, failed_service, code
FROM stack_deploys WHERE stack_id = ? ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: LatestStackDeploysByEnv :many
-- The newest deploy row of each stack of an environment, for the list.
SELECT d.id, d.stack_id, d.env_id, d.kind, d.state, d.revision_seq, d.job_id, d.started_by, d.started_at, d.ended_at, d.failed_service, d.code
FROM stack_deploys d
WHERE d.env_id = ?
  AND d.id = (SELECT d2.id FROM stack_deploys d2 WHERE d2.stack_id = d.stack_id ORDER BY d2.started_at DESC, d2.id DESC LIMIT 1);

-- name: LatestDoneStackDeploysByEnv :many
-- The newest successful operation of each stack of an environment, for "last deployed".
SELECT d.id, d.stack_id, d.env_id, d.kind, d.state, d.revision_seq, d.job_id, d.started_by, d.started_at, d.ended_at, d.failed_service, d.code
FROM stack_deploys d
WHERE d.env_id = ? AND d.state = 'done' AND d.kind IN ('deploy', 'update', 'rollback')
  AND d.id = (SELECT d2.id FROM stack_deploys d2
              WHERE d2.stack_id = d.stack_id AND d2.state = 'done' AND d2.kind IN ('deploy', 'update', 'rollback')
              ORDER BY d2.started_at DESC, d2.id DESC LIMIT 1);

-- name: ListDraftsByEnv :many
-- The draft of each stack of an environment, for the list (its version and when it was saved).
SELECT dr.stack_id, dr.version, dr.created_by, dr.created_at FROM stack_snapshots dr
JOIN stacks s ON s.id = dr.stack_id
WHERE s.env_id = ? AND dr.kind = 'draft';

-- name: ListLatestRevisionTimesByEnv :many
-- When the newest revision of each stack of an environment was taken.
SELECT r.stack_id, CAST(MAX(r.created_at) AS INTEGER) AS taken_at FROM stack_snapshots r
JOIN stacks s ON s.id = r.stack_id
WHERE s.env_id = ? AND r.kind = 'revision'
GROUP BY r.stack_id;
