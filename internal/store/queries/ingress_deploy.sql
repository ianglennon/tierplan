/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- The ingress deployment (Phase 24): the switch, the access-log choice, the restore pause, the ensure
-- state and the pending cache wipes. config_version is raised by the store's write helper through
-- BumpIngressConfigVersion, not by these statements.

-- name: UpsertIngressEnabled :execrows
-- Creates the environment's row when it has none, otherwise sets the switch. Reports one row when the
-- switch moved or the row was created and zero when it already had that value, so a repeat is a no-op.
INSERT INTO ingress_env (env_id, enabled) VALUES (sqlc.arg(env_id), sqlc.arg(enabled))
ON CONFLICT (env_id) DO UPDATE SET enabled = excluded.enabled
WHERE ingress_env.enabled <> excluded.enabled;

-- name: SetIngressAccessLogs :execrows
-- Creates the row when it has none; reports zero rows when the value was already set.
INSERT INTO ingress_env (env_id, access_logs) VALUES (sqlc.arg(env_id), sqlc.arg(access_logs))
ON CONFLICT (env_id) DO UPDATE SET access_logs = excluded.access_logs
WHERE ingress_env.access_logs <> excluded.access_logs;

-- name: SetIngressPaused :execrows
-- Creates the row when it has none; reports zero rows when the value was already set.
INSERT INTO ingress_env (env_id, paused) VALUES (sqlc.arg(env_id), sqlc.arg(paused))
ON CONFLICT (env_id) DO UPDATE SET paused = excluded.paused
WHERE ingress_env.paused <> excluded.paused;

-- name: RecordIngressEnsure :exec
-- The result of one ensure pass. It changes no route, so config_version stays where it is. Creates the
-- row when it has none.
INSERT INTO ingress_env (env_id, ensure_at, ensure_error) VALUES (sqlc.arg(env_id), sqlc.arg(ensure_at), sqlc.arg(ensure_error))
ON CONFLICT (env_id) DO UPDATE SET ensure_at = excluded.ensure_at, ensure_error = excluded.ensure_error;

-- name: GetIngressDeployState :one
SELECT enabled, access_logs, paused, ensure_at, ensure_error FROM ingress_env WHERE env_id = ?;

-- name: ListEnabledIngressEnvs :many
SELECT env_id FROM ingress_env WHERE enabled = 1 ORDER BY env_id;

-- name: InsertIngressPendingWipe :exec
-- Queuing the same wipe twice is one row, and the first queue time stands.
INSERT OR IGNORE INTO ingress_pending_wipes (env_id, swarm_node_id, volume, created_at) VALUES (?, ?, ?, ?);

-- name: ListIngressPendingWipes :many
SELECT * FROM ingress_pending_wipes WHERE env_id = ? ORDER BY created_at, swarm_node_id, volume;

-- name: ListAllIngressPendingWipes :many
SELECT * FROM ingress_pending_wipes ORDER BY env_id, created_at, swarm_node_id, volume;

-- name: DeleteIngressPendingWipe :execrows
DELETE FROM ingress_pending_wipes WHERE env_id = ? AND swarm_node_id = ? AND volume = ?;

-- name: ListIngressLiveRoutes :many
-- Every route of every claim of one environment, whether or not the claiming stack is running (D-25),
-- with the stack that holds the claim. A system claim has no stack, so the join to stacks is a LEFT JOIN
-- and its stack id and name read as ''.
SELECT r.hostname, r.path_prefix,
       CAST(COALESCE(h.stack_id, '') AS TEXT) AS stack_id,
       CAST(COALESCE(s.name, '') AS TEXT) AS stack_name
FROM ingress_routes r
JOIN ingress_hostnames h ON h.hostname = r.hostname
LEFT JOIN stacks s ON s.id = h.stack_id
WHERE h.env_id = ?
ORDER BY r.hostname, r.path_prefix;

-- name: DeleteIngressNodesByEnv :many
-- Every enrolled proxy node of one environment, active or revoked. The rows come back so the caller can
-- record what it removed (D-39).
DELETE FROM ingress_nodes WHERE env_id = ? RETURNING *;
