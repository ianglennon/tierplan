/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors

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
