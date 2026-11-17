/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

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
UPDATE environments SET name = ?, name_auto = 0, updated_at = ? WHERE id = ?;

-- name: DeleteEnvironment :execrows
DELETE FROM environments WHERE id = ?;

-- name: SetEnvironmentAutoUpdate :execrows
UPDATE environments SET auto_update = ?, updated_at = ? WHERE id = ?;

-- name: FoldLocalHost :execrows
UPDATE environments
SET local_node_id = ?, local_socket_path = ?, updated_at = ?
WHERE id = ? AND kind = 'agent' AND env_type = 'swarm' AND swarm_cluster_id = ? AND local_node_id IS NULL
  AND enabled = 1 AND cluster_changed_at IS NULL;

-- name: CountFoldedEnvironments :one
SELECT COUNT(*) FROM environments WHERE kind = 'agent' AND local_node_id IS NOT NULL;

-- name: CountFoldRemovalsByEnv :one
-- What a fold of the Local environment removes through the cascade, for the card that offers it (D-16):
-- its stacks, Git repositories and registry logins, its own Git credentials, image-update ignores, janitor
-- keeps and run history (the janitor's and the image updates' runs together), and the janitor settings
-- (the environment's policy row or any node's) as a count that is non-zero when there are any.
SELECT
    (SELECT COUNT(*) FROM stacks AS s WHERE s.env_id = ?1) AS stacks,
    (SELECT COUNT(*) FROM git_repos AS g WHERE g.env_id = ?1) AS git_repos,
    (SELECT COUNT(*) FROM registry_credentials AS c WHERE c.env_id = ?1) AS registries,
    (SELECT COUNT(*) FROM git_credentials AS gc WHERE gc.env_id = ?1) AS git_credentials,
    (SELECT COUNT(*) FROM image_update_ignores AS iu WHERE iu.env_id = ?1) AS image_update_ignores,
    (SELECT COUNT(*) FROM janitor_keeps AS jk WHERE jk.env_id = ?1) AS janitor_keeps,
    (SELECT COUNT(*) FROM janitor_runs AS jr WHERE jr.env_id = ?1)
        + (SELECT COUNT(*) FROM image_update_runs AS ir WHERE ir.env_id = ?1) AS runs,
    (SELECT COUNT(*) FROM janitor_env_policy AS je WHERE je.env_id = ?1)
        + (SELECT COUNT(*) FROM janitor_node_policy AS jn WHERE jn.env_id = ?1) AS janitor;
