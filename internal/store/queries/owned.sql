/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

-- name: ListOwnedDetailByOwner :many
-- Every automation an owner holds, with the name of its environment (empty when
-- the environment no longer exists), for the owner's page.
SELECT o.kind, o.resource_id, o.env_id, o.name, o.state, o.needs_env_admin,
       COALESCE(e.name, '') AS env_name
FROM owned_resources o
LEFT JOIN environments e ON e.id = o.env_id
WHERE o.owner_id = ?
ORDER BY o.kind, o.resource_id;

-- name: GetOwnedDetail :one
-- One automation as ListOwnedDetailByOwner gives it, with its owner.
SELECT o.kind, o.resource_id, o.env_id, o.name, o.owner_id, o.state, o.needs_env_admin,
       COALESCE(e.name, '') AS env_name
FROM owned_resources o
LEFT JOIN environments e ON e.id = o.env_id
WHERE o.kind = ? AND o.resource_id = ?;

-- name: ListActiveUsersForEligibility :many
-- The users an automation could be handed to before their rights are looked at:
-- not deleted and not disabled (D-14).
SELECT id, username, display_name FROM users
WHERE deleted_at IS NULL AND disabled = 0
ORDER BY username;

-- name: TransferOwned :exec
-- Hands an automation to another owner. Only the access service calls it, after
-- checking the new owner could run it (D-14).
UPDATE owned_resources SET owner_id = ?, updated_at = ? WHERE kind = ? AND resource_id = ?;

-- name: DeleteOwnedResource :execrows
-- Forgets an automation whose resource is gone or was switched off by a person: removing a stack's webhook deregisters
-- it in the same transaction. Only the access service calls it.
DELETE FROM owned_resources WHERE kind = ? AND resource_id = ?;

-- name: DeleteOwnedResourcesOfStack :execrows
-- Forgets the automations of a stack that was removed (its webhook and its auto-deploy), in the same transaction as the
-- removal, so no row is left for a resource that is gone. The two kinds are passed in because the names live in gitsync.
DELETE FROM owned_resources WHERE resource_id = sqlc.arg(resource_id) AND kind IN (sqlc.arg(kind_a), sqlc.arg(kind_b));
