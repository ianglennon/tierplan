/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertSession :exec
INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, absolute_expires_at, remember, ip, user_agent)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetSessionByHash :one
SELECT * FROM sessions WHERE id_hash = ?;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = ?;

-- name: DeleteUserSessionsExcept :exec
-- Ends every session of a user except the one making the request, for a
-- password change or a privilege change (D-32).
DELETE FROM sessions WHERE user_id = ? AND id_hash != ?;

-- name: DeleteExpiredSessions :execrows
-- Browser-session cookies expire by idle time, remembered ones by their
-- absolute expiry (D-31).
DELETE FROM sessions
WHERE (remember = 0 AND last_seen_at < sqlc.arg(idle_cutoff))
   OR (absolute_expires_at IS NOT NULL AND absolute_expires_at < CAST(sqlc.arg(now) AS INTEGER));
