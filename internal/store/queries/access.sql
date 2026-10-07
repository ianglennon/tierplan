/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors

   Queries of the access service (internal/access): users, one-time links, teams
   and grants. Each Phase 3 access plan adds its own section below. */

-- name: InsertInvitedUser :exec
-- An invited user has no password yet: the empty hash can never verify, and the
-- sign-in refuses it as invite_pending. password_changed_at is the creation time.
INSERT INTO users (id, username, password_hash, display_name, email, created_at, updated_at, password_changed_at)
VALUES (?, ?, '', ?, ?, ?, ?, ?);

-- name: GetUserByUsernameAny :one
-- Finds a username whether the user is live or a tombstone (deleted_at set). A
-- deleted user's name stays reserved (D-22c).
SELECT * FROM users WHERE username = ?;

-- name: DeleteUserLinks :exec
-- A user holds one live link at a time: a new one replaces the old.
DELETE FROM user_links WHERE user_id = ?;

-- name: InsertUserLink :exec
INSERT INTO user_links (id, user_id, kind, token_hash, created_at, expires_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetUserLinkByHash :one
-- A link that has not expired, with the state of the user it belongs to. The
-- caller refuses a disabled or deleted user with the same answer as an unknown
-- link.
SELECT l.user_id, l.kind, l.expires_at, u.username, u.disabled, u.deleted_at
FROM user_links l
JOIN users u ON u.id = l.user_id
WHERE l.token_hash = ? AND l.expires_at > ?;

-- name: ConsumeUserLink :one
-- Spends a link. The delete and the read of who it was for are one statement, so
-- of any number of concurrent accepts exactly one gets a row back.
DELETE FROM user_links WHERE token_hash = ? AND expires_at > ? RETURNING user_id, kind;
