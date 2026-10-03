/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertAgentEnvironment :exec
INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
VALUES (?, 'agent', ?, '', 1, ?, ?);

-- name: NameEnvironmentIfBlank :execrows
UPDATE environments SET name = ?, updated_at = ? WHERE id = ? AND name = '';

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

-- name: TouchAgentLastSeen :exec
UPDATE agents SET last_seen_at = ? WHERE id = ?;
