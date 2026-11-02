/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors

   Queries of grants: the role_assignments rows that give a user or a team a role
   on one environment (or on all of them) and the assignment_extras that ride on
   them (internal/access/grants.go). A grant has no expiry (D-21). InsertAssignmentExtra
   lives in rbac.sql; env_id NULL means every environment. */

-- name: GetAssignmentForSubjectScope :one
-- The one assignment a subject holds on a scope. scope is the environment ID, or
-- the empty string for every environment (the unique index is on COALESCE(env_id, '')).
SELECT * FROM role_assignments
WHERE principal_type = sqlc.arg(principal_type)
  AND principal_id = sqlc.arg(principal_id)
  AND COALESCE(env_id, '') = CAST(sqlc.arg(scope) AS TEXT);

-- name: InsertAssignment :exec
INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: UpdateAssignmentRole :exec
UPDATE role_assignments SET role_id = ? WHERE id = ?;

-- name: DeleteAssignmentExtras :exec
DELETE FROM assignment_extras WHERE assignment_id = ?;

-- name: GetAssignment :one
SELECT * FROM role_assignments WHERE id = ?;

-- name: ListAssignmentExtras :many
SELECT permission FROM assignment_extras WHERE assignment_id = ? ORDER BY permission;

-- name: DeleteAssignment :execrows
-- Its extras go with it (ON DELETE CASCADE).
DELETE FROM role_assignments WHERE id = ?;

-- name: ListSubjectAssignments :many
-- Every assignment made to one user or team, with the dangerous extras it
-- carries as a JSON array and the name of its environment (empty for every
-- environment). Sorted so "all environments" comes first, then by environment name.
SELECT ra.id AS id,
       ra.principal_type AS principal_type,
       ra.principal_id AS principal_id,
       ra.role_id AS role_id,
       ra.env_id AS env_id,
       ra.created_at AS created_at,
       CAST(COALESCE(e.name, '') AS TEXT) AS env_name,
       CAST(COALESCE((SELECT json_group_array(ae.permission) FROM (SELECT permission FROM assignment_extras WHERE assignment_id = ra.id ORDER BY permission) ae), '[]') AS TEXT) AS extras
FROM role_assignments ra
LEFT JOIN environments e ON e.id = ra.env_id
WHERE ra.principal_type = ? AND ra.principal_id = ?
ORDER BY (ra.env_id IS NOT NULL), lower(COALESCE(e.name, '')), ra.id;

-- name: DeleteEnvironmentAssignments :execrows
-- Every assignment on one environment, whoever holds it; the extras go with
-- them. An environment that is removed or folded away takes its grants with it,
-- so none outlives it (the environment ID is never reused).
DELETE FROM role_assignments WHERE env_id = ?;

-- name: DeleteEnvironmentOwnedResources :execrows
-- The automations registered on one environment, for the same reason.
DELETE FROM owned_resources WHERE env_id = ?;

-- name: ListEnvAccessRows :many
-- Every assignment that reaches one environment: the ones on it and the ones on
-- every environment (env_id NULL), whoever holds them. A team assignment comes
-- once per member (member_id is empty for a team with no members), so the people
-- who hold it through the team can be folded in Go with the same merge rules the
-- evaluator uses. subject_name and subject_display are the user's username and
-- display name, or the team's name. member_active is 1 for a member who can sign
-- in (not disabled, not deleted); a deleted member is not listed at all.
SELECT ra.id AS id,
       ra.principal_type AS principal_type,
       ra.principal_id AS principal_id,
       ra.role_id AS role_id,
       ra.env_id AS env_id,
       CAST(COALESCE((SELECT json_group_array(ae.permission) FROM (SELECT permission FROM assignment_extras WHERE assignment_id = ra.id ORDER BY permission) ae), '[]') AS TEXT) AS extras,
       CAST(COALESCE(CASE ra.principal_type WHEN 'user' THEN u.username ELSE t.name END, '') AS TEXT) AS subject_name,
       CAST(COALESCE(u.display_name, '') AS TEXT) AS subject_display,
       CAST(CASE WHEN u.id IS NOT NULL AND u.disabled = 0 AND u.deleted_at IS NULL THEN 1 ELSE 0 END AS INTEGER) AS subject_active,
       CAST(COALESCE(m.id, '') AS TEXT) AS member_id,
       CAST(COALESCE(m.username, '') AS TEXT) AS member_username,
       CAST(COALESCE(m.display_name, '') AS TEXT) AS member_display,
       CAST(CASE WHEN m.id IS NOT NULL AND m.disabled = 0 THEN 1 ELSE 0 END AS INTEGER) AS member_active
FROM role_assignments ra
LEFT JOIN users u ON ra.principal_type = 'user' AND u.id = ra.principal_id
LEFT JOIN teams t ON ra.principal_type = 'team' AND t.id = ra.principal_id
LEFT JOIN team_members tm ON ra.principal_type = 'team' AND tm.team_id = ra.principal_id
LEFT JOIN users m ON m.id = tm.user_id AND m.deleted_at IS NULL
WHERE ra.env_id = sqlc.arg(env_id) OR ra.env_id IS NULL
ORDER BY ra.created_at, ra.id, m.username;

-- name: ListEnvAccessInstanceAdmins :many
-- The instance admins who can sign in: each holds Admin on every environment.
SELECT id, username, display_name FROM users
WHERE instance_admin = 1 AND disabled = 0 AND deleted_at IS NULL
ORDER BY username;

-- name: ListAccessCandidates :many
-- The users and teams an environment's Access tab can give access to: the users
-- who can sign in and are not instance admins (who hold every environment
-- already), and every team, each without a direct assignment on the environment.
-- kind is user or team; display_name is empty for a team.
SELECT CAST('user' AS TEXT) AS kind, u.id AS id, u.username AS name, u.display_name AS display_name
FROM users u
WHERE u.deleted_at IS NULL AND u.disabled = 0 AND u.instance_admin = 0
  AND NOT EXISTS (SELECT 1 FROM role_assignments ra
                  WHERE ra.principal_type = 'user' AND ra.principal_id = u.id AND ra.env_id = sqlc.arg(env_id))
UNION ALL
SELECT CAST('team' AS TEXT) AS kind, t.id AS id, t.name AS name, CAST('' AS TEXT) AS display_name
FROM teams t
WHERE NOT EXISTS (SELECT 1 FROM role_assignments ra
                  WHERE ra.principal_type = 'team' AND ra.principal_id = t.id AND ra.env_id = sqlc.arg(env_id))
ORDER BY kind, name COLLATE NOCASE, id;
