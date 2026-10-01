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
