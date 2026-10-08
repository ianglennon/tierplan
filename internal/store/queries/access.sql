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

-- name: ListUsersForAdmin :many
-- Every user who is not deleted, for the admin's users table, with the expiry of
-- the invite link they hold (NULL when none) and how many teams they belong to.
-- The status is worked out in Go so the precedence lives in one place.
SELECT u.id, u.username, u.display_name, u.email, u.instance_admin, u.disabled,
       u.locked_until, u.password_hash, u.last_login_at, l.expires_at AS link_expires_at,
       (SELECT COUNT(*) FROM team_members m WHERE m.user_id = u.id) AS team_count
FROM users u
LEFT JOIN user_links l ON l.user_id = u.id
WHERE u.deleted_at IS NULL
ORDER BY u.username;

-- name: GetUserDetail :one
-- One user for the user page, with the expiry of the invite link they hold
-- (NULL when none) and how many teams they belong to. A tombstone is returned
-- too; the caller answers not found for it.
SELECT u.id, u.username, u.display_name, u.email, u.instance_admin, u.disabled,
       u.locked_until, u.password_hash, u.last_login_at, u.created_at, u.updated_at,
       u.deleted_at, l.expires_at AS link_expires_at,
       (SELECT COUNT(*) FROM team_members m WHERE m.user_id = u.id) AS team_count
FROM users u
LEFT JOIN user_links l ON l.user_id = u.id
WHERE u.id = ?;

-- name: SetUserDisabled :exec
UPDATE users SET disabled = ?, updated_at = ? WHERE id = ?;

-- name: CountActiveInstanceAdmins :one
-- How many instance admins can still sign in. Counted after a change inside the
-- same write transaction, it is what keeps an installation from locking itself
-- out (D-02).
SELECT COUNT(*) FROM users WHERE instance_admin = 1 AND disabled = 0 AND deleted_at IS NULL;

-- name: DeleteAllUserSessions :execrows
-- Ends every session of a user and says how many there were.
DELETE FROM sessions WHERE user_id = ?;

-- name: TombstoneUser :execrows
-- Deletes a user without deleting the row (D-22c): what lets them in is erased,
-- and the row, the username and every audit reference to the ID stay. The
-- username is never touched, so it stays reserved. Zero rows means the user was
-- already a tombstone.
UPDATE users
SET disabled = 1, deleted_at = ?, password_hash = '', email = '', display_name = '',
    instance_admin = 0, failed_count = 0, locked_until = NULL, updated_at = ?
WHERE id = ? AND deleted_at IS NULL;

-- name: DeleteUserAssignments :execrows
-- Every role assignment made to the user themselves; their extras go with them.
-- Assignments made to a team are the team's.
DELETE FROM role_assignments WHERE principal_type = 'user' AND principal_id = ?;

-- name: DeleteUserMemberships :execrows
DELETE FROM team_members WHERE user_id = ?;

-- name: UpdateUserProfile :execrows
-- Saves the display name and email only if the user is still at the version the
-- caller read (updated_at), so two admins never silently overwrite each other
-- (T-03-57). Zero rows means the version moved.
UPDATE users SET display_name = ?, email = ?, updated_at = ?
WHERE id = ? AND updated_at = ? AND deleted_at IS NULL;
