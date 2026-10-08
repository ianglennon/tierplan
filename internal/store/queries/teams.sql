/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors

   Queries of teams and their membership (internal/access/teams.go). A team's
   grants are role_assignments rows with principal_type 'team'; they have no
   foreign key to teams, so deleting a team deletes them explicitly. */

-- name: InsertTeam :exec
INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, ?, ?);

-- name: ListTeamsWithCounts :many
-- Every team with how many members it has and how many grants (environment
-- assignments) it holds, for the Teams table.
SELECT t.id, t.name, t.created_at, t.updated_at,
       (SELECT COUNT(*) FROM team_members m WHERE m.team_id = t.id) AS member_count,
       (SELECT COUNT(*) FROM role_assignments ra WHERE ra.principal_type = 'team' AND ra.principal_id = t.id) AS grant_count
FROM teams t
ORDER BY lower(t.name), t.id;

-- name: GetTeam :one
SELECT * FROM teams WHERE id = ?;

-- name: GetTeamWithCounts :one
SELECT t.id, t.name, t.created_at, t.updated_at,
       (SELECT COUNT(*) FROM team_members m WHERE m.team_id = t.id) AS member_count,
       (SELECT COUNT(*) FROM role_assignments ra WHERE ra.principal_type = 'team' AND ra.principal_id = t.id) AS grant_count
FROM teams t
WHERE t.id = ?;

-- name: GetTeamByNameCI :one
-- A team by name, ignoring case (the unique index is on lower(name)).
SELECT * FROM teams WHERE lower(name) = lower(?);

-- name: RenameTeam :execrows
-- Renames a team only if it is still at the version the caller read
-- (updated_at). Zero rows means the version moved.
UPDATE teams SET name = ?, updated_at = ? WHERE id = ? AND updated_at = ?;

-- name: DeleteTeam :execrows
-- Its memberships go with it (ON DELETE CASCADE).
DELETE FROM teams WHERE id = ?;

-- name: DeleteTeamAssignments :execrows
-- The team's own grants; their extras go with them (ON DELETE CASCADE). There is
-- no foreign key from principal_id, so this must run with every team delete.
DELETE FROM role_assignments WHERE principal_type = 'team' AND principal_id = ?;

-- name: ListTeamMembers :many
-- The members of a team with what the Members card shows, sorted by username.
-- The status is worked out in Go (access.Status) from the lock, the password and
-- the invite link, like the users table.
SELECT u.id, u.username, u.display_name, u.instance_admin, u.disabled, u.deleted_at,
       u.locked_until, u.password_hash, l.expires_at AS link_expires_at, m.created_at AS added_at
FROM team_members m
JOIN users u ON u.id = m.user_id
LEFT JOIN user_links l ON l.user_id = u.id
WHERE m.team_id = ?
ORDER BY u.username;

-- name: InsertTeamMember :execrows
-- Adds a member; adding one who is already in the team changes nothing, and the
-- row count says which happened.
INSERT INTO team_members (team_id, user_id, created_at) VALUES (?, ?, ?)
ON CONFLICT (team_id, user_id) DO NOTHING;

-- name: DeleteTeamMember :execrows
DELETE FROM team_members WHERE team_id = ? AND user_id = ?;

-- name: ListTeamMemberIDs :many
SELECT user_id FROM team_members WHERE team_id = ? ORDER BY user_id;
