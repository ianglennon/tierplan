/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors

   Queries of the owned-resources registry (internal/access/owned.go): automations
   that act on a user's authority and pause when the user no longer qualifies
   (D-14). Only the access service writes these rows. */

-- name: InsertOwnedResource :exec
INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, state, needs_env_admin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetOwnedResource :one
SELECT * FROM owned_resources WHERE kind = ? AND resource_id = ?;

-- name: ListOwnedByOwner :many
SELECT * FROM owned_resources WHERE owner_id = ? ORDER BY kind, resource_id;

-- name: ListOwnedByOwners :many
-- owners is a JSON array of user IDs, not sqlc.slice, for the reason ListAuditPage
-- gives: a slice placeholder collides with numbered ones.
SELECT * FROM owned_resources
WHERE owner_id IN (SELECT value FROM json_each(CAST(sqlc.arg(owners) AS TEXT)))
ORDER BY owner_id, kind, resource_id;

-- name: UpdateOwnedState :exec
UPDATE owned_resources SET state = ?, updated_at = ? WHERE kind = ? AND resource_id = ?;

-- name: SetOwnedNeedsEnvAdmin :exec
-- Recorded by the stack phases when the last run used host-escalating keys, so
-- the recheck demands env Admin from the owner (D-13, D-14).
UPDATE owned_resources SET needs_env_admin = ?, updated_at = ? WHERE kind = ? AND resource_id = ?;
