/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: UpsertImageUpdateFailure :exec
-- Remembers the digest an update failed with for a workload. A workload has one failure: a newer one
-- replaces the older.
INSERT INTO image_update_failures (env_id, kind, workload, digest, reason, run_id, rollback_failed, failed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, kind, workload) DO UPDATE SET
    digest = excluded.digest,
    reason = excluded.reason,
    run_id = excluded.run_id,
    rollback_failed = excluded.rollback_failed,
    failed_at = excluded.failed_at;

-- name: DeleteImageUpdateFailure :execrows
-- Forgets the failure of a workload, after a later good update. The count is 0 when there was none.
DELETE FROM image_update_failures
WHERE env_id = ? AND kind = ? AND workload = ?;

-- name: ListImageUpdateFailures :many
-- The remembered failures of one environment.
SELECT env_id, kind, workload, digest, reason, run_id, rollback_failed, failed_at
FROM image_update_failures
WHERE env_id = ?
ORDER BY kind, workload;
