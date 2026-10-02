/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

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
-- two actions are given.
SELECT * FROM audit_log
WHERE (CAST(sqlc.arg(actor) AS TEXT) = '' OR actor_name = sqlc.arg(actor))
  AND (json_array_length(sqlc.arg(actions)) = 0 OR action IN (SELECT value FROM json_each(sqlc.arg(actions))))
  AND (CAST(sqlc.arg(from_ts) AS INTEGER) = 0 OR ts >= sqlc.arg(from_ts))
  AND (CAST(sqlc.arg(to_ts) AS INTEGER) = 0 OR ts <= sqlc.arg(to_ts))
  AND (CAST(sqlc.arg(cursor) AS INTEGER) = 0 OR seq < sqlc.arg(cursor))
ORDER BY seq DESC
LIMIT sqlc.arg(lim);

-- name: ListAuditActors :many
SELECT DISTINCT actor_name FROM audit_log ORDER BY actor_name LIMIT 200;
