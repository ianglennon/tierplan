/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: InsertSession :exec
INSERT INTO sessions (id_hash, user_id, created_at, last_seen_at, absolute_expires_at, remember, ip, user_agent, public_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

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

-- name: ListUserSessions :many
-- A user's sessions, most recently active first, for the session list. public_id
-- is the handle the list names a session by; id_hash only lets the caller tell
-- which row is their own and never leaves the server (T-03-59).
SELECT CAST(COALESCE(public_id, '') AS TEXT) AS public_id, id_hash, created_at, last_seen_at, ip, user_agent
FROM sessions
WHERE user_id = ?
ORDER BY last_seen_at DESC, created_at DESC, id_hash;

-- name: GetUserSessionByPublicID :one
-- Scoped by user, so another user's handle is no row at all (T-03-60).
SELECT id_hash, ip, user_agent FROM sessions WHERE user_id = ? AND public_id = ?;

-- name: DeleteUserSessionByPublicID :execrows
DELETE FROM sessions WHERE user_id = ? AND public_id = ?;

-- name: DeleteUserSessionsExceptHash :execrows
-- Ends every other session of a user and says how many ended, for "sign out
-- everywhere else".
DELETE FROM sessions WHERE user_id = ? AND id_hash != ?;
