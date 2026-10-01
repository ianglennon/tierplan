/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: UpsertRole :exec
INSERT INTO roles (id, name, builtin) VALUES (?, ?, ?)
ON CONFLICT (id) DO UPDATE SET name = excluded.name, builtin = excluded.builtin;

-- name: ListRolePermissions :many
SELECT permission FROM role_permissions WHERE role_id = ? ORDER BY permission;

-- name: InsertRolePermission :exec
INSERT INTO role_permissions (role_id, permission) VALUES (?, ?)
ON CONFLICT (role_id, permission) DO NOTHING;

-- name: DeleteRolePermission :exec
DELETE FROM role_permissions WHERE role_id = ? AND permission = ?;

-- name: InsertRoleAssignment :exec
INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: PrincipalHasPermission :one
-- An assignment with a NULL env_id applies everywhere. One tied to an
-- environment applies only when the caller passes that environment ID; a global
-- permission check passes the empty string, which no assignment matches.
SELECT EXISTS (
    SELECT 1
    FROM role_assignments ra
    JOIN role_permissions rp ON rp.role_id = ra.role_id
    WHERE ra.principal_type = sqlc.arg(principal_type)
      AND ra.principal_id = sqlc.arg(principal_id)
      AND rp.permission = sqlc.arg(permission)
      AND (ra.env_id IS NULL OR (CAST(sqlc.arg(env_id) AS TEXT) <> '' AND ra.env_id = CAST(sqlc.arg(env_id) AS TEXT)))
) AS allowed;

-- name: ListPrincipalPermissions :many
SELECT DISTINCT ra.env_id AS env_id, rp.permission AS permission
FROM role_assignments ra
JOIN role_permissions rp ON rp.role_id = ra.role_id
WHERE ra.principal_type = ? AND ra.principal_id = ?
ORDER BY ra.env_id, rp.permission;
