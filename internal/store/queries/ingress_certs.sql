/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: InsertIngressCert :exec
-- A new certificate row in its first phase. The partial unique index ingress_certs_live refuses a second live
-- row for the hostname.
INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, phase, requested_at, requested_by,
                           created_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetIngressCert :one
SELECT * FROM ingress_certs WHERE id = ?;

-- name: GetLiveIngressCertByHostname :one
SELECT * FROM ingress_certs WHERE hostname = ? AND retired_at = 0;

-- name: ListIngressCertsByEnv :many
SELECT * FROM ingress_certs WHERE env_id = ? ORDER BY hostname, created_at, id;

-- name: ListLiveIngressCerts :many
SELECT * FROM ingress_certs WHERE retired_at = 0 ORDER BY hostname, id;

-- name: UpdateIngressCertIssued :exec
-- Stores an issued (or uploaded) certificate: the chain, the sealed key, the leaf's serial and validity. The
-- row becomes idle and the queue and failure columns clear.
UPDATE ingress_certs SET
    chain_pem = ?, key_sealed = ?, serial = ?, not_before = ?, not_after = ?, issued_at = ?, account_url = ?,
    last_success_at = ?, phase = 'idle', stopped_reason = '',
    queue_code = '', queue_used = 0, queue_cap = 0, queue_domain = '', queue_next_slot_at = 0,
    consecutive_failures = 0, next_attempt_at = 0, retry_after_until = 0, failure_times = '[]',
    last_error_at = 0, last_error_class = '', last_error_detail = '',
    updated_at = ?
WHERE id = ?;

-- name: BumpIngressCertsVersion :exec
-- Creates the environment's row on its first change, then raises certs_version by one. It never touches
-- config_version.
INSERT INTO ingress_env (env_id, certs_version, updated_at, updated_by) VALUES (?, 1, ?, ?)
ON CONFLICT (env_id) DO UPDATE SET certs_version = ingress_env.certs_version + 1;

-- name: GetIngressCertsVersion :one
SELECT CAST(COALESCE((SELECT certs_version FROM ingress_env WHERE env_id = ?), 0) AS INTEGER) AS certs_version;
