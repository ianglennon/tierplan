/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

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

-- name: GetUserGlobalRole :one
-- The role a user holds across the whole installation (an assignment with no
-- environment). Phase 1 has one such role per user.
SELECT role_id FROM role_assignments
WHERE principal_type = 'user' AND principal_id = ? AND env_id IS NULL
ORDER BY created_at, id
LIMIT 1;

-- name: UpdatePassword :exec
-- Stores a new password hash and stamps when it changed (D-34).
UPDATE users SET password_hash = ?, password_changed_at = ?, updated_at = ? WHERE id = ?;
