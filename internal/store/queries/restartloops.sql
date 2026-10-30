/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: GetRestartLoopSettings :one
-- The threshold and window restart-loop detection uses for one environment.
SELECT restart_loop_failures, restart_loop_window_minutes FROM environments WHERE id = ?;

-- name: SetRestartLoopSettings :execrows
UPDATE environments
SET restart_loop_failures = ?, restart_loop_window_minutes = ?, updated_at = ?
WHERE id = ?;

-- name: ListRestartLoopMutes :many
SELECT env_id, kind, target_id, name, muted_by, muted_at FROM restart_loop_mutes
WHERE env_id = ?
ORDER BY muted_at, kind, target_id;

-- name: InsertRestartLoopMute :exec
-- A second mute of the same target keeps the first (who muted it and when).
INSERT INTO restart_loop_mutes (env_id, kind, target_id, name, muted_by, muted_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, kind, target_id) DO NOTHING;

-- name: DeleteRestartLoopMute :execrows
DELETE FROM restart_loop_mutes WHERE env_id = ? AND kind = ? AND target_id = ?;
