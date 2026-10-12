/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: InsertRegistryCredential :exec
-- The sealed secret is written here and never selected back by the list or get queries.
INSERT INTO registry_credentials (
    id, env_id, name, type, host, prefix, username, secret_sealed, secret_last4,
    hub_fallback, ca_pem, insecure, expires_at, created_at, updated_at, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRegistryCredential :one
SELECT id, env_id, name, type, host, prefix, username, secret_last4, hub_fallback, ca_pem, insecure,
       expires_at, last_test_at, last_test_ok, last_test_detail, last_failure_at, last_failure_code,
       created_at, updated_at, created_by
FROM registry_credentials
WHERE id = ?;

-- name: ListRegistryCredentials :many
-- Instance credentials first, then each environment's, by host and prefix.
SELECT id, env_id, name, type, host, prefix, username, secret_last4, hub_fallback, ca_pem, insecure,
       expires_at, last_test_at, last_test_ok, last_test_detail, last_failure_at, last_failure_code,
       created_at, updated_at, created_by
FROM registry_credentials
ORDER BY (env_id IS NOT NULL), env_id, host, prefix, name, id;
