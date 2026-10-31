/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Coaming Authors */

-- name: ListNetworkSeen :many
-- Every network of an environment Coaming has a clock for.
SELECT env_id, node_id, network_id, name, first_seen_at, last_in_use_at FROM network_seen
WHERE env_id = ?
ORDER BY node_id, network_id;

-- name: UpsertNetworkSeen :exec
-- A network seen in use. A new row is written whole; an existing one only ever moves forward.
-- The caller writes an existing row only when its stored time is more than five minutes old, so a
-- busy network costs the single writer one write in five minutes and not one per pass.
INSERT INTO network_seen (env_id, node_id, network_id, name, first_seen_at, last_in_use_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, node_id, network_id) DO UPDATE
SET last_in_use_at = excluded.last_in_use_at, name = excluded.name
WHERE excluded.last_in_use_at > network_seen.last_in_use_at;

-- name: InsertNetworkSeenIfAbsent :exec
-- A network seen for the first time and not in use: its clock starts, an existing one is kept.
INSERT INTO network_seen (env_id, node_id, network_id, name, first_seen_at, last_in_use_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, node_id, network_id) DO NOTHING;

-- name: DeleteNetworkSeen :execrows
DELETE FROM network_seen WHERE env_id = ? AND node_id = ? AND network_id = ?;

-- name: ListNetworkKeeps :many
SELECT env_id, node_id, name, kept_by, kept_at FROM network_keeps
WHERE env_id = ?
ORDER BY node_id, name;

-- name: InsertNetworkKeep :exec
-- A second keep of the same network keeps the first (who kept it and when).
INSERT INTO network_keeps (env_id, node_id, name, kept_by, kept_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (env_id, node_id, name) DO NOTHING;

-- name: DeleteNetworkKeep :execrows
DELETE FROM network_keeps WHERE env_id = ? AND node_id = ? AND name = ?;
