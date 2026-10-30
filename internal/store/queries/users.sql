/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: GetUserByUsername :one
-- Usernames are stored lowercase; callers lower-case before looking up (D-35).
SELECT * FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: InsertUser :exec
INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: RecordLoginFailure :one
-- Counts one wrong password and returns the new count, in one statement so
-- concurrent attempts cannot both read the same count (D-29).
UPDATE users SET failed_count = failed_count + 1 WHERE id = ? RETURNING failed_count;

-- name: LockUser :exec
UPDATE users SET locked_until = ? WHERE id = ?;

-- name: ResetLoginFailures :exec
-- A successful sign-in, an expired lock or an operator unlock all clear the
-- count and the lock together.
UPDATE users SET failed_count = 0, locked_until = NULL WHERE id = ?;

-- name: UnlockUserByUsername :execrows
UPDATE users SET failed_count = 0, locked_until = NULL WHERE username = ?;

-- name: UpdatePassword :exec
-- Stores a new password hash and stamps when it changed (D-34).
UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?;

-- name: SetUserInstanceAdmin :execrows
UPDATE users SET instance_admin = ?, updated_at = ? WHERE id = ?;

-- name: ListActiveInstanceAdmins :many
-- The usernames of the instance admins who can still sign in: not disabled and
-- not tombstoned. A signed-in user with no access is told whom to ask (D-08).
SELECT username FROM users
WHERE instance_admin = 1 AND disabled = 0 AND deleted_at IS NULL
ORDER BY username;

-- name: SetUserLastLogin :exec
-- Stamps a successful sign-in (UTC unix nanoseconds).
UPDATE users SET last_login_at = ? WHERE id = ?;
