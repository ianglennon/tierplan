/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: UpsertRecentEnvironment :exec
-- A repeat keeps its one row and the newer of the two times, so a device whose
-- clock runs behind cannot move a use backwards.
INSERT INTO user_recent_environments (user_id, env_id, used_at)
VALUES (?, ?, ?)
ON CONFLICT (user_id, env_id) DO UPDATE SET used_at = MAX(used_at, excluded.used_at);

-- name: TrimRecentEnvironments :exec
-- Keeps the newest 3 of a user's rows, in the order ListRecentEnvironments gives.
DELETE FROM user_recent_environments
WHERE user_recent_environments.user_id = ?1
  AND user_recent_environments.env_id NOT IN (
    SELECT keep.env_id FROM user_recent_environments AS keep
    WHERE keep.user_id = ?1
    ORDER BY keep.used_at DESC, keep.env_id
    LIMIT 3
  );

-- name: ListRecentEnvironments :many
SELECT env_id FROM user_recent_environments
WHERE user_id = ?
ORDER BY used_at DESC, env_id;
