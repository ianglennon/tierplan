/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: InsertRoleAssignment :exec
INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: InsertAssignmentExtra :exec
INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, ?)
ON CONFLICT (assignment_id, permission) DO NOTHING;

-- name: GetUserAccessFlags :one
-- What decides whether a user may act at all, and as whom: the instance flag,
-- the disabled switch and the tombstone.
SELECT instance_admin, disabled, deleted_at FROM users WHERE id = ?;

-- name: ListUserAssignments :many
-- Every assignment that reaches a user: the ones made to the user and the ones
-- made to a team the user belongs to. A NULL env_id applies to every
-- environment. extras is a JSON array of the dangerous permissions the
-- assignment carries; team_name is empty for a direct assignment.
SELECT ra.id AS id,
       ra.principal_type AS principal_type,
       ra.principal_id AS principal_id,
       ra.role_id AS role_id,
       ra.env_id AS env_id,
       CAST(COALESCE((SELECT json_group_array(ae.permission) FROM assignment_extras ae WHERE ae.assignment_id = ra.id), '[]') AS TEXT) AS extras,
       CAST(COALESCE(t.name, '') AS TEXT) AS team_name
FROM role_assignments ra
LEFT JOIN teams t ON ra.principal_type = 'team' AND t.id = ra.principal_id
WHERE (ra.principal_type = 'user' AND ra.principal_id = sqlc.arg(user_id))
   OR (ra.principal_type = 'team' AND ra.principal_id IN (SELECT tm.team_id FROM team_members tm WHERE tm.user_id = sqlc.arg(user_id)))
ORDER BY ra.created_at, ra.id;
