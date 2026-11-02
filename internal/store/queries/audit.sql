/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: LastAuditHMAC :one
SELECT hmac FROM audit_log ORDER BY seq DESC LIMIT 1;

-- name: InsertAudit :one
INSERT INTO audit_log (
    id, ts, actor_type, actor_id, actor_name, action, target_type, target_id,
    env_id, result, reason, ip, user_agent, details, key_id, prev_hmac, hmac
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING seq;

-- name: ListAuditForVerify :many
SELECT * FROM audit_log WHERE seq > ? ORDER BY seq ASC LIMIT ?;

-- name: ListAuditChain :many
-- Every field an entry's HMAC covers, plus the chain links, in sequence order and
-- paged by sequence. audit.Rekey walks the whole chain with it inside one
-- transaction.
SELECT * FROM audit_log WHERE seq > ? ORDER BY seq ASC LIMIT ?;

-- name: UpdateAuditChain :exec
-- Re-signs one entry: the only columns Rekey ever changes.
UPDATE audit_log SET key_id = ?, prev_hmac = ?, hmac = ? WHERE seq = ?;

-- name: CountAudit :one
SELECT COUNT(*) FROM audit_log;

-- name: MaxSeqBefore :one
SELECT CAST(COALESCE(MAX(seq), 0) AS INTEGER) AS seq FROM audit_log WHERE ts < ?;

-- name: DeleteAuditUpTo :execrows
DELETE FROM audit_log WHERE seq <= ?;

-- name: GetAuditBySeq :one
SELECT * FROM audit_log WHERE seq = ?;

-- name: ListAuditPage :many
-- actions is a JSON array of action keys, not sqlc.slice: sqlc numbers a slice
-- placeholder as an anonymous "?" among numbered ones, which collides as soon as
-- two actions are given. envs is a JSON array of environment IDs the same way.
-- A scoped read (an env Admin, D-09) keeps only entries on one of envs, or on any
-- environment when any_env is set; an entry with no environment, an instance-level
-- event, never matches. The scope is part of the WHERE clause, so it applies
-- before the cursor and the limit and a page is never short of entries.
-- bulk is the bulk-operation ID of the container actions (D-14), compared to the
-- bulk detail of an entry on a fixed JSON path; empty means every entry. Like the
-- other filters it is ANDed with the scope clause, before the cursor and limit.
-- actor_deleted is a display-time join to the user tombstone (D-22c): the signed
-- row is never rewritten when a user is deleted.
SELECT a.*, CAST(COALESCE(u.deleted_at IS NOT NULL, 0) AS INTEGER) AS actor_deleted
FROM audit_log a
LEFT JOIN users u ON u.id = a.actor_id
WHERE (CAST(sqlc.arg(actor) AS TEXT) = '' OR a.actor_name = sqlc.arg(actor))
  AND (json_array_length(sqlc.arg(actions)) = 0 OR a.action IN (SELECT value FROM json_each(sqlc.arg(actions))))
  AND (CAST(sqlc.arg(from_ts) AS INTEGER) = 0 OR a.ts >= sqlc.arg(from_ts))
  AND (CAST(sqlc.arg(to_ts) AS INTEGER) = 0 OR a.ts <= sqlc.arg(to_ts))
  AND (CAST(sqlc.arg(bulk) AS TEXT) = ''
       OR json_extract(a.details, '$.bulk') = sqlc.arg(bulk))
  AND (CAST(sqlc.arg(cursor) AS INTEGER) = 0 OR a.seq < sqlc.arg(cursor))
  AND (CAST(sqlc.arg(scoped) AS INTEGER) = 0
       OR (CAST(sqlc.arg(any_env) AS INTEGER) = 1 AND a.env_id IS NOT NULL)
       OR a.env_id IN (SELECT value FROM json_each(CAST(sqlc.arg(envs) AS TEXT))))
ORDER BY a.seq DESC
LIMIT sqlc.arg(lim);

-- name: ListAuditActors :many
-- The distinct actor names, under the same scope as ListAuditPage, each with
-- whether the user behind it has been deleted.
SELECT a.actor_name, CAST(MAX(COALESCE(u.deleted_at IS NOT NULL, 0)) AS INTEGER) AS deleted
FROM audit_log a
LEFT JOIN users u ON u.id = a.actor_id
WHERE CAST(sqlc.arg(scoped) AS INTEGER) = 0
   OR (CAST(sqlc.arg(any_env) AS INTEGER) = 1 AND a.env_id IS NOT NULL)
   OR a.env_id IN (SELECT value FROM json_each(CAST(sqlc.arg(envs) AS TEXT)))
GROUP BY a.actor_name
ORDER BY a.actor_name LIMIT 200;
