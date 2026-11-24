/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- The virtual IP (Phase 26): the configuration, the sealed key and the bounded history of moves.
-- key_sealed is vault.Seal output; none of these statements reads it back in clear.

-- name: PutVIP :exec
-- Creates the environment's virtual IP row or replaces its configuration. created_at stands on a
-- replace; the ensure state is kept.
INSERT INTO ingress_vip (env_id, address, prefix_len, router_id, router_derived, key_sealed, key_generation, enabled, created_at, updated_at)
VALUES (sqlc.arg(env_id), sqlc.arg(address), sqlc.arg(prefix_len), sqlc.arg(router_id), sqlc.arg(router_derived), sqlc.arg(key_sealed), sqlc.arg(key_generation), sqlc.arg(enabled), sqlc.arg(created_at), sqlc.arg(updated_at))
ON CONFLICT (env_id) DO UPDATE SET
    address = excluded.address,
    prefix_len = excluded.prefix_len,
    router_id = excluded.router_id,
    router_derived = excluded.router_derived,
    key_sealed = excluded.key_sealed,
    key_generation = excluded.key_generation,
    enabled = excluded.enabled,
    updated_at = excluded.updated_at;

-- name: GetVIP :one
SELECT * FROM ingress_vip WHERE env_id = ?;

-- name: ListVIPs :many
-- Every virtual IP of the install, for the ensure loop and the reader.
SELECT * FROM ingress_vip ORDER BY env_id;

-- name: SetVIPEnabled :execrows
UPDATE ingress_vip SET enabled = sqlc.arg(enabled), updated_at = sqlc.arg(updated_at) WHERE env_id = sqlc.arg(env_id);

-- name: UpdateVIPAddress :execrows
UPDATE ingress_vip
SET address = sqlc.arg(address), prefix_len = sqlc.arg(prefix_len), router_id = sqlc.arg(router_id),
    router_derived = sqlc.arg(router_derived), updated_at = sqlc.arg(updated_at)
WHERE env_id = sqlc.arg(env_id);

-- name: DeleteVIP :execrows
DELETE FROM ingress_vip WHERE env_id = ?;

-- name: RecordVIPEnsure :execrows
-- The result of one ensure pass. It changes no configuration, so updated_at stays where it is.
UPDATE ingress_vip SET ensure_at = sqlc.arg(ensure_at), ensure_error = sqlc.arg(ensure_error) WHERE env_id = sqlc.arg(env_id);

-- name: InsertVIPMove :exec
INSERT INTO ingress_vip_moves (env_id, at, from_node_id, from_node_name, to_node_id, to_node_name, reason)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: PruneVIPMoves :exec
-- Keeps the newest keep rows of one environment, newest by time then by insertion id, and deletes the
-- rest. Another environment's rows are never touched.
DELETE FROM ingress_vip_moves
WHERE ingress_vip_moves.env_id = sqlc.arg(env_id)
  AND ingress_vip_moves.id NOT IN (
      SELECT m.id FROM ingress_vip_moves m WHERE m.env_id = sqlc.arg(env_id)
      ORDER BY m.at DESC, m.id DESC LIMIT sqlc.arg(keep)
  );

-- name: ListVIPMoves :many
-- Newest first, equal instants by id descending. The keyset cursor is the (at, id) of the last row of
-- the previous page: a row is on this page when it sorts strictly after the cursor, so a page boundary
-- inside a run of equal instants repeats nothing and skips nothing. before_at of the largest value and
-- before_id of the largest value start from the newest row.
SELECT * FROM ingress_vip_moves
WHERE env_id = sqlc.arg(env_id)
  AND (at < sqlc.arg(before_at) OR (at = sqlc.arg(before_at) AND id < sqlc.arg(before_id)))
ORDER BY at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: LatestVIPMove :one
SELECT * FROM ingress_vip_moves WHERE env_id = ? ORDER BY at DESC, id DESC LIMIT 1;

-- name: VIPMovesSince :many
-- The moves after an instant, oldest first, for a reader that counts the moves in a window.
SELECT * FROM ingress_vip_moves WHERE env_id = ? AND at > ? ORDER BY at, id;
