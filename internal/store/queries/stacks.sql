/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: InsertStack :exec
INSERT INTO stacks (id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0);

-- name: InsertAdoptedStack :exec
-- InsertStack with the adoption record (D-14): the stack of a discovered stack Coaming took over.
INSERT INTO stacks (id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped, adoption)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?);

-- name: SetStackAdoption :execrows
-- Replaces the adoption record of a stack. The caller read the stack by environment and ID first.
UPDATE stacks SET adoption = ? WHERE id = ?;

-- name: ClearStackAdoptionPending :execrows
-- Ends the pending window of an adopted stack (the first good deploy, D-16) and keeps the rest of the record. The
-- flag is omitted from the record when false, so removing it is clearing it.
UPDATE stacks SET adoption = json_remove(adoption, '$.pending') WHERE id = ?;

-- name: GetStack :one
-- A stack is always looked up by its environment and ID, so an ID of another environment is not found.
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped, adoption
FROM stacks WHERE env_id = ? AND id = ?;

-- name: ListStacksByEnv :many
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped, adoption
FROM stacks WHERE env_id = ? ORDER BY name;

-- name: GetStackByName :one
SELECT id, env_id, name, target, source_kind, source_ref, created_by, created_at, stopped, adoption
FROM stacks WHERE env_id = ? AND name = ?;

-- name: SetStackStopped :execrows
-- A person stopped the stack (1) or it was started or deployed again (0). The stack is looked up by its ID alone: the
-- caller read it by environment and ID first.
UPDATE stacks SET stopped = ? WHERE id = ?;

-- name: DeleteStack :execrows
-- Removing a stack deletes its definition: the snapshots, their files and variables, and its operations go with it
-- (ON DELETE CASCADE). The caller read the stack by environment and ID first.
DELETE FROM stacks WHERE id = ?;

-- name: LatestDeployLikeStackDeploy :one
-- The newest deploy, update or roll back of one stack, whatever came after it: a stop or a start does not hide a
-- failed deploy (UI-SPEC Defaults 11).
SELECT id, stack_id, env_id, kind, state, revision_seq, job_id, started_by, started_at, ended_at, failed_service, code
FROM stack_deploys WHERE stack_id = ? AND kind IN ('deploy', 'update', 'rollback') ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: LatestDeployLikeStackDeploysByEnv :many
-- The newest deploy, update or roll back of each stack of an environment, for the list.
SELECT d.id, d.stack_id, d.env_id, d.kind, d.state, d.revision_seq, d.job_id, d.started_by, d.started_at, d.ended_at, d.failed_service, d.code
FROM stack_deploys d
WHERE d.env_id = ? AND d.kind IN ('deploy', 'update', 'rollback')
  AND d.id = (SELECT d2.id FROM stack_deploys d2
              WHERE d2.stack_id = d.stack_id AND d2.kind IN ('deploy', 'update', 'rollback')
              ORDER BY d2.started_at DESC, d2.id DESC LIMIT 1);

-- name: InsertSnapshot :exec
INSERT INTO stack_snapshots (id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at, origin, restored_from)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetDraft :one
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at, origin, restored_from, platforms,
       git_commit, git_tree, git_source, compose_files, sync_trigger, reverted_drift
FROM stack_snapshots WHERE stack_id = ? AND kind = 'draft';

-- name: UpdateDraftVersion :execrows
-- The optimistic save: it matches only while the draft is still at the version the caller read.
UPDATE stack_snapshots SET version = version + 1, created_by = ?, created_at = ?
WHERE id = ? AND version = ?;

-- name: DeleteDraft :execrows
-- Discarding a draft: the stack's draft row only. Revisions are never deleted here.
DELETE FROM stack_snapshots WHERE id = ? AND kind = 'draft';

-- name: DeleteSnapshotFiles :exec
DELETE FROM stack_snapshot_files WHERE snapshot_id = ?;

-- name: InsertSnapshotFile :exec
INSERT INTO stack_snapshot_files (snapshot_id, path, content, content_sealed, secret, origin)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListSnapshotFiles :many
SELECT snapshot_id, path, content, content_sealed, secret, origin FROM stack_snapshot_files
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
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at, origin, restored_from, platforms,
       git_commit, git_tree, git_source, compose_files, sync_trigger, reverted_drift
FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND outcome = 'good'
ORDER BY seq DESC LIMIT 1;

-- name: LatestAttemptSeq :one
-- The number of the stack's newest revision whatever became of it (0 when it has none): the deploy the
-- stack stands at, good or not.
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) AS seq FROM stack_snapshots
WHERE stack_id = ? AND kind = 'revision';

-- name: GoodRevisionBefore :one
-- The newest good revision numbered below seq: what a roll back restores when the newest revision is
-- itself good.
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at, origin, restored_from, platforms,
       git_commit, git_tree, git_source, compose_files, sync_trigger, reverted_drift
FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND outcome = 'good' AND seq < ?
ORDER BY seq DESC LIMIT 1;

-- name: GetSnapshotPinsAndRendered :one
-- What a revision ran and rendered, for the copy a roll back takes of it.
SELECT rendered, pins FROM stack_snapshots WHERE id = ? AND kind = 'revision';

-- name: SetRevisionOutcome :execrows
-- An empty pins keeps the pins a deploy stored on the revision while it ran, and an empty rendered
-- the rendering the deploy stored when it started.
UPDATE stack_snapshots SET outcome = sqlc.arg(outcome),
    rendered = COALESCE(NULLIF(CAST(sqlc.arg(rendered) AS TEXT), ''), rendered),
    pins = COALESCE(NULLIF(CAST(sqlc.arg(pins) AS TEXT), ''), pins)
WHERE id = sqlc.arg(id) AND kind = 'revision';

-- name: SetSnapshotRendered :execrows
-- The masked rendering of a revision: what the next review compares the draft's rendering with.
UPDATE stack_snapshots SET rendered = ? WHERE id = ? AND kind = 'revision';

-- name: GetRevision :one
SELECT id, stack_id, kind, seq, base_seq, version, outcome, rendered, pins, created_by, created_at, origin, restored_from, platforms,
       git_commit, git_tree, git_source, compose_files, sync_trigger, reverted_drift
FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = ?;

-- name: SetDraftBaseSeq :execrows
-- A deploy of the draft makes the draft the content of the new revision, so the draft now starts from it.
UPDATE stack_snapshots SET base_seq = ? WHERE stack_id = ? AND kind = 'draft';

-- name: SetSnapshotPins :execrows
UPDATE stack_snapshots SET pins = ? WHERE id = ? AND kind = 'revision';

-- name: SetSnapshotComposeFiles :execrows
-- The ordered compose files of a snapshot, a JSON array of paths ('[]' for an inline stack).
UPDATE stack_snapshots SET compose_files = ? WHERE id = ?;

-- name: SetSnapshotGit :execrows
-- What a snapshot of a git stack was made from (D-08): the commit, the tree hash of the folder, the source as JSON
-- and what triggered the deploy of a revision ('' for a person).
UPDATE stack_snapshots SET git_commit = ?, git_tree = ?, git_source = ?, sync_trigger = ? WHERE id = ?;

-- name: SetSnapshotSyncTrigger :execrows
-- What started the deploy of a revision no person pressed ('poll' or 'webhook'), set once the revision is taken.
UPDATE stack_snapshots SET sync_trigger = ? WHERE id = ? AND kind = 'revision';

-- name: SetSnapshotRevertedDrift :execrows
-- The lines an automatic deploy put back to what the repository says, a JSON array of strings.
UPDATE stack_snapshots SET reverted_drift = ? WHERE id = ? AND kind = 'revision';

-- name: GetRevisionRevertedDrift :one
-- The lines an automatic deploy put back on one revision of a stack, a JSON array of strings.
SELECT reverted_drift FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision' AND seq = ?;

-- name: SetSnapshotPlatforms :execrows
-- The platforms each service of a revision was sent with, JSON by swarm service name.
UPDATE stack_snapshots SET platforms = ? WHERE id = ? AND kind = 'revision';

-- name: InsertStackDeploy :exec
INSERT INTO stack_deploys (id, stack_id, env_id, kind, state, revision_seq, job_id, started_by, started_at)
VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?);

-- name: FinishStackDeploy :execrows
UPDATE stack_deploys SET state = ?, ended_at = ?, failed_service = ?, code = ?
WHERE id = ? AND state = 'running';

-- name: MarkRunningRevisionsFailed :execrows
-- A deploy, an update or a roll back that was running when the server stopped did not finish: the
-- revision it took is failed. Only a revision still pending is touched. A roll back takes a revision of
-- its own (a copy of the one it restores), so it is failed the same way.
UPDATE stack_snapshots SET outcome = 'failed'
WHERE kind = 'revision' AND outcome = 'pending'
  AND EXISTS (SELECT 1 FROM stack_deploys d
              WHERE d.stack_id = stack_snapshots.stack_id AND d.revision_seq = stack_snapshots.seq
                AND d.state = 'running' AND d.kind IN ('deploy', 'update', 'rollback'));

-- name: MarkRunningDeploysInterrupted :execrows
-- Every operation still running at startup was running in a process that is gone: it ends interrupted.
UPDATE stack_deploys SET state = 'interrupted', ended_at = ?, code = 'interrupted' WHERE state = 'running';

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

-- name: LatestDoneStackDeployTriggersByEnv :many
-- What started the revision behind each stack's "last deployed" (the same operation as LatestDoneStackDeploysByEnv),
-- for the stacks whose newest successful operation was an automatic deploy: poll or webhook. A deploy a person pressed
-- has no row.
SELECT d.stack_id, r.sync_trigger
FROM stack_deploys d
JOIN stack_snapshots r ON r.stack_id = d.stack_id AND r.kind = 'revision' AND r.seq = d.revision_seq
WHERE d.env_id = ? AND d.state = 'done' AND d.kind IN ('deploy', 'update', 'rollback') AND r.sync_trigger != ''
  AND d.id = (SELECT d2.id FROM stack_deploys d2
              WHERE d2.stack_id = d.stack_id AND d2.state = 'done' AND d2.kind IN ('deploy', 'update', 'rollback')
              ORDER BY d2.started_at DESC, d2.id DESC LIMIT 1);

-- name: ListDraftsByEnv :many
-- The draft of each stack of an environment, for the list (its version and when it was saved).
SELECT dr.stack_id, dr.version, dr.created_by, dr.created_at FROM stack_snapshots dr
JOIN stacks s ON s.id = dr.stack_id
WHERE s.env_id = ? AND dr.kind = 'draft';

-- name: ListLatestRevisionTimesByEnv :many
-- When the newest revision of each stack of an environment was taken, not counting a roll back: it copies an earlier
-- revision and does not deploy the draft, so it must not make a draft saved before it read as deployed (D-03).
SELECT r.stack_id, CAST(MAX(r.created_at) AS INTEGER) AS taken_at FROM stack_snapshots r
JOIN stacks s ON s.id = r.stack_id
WHERE s.env_id = ? AND r.kind = 'revision' AND r.origin <> 'rollback'
GROUP BY r.stack_id;

-- name: ListRevisions :many
-- The revisions of one stack, newest first, with the failure each one's deploy ended in. Two operations can name one
-- seq (a retry of the same revision), so the newest deploy row is the one read.
SELECT r.seq, r.origin, r.restored_from, r.outcome, r.pins, r.created_by, r.created_at,
       r.git_commit, r.git_tree, r.git_source, r.sync_trigger,
       COALESCE(d.failed_service, '') AS failed_service, COALESCE(d.code, '') AS code
FROM stack_snapshots r
LEFT JOIN stack_deploys d ON d.id = (
    SELECT d2.id FROM stack_deploys d2
    WHERE d2.stack_id = r.stack_id AND d2.revision_seq = r.seq AND d2.kind IN ('deploy', 'update', 'rollback')
    ORDER BY d2.started_at DESC, d2.id DESC LIMIT 1)
WHERE r.stack_id = ? AND r.kind = 'revision'
ORDER BY r.seq DESC
LIMIT ?;

-- name: PruneRevisions :execrows
-- Retention (D-01): a stack keeps its newest keep revisions by seq, its newest good revision (the deployed
-- one) however old, and any revision still pending. The rest are deleted; the foreign keys cascade to their
-- files and variables, so a sealed value goes with its revision. A draft is never touched.
DELETE FROM stack_snapshots
WHERE stack_snapshots.stack_id = sqlc.arg(stack_id) AND stack_snapshots.kind = 'revision' AND stack_snapshots.outcome <> 'pending'
  AND stack_snapshots.seq NOT IN (SELECT k.seq FROM stack_snapshots k
                  WHERE k.stack_id = stack_snapshots.stack_id AND k.kind = 'revision'
                  ORDER BY k.seq DESC LIMIT sqlc.arg(keep))
  AND stack_snapshots.seq <> (SELECT CAST(COALESCE(MAX(g.seq), 0) AS INTEGER) FROM stack_snapshots g
              WHERE g.stack_id = stack_snapshots.stack_id AND g.kind = 'revision' AND g.outcome = 'good');

-- name: CountPrunableRevisions :one
-- How many revisions across how many stacks PruneRevisions would delete for keep, over every stack: the
-- same predicate, read only.
SELECT CAST(COUNT(*) AS INTEGER) AS revisions, CAST(COUNT(DISTINCT s.stack_id) AS INTEGER) AS stacks
FROM stack_snapshots s
WHERE s.kind = 'revision' AND s.outcome <> 'pending'
  AND s.seq NOT IN (SELECT k.seq FROM stack_snapshots k
                    WHERE k.stack_id = s.stack_id AND k.kind = 'revision'
                    ORDER BY k.seq DESC LIMIT sqlc.arg(keep))
  AND s.seq <> (SELECT CAST(COALESCE(MAX(g.seq), 0) AS INTEGER) FROM stack_snapshots g
                WHERE g.stack_id = s.stack_id AND g.kind = 'revision' AND g.outcome = 'good');

-- name: ListStackIDs :many
SELECT id FROM stacks ORDER BY id;
