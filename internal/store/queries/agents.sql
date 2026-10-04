/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertAgentEnvironment :exec
INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, name_auto)
VALUES (?, 'agent', ?, '', 1, ?, ?, ?);

-- name: NameEnvironmentAuto :execrows
UPDATE environments SET name = ?, name_auto = 0, updated_at = ?
WHERE id = ? AND (name_auto = 1 OR name = '');

-- name: BindEnvironmentType :execrows
UPDATE environments SET env_type = ?, updated_at = ? WHERE id = ? AND env_type IS NULL;

-- name: BindEnvironmentEngine :execrows
UPDATE environments SET engine_id = ?, updated_at = ? WHERE id = ? AND engine_id IS NULL;

-- name: BindEnvironmentCluster :execrows
UPDATE environments SET swarm_cluster_id = ?, updated_at = ? WHERE id = ? AND swarm_cluster_id IS NULL;

-- name: RecordClusterChange :execrows
UPDATE environments SET cluster_changed_at = ?, pending_cluster_id = ?, updated_at = ?
WHERE id = ? AND kind = 'agent' AND cluster_changed_at IS NULL;

-- name: AdoptPendingCluster :execrows
UPDATE environments
SET swarm_cluster_id = pending_cluster_id, pending_cluster_id = NULL, cluster_changed_at = NULL, updated_at = ?
WHERE id = ? AND kind = 'agent' AND cluster_changed_at IS NOT NULL AND pending_cluster_id IS NOT NULL;

-- The duplicate checks of an enrolment. Each answers the name of another agent
-- environment that already owns the identity, or no row. The local socket
-- environment is never one of them (D-12, D-48).

-- name: OtherEnvironmentWithEngine :one
SELECT e.name FROM environments e
WHERE e.kind = 'agent' AND e.id <> sqlc.arg(env_id)
  AND (e.engine_id = sqlc.arg(engine_id)
       OR EXISTS (SELECT 1 FROM agents a WHERE a.env_id = e.id AND a.status = 'active' AND a.engine_id = sqlc.arg(engine_id)))
ORDER BY e.created_at, e.id
LIMIT 1;

-- name: OtherEnvironmentWithCluster :one
SELECT e.name FROM environments e
WHERE e.kind = 'agent' AND e.id <> sqlc.arg(env_id) AND e.swarm_cluster_id = sqlc.arg(cluster_id)
ORDER BY e.created_at, e.id
LIMIT 1;

-- name: OtherEnvironmentWithSwarmNode :one
SELECT e.name FROM agents a
JOIN environments e ON e.id = a.env_id
WHERE e.kind = 'agent' AND a.env_id <> sqlc.arg(env_id) AND a.status = 'active' AND a.swarm_node_id = sqlc.arg(node_id)
ORDER BY e.created_at, e.id
LIMIT 1;

-- name: InsertJoinToken :exec
INSERT INTO env_join_tokens (id, env_id, token_hash, generation, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListJoinTokens :many
SELECT * FROM env_join_tokens
WHERE env_id = ?
ORDER BY generation DESC;

-- name: AgentEnvironmentExists :one
SELECT COUNT(*) FROM environments WHERE id = ? AND kind = 'agent';

-- name: NextGeneration :one
SELECT CAST(COALESCE(MAX(generation), 0) + 1 AS INTEGER) AS next_generation
FROM env_join_tokens
WHERE env_id = ?;

-- name: SupersedeCurrentToken :execrows
UPDATE env_join_tokens SET superseded_at = ?
WHERE env_id = ? AND superseded_at IS NULL;

-- name: DisableCurrentToken :execrows
UPDATE env_join_tokens SET disabled_at = ?
WHERE env_id = ? AND superseded_at IS NULL AND disabled_at IS NULL;

-- name: TokenStatus :one
SELECT t.generation, t.created_at, t.disabled_at, COALESCE(u.username, t.created_by) AS created_by
FROM env_join_tokens t
LEFT JOIN users u ON u.id = t.created_by
WHERE t.env_id = ? AND t.superseded_at IS NULL
ORDER BY t.generation DESC
LIMIT 1;

-- name: ActiveAgentRoles :many
SELECT role, COUNT(*) AS agents FROM agents
WHERE env_id = ? AND status = 'active'
GROUP BY role;

-- name: InsertCA :exec
INSERT INTO env_cas (env_id, cert_pem, key_sealed, created_at)
VALUES (?, ?, ?, ?);

-- name: GetCA :one
SELECT * FROM env_cas WHERE env_id = ?;

-- name: InsertAgent :exec
INSERT INTO agents (id, env_id, name, role, engine_id, swarm_node_id, public_key, cert_serial,
                    cert_fingerprint, cert_not_after, agent_version, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?);

-- name: GetAgent :one
SELECT * FROM agents WHERE id = ?;

-- name: ListAgentsByEnv :many
SELECT * FROM agents WHERE env_id = ? ORDER BY created_at, id;

-- The agents that may connect: everything not revoked.
-- name: ListActiveAgentIDs :many
SELECT id FROM agents WHERE status = 'active' ORDER BY id;

-- name: TouchAgentLastSeen :exec
UPDATE agents SET last_seen_at = ? WHERE id = ?;

-- A renewal keeps the key and changes only the certificate's record. The status
-- test is what makes a revoke that commits first win: no row is changed, so the
-- renewal is refused (T-02-83).
-- name: RenewAgentCert :execrows
UPDATE agents
SET cert_serial = ?, cert_fingerprint = ?, cert_not_after = ?
WHERE id = ? AND status = 'active';

-- name: RevokeAgent :execrows
UPDATE agents
SET status = 'revoked', revoked_at = ?, revoked_by = ?, revoke_reason = ?
WHERE id = ? AND env_id = ? AND status = 'active';

-- name: InsertBlock :exec
INSERT INTO agent_blocks (env_id, kind, identity, agent_id, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (env_id, kind, identity)
DO UPDATE SET agent_id = excluded.agent_id, created_at = excluded.created_at, created_by = excluded.created_by;

-- name: ListBlocksForAgent :many
SELECT * FROM agent_blocks WHERE env_id = ? AND agent_id = ? ORDER BY kind;

-- name: DeleteBlocksForAgent :execrows
DELETE FROM agent_blocks WHERE env_id = ? AND agent_id = ?;

-- name: IsBlocked :one
SELECT COUNT(*) FROM agent_blocks WHERE env_id = ? AND kind = ? AND identity = ?;

-- name: ListBlocks :many
SELECT * FROM agent_blocks WHERE env_id = ? ORDER BY kind, identity;
