/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: UpsertShellPref :exec
-- A repeat keeps its one row: the latest open of an image is the choice remembered.
INSERT INTO user_shell_prefs (user_id, image, shell, command, run_as, working_dir, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, image) DO UPDATE SET
    shell       = excluded.shell,
    command     = excluded.command,
    run_as      = excluded.run_as,
    working_dir = excluded.working_dir,
    updated_at  = excluded.updated_at;

-- name: GetShellPref :one
SELECT shell, command, run_as, working_dir FROM user_shell_prefs
WHERE user_id = ? AND image = ?;

-- name: TrimShellPrefs :exec
-- Keeps the newest 200 of a user's rows.
DELETE FROM user_shell_prefs
WHERE user_shell_prefs.user_id = ?1
  AND user_shell_prefs.image NOT IN (
    SELECT keep.image FROM user_shell_prefs AS keep
    WHERE keep.user_id = ?1
    ORDER BY keep.updated_at DESC, keep.image
    LIMIT 200
  );
