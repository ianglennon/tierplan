/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: ListEnvironments :many
SELECT * FROM environments ORDER BY created_at, id;

-- name: GetEnvironment :one
SELECT * FROM environments WHERE id = ?;

-- name: CountEnvironmentsOfKind :one
SELECT COUNT(*) FROM environments WHERE kind = ?;

-- name: InsertEnvironment :exec
INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, 1, ?, ?);

-- name: RenameEnvironment :execrows
UPDATE environments SET name = ?, updated_at = ? WHERE id = ?;

-- name: DeleteEnvironment :execrows
DELETE FROM environments WHERE id = ?;
