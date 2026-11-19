/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- The ingress node service (Phase 22): the proxy nodes an environment enrols and their join token.
-- Every lookup is environment-scoped except GetIngressNode, which the connect path uses by ID and then
-- checks the environment of.

-- name: InsertIngressJoinToken :exec
INSERT INTO ingress_join_tokens (id, env_id, token_hash, generation, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListIngressJoinTokens :many
-- Every token the environment ever had, newest generation first.
SELECT * FROM ingress_join_tokens
WHERE env_id = ?
ORDER BY generation DESC;

-- name: NextIngressTokenGeneration :one
SELECT CAST(COALESCE(MAX(generation), 0) + 1 AS INTEGER) AS next_generation
FROM ingress_join_tokens
WHERE env_id = ?;

-- name: SupersedeCurrentIngressToken :execrows
UPDATE ingress_join_tokens SET superseded_at = ?
WHERE env_id = ? AND superseded_at = 0;

-- name: DisableCurrentIngressToken :execrows
UPDATE ingress_join_tokens SET disabled_at = ?
WHERE env_id = ? AND superseded_at = 0 AND disabled_at = 0;

-- name: IngressTokenStatus :one
-- The environment's current token, or no row when it has never had one.
SELECT * FROM ingress_join_tokens
WHERE env_id = ? AND superseded_at = 0
ORDER BY generation DESC
LIMIT 1;

-- name: IngressEnvironmentTopology :one
-- The kind and the swarm or standalone type of one environment, so the enrolment can tell a swarm node
-- (which must name a swarm node ID) from a standalone host (which must not).
SELECT kind, env_type FROM environments WHERE id = ?;

-- name: InsertIngressNode :exec
INSERT INTO ingress_nodes (id, env_id, name, swarm_node_id, public_key, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetIngressNode :one
SELECT * FROM ingress_nodes WHERE id = ?;

-- name: ListIngressNodes :many
SELECT * FROM ingress_nodes
WHERE env_id = ?
ORDER BY created_at, id;

-- name: CountActiveIngressSwarmNode :one
SELECT COUNT(*) FROM ingress_nodes
WHERE env_id = sqlc.arg(env_id) AND swarm_node_id = sqlc.arg(swarm_node_id) AND status = 'active';

-- name: CountActiveIngressStandalone :one
SELECT COUNT(*) FROM ingress_nodes
WHERE env_id = ? AND swarm_node_id IS NULL AND status = 'active';

-- name: CountBlockedIngressSwarmNode :one
-- A revoked row whose revoke also blocked re-enrolment (D-20).
SELECT COUNT(*) FROM ingress_nodes
WHERE env_id = sqlc.arg(env_id) AND swarm_node_id = sqlc.arg(swarm_node_id)
  AND status = 'revoked' AND reenrol_blocked = 1;

-- name: CountBlockedIngressStandalone :one
SELECT COUNT(*) FROM ingress_nodes
WHERE env_id = ? AND swarm_node_id IS NULL AND status = 'revoked' AND reenrol_blocked = 1;

-- name: RevokeIngressNode :execrows
-- Only an active row is revoked, so a second revoke changes nothing and the first one's author and
-- reason stand.
UPDATE ingress_nodes
SET status = 'revoked', revoked_at = ?, revoked_by = ?, revoke_reason = ?, reenrol_blocked = ?
WHERE id = ? AND env_id = ? AND status = 'active';

-- name: UpdateIngressNodeHello :exec
UPDATE ingress_nodes SET version = ?, schemas = ?, last_seen_at = ?
WHERE id = ? AND env_id = ?;

-- name: UpdateIngressNodeHeld :exec
UPDATE ingress_nodes SET held_hostnames = ?
WHERE id = ? AND env_id = ?;

-- name: UpdateIngressNodeApplied :exec
UPDATE ingress_nodes SET applied_bundle_id = ?, applied_at = ?
WHERE id = ? AND env_id = ?;

-- name: UpdateIngressNodeRejected :exec
UPDATE ingress_nodes SET rejected_bundle_id = ?, rejected_reason = ?, rejected_at = ?
WHERE id = ? AND env_id = ?;

-- name: UpdateIngressNodeHealth :exec
UPDATE ingress_nodes SET health = ?, health_at = ?
WHERE id = ? AND env_id = ?;

-- name: TouchIngressNodeLastSeen :exec
UPDATE ingress_nodes SET last_seen_at = ?
WHERE id = ? AND env_id = ?;
