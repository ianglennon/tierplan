/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: ListImageUpdateIgnores :many
-- Every skip and ignore of one environment, oldest first.
SELECT id, env_id, mode, scope, host, repo, tag, kind, name, digest, set_by, set_at
FROM image_update_ignores WHERE env_id = ? ORDER BY set_at, id;

-- name: InsertImageUpdateIgnore :exec
-- Saves a skip or an ignore. Setting the same one again (the same mode and scope) replaces who set it, when
-- and the digest a skip passes over, so skipping a newer version of a workload replaces the earlier skip.
INSERT INTO image_update_ignores (id, env_id, mode, scope, host, repo, tag, kind, name, digest, set_by, set_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, mode, scope, host, repo, tag, kind, name)
DO UPDATE SET digest = excluded.digest, set_by = excluded.set_by, set_at = excluded.set_at;

-- name: DeleteImageUpdateIgnore :execrows
-- Removes one skip or ignore of an environment by what names it; zero rows means there was none.
DELETE FROM image_update_ignores
WHERE env_id = ? AND mode = ? AND scope = ? AND host = ? AND repo = ? AND tag = ? AND kind = ? AND name = ?;
