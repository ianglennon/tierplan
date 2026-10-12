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

-- name: UpdateRegistryCredential :execrows
-- Everything but the secret and the layer. A credential never moves between layers.
UPDATE registry_credentials
SET name = ?, type = ?, host = ?, prefix = ?, username = ?, hub_fallback = ?, expires_at = ?, updated_at = ?
WHERE id = ?;

-- name: UpdateRegistryCredentialSecret :execrows
-- Re-seals the secret. Only called when a new one was given; otherwise the stored envelope is
-- never touched.
UPDATE registry_credentials
SET secret_sealed = ?, secret_last4 = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteRegistryCredential :one
DELETE FROM registry_credentials WHERE id = ? RETURNING id;

-- name: FindRegistryCredentialClash :one
-- The name of the other credential of the same layer that a refused write clashed with, so
-- the page can say which one. kind is 'name', 'prefix' or 'hub'.
SELECT name FROM registry_credentials
WHERE env_id IS sqlc.narg(env_id) AND id <> sqlc.arg(id)
  AND ((sqlc.arg(kind) = 'name' AND name = sqlc.arg(name))
    OR (sqlc.arg(kind) = 'prefix' AND host = sqlc.arg(host) AND prefix = sqlc.arg(prefix))
    OR (sqlc.arg(kind) = 'hub' AND hub_fallback = 1))
LIMIT 1;

-- name: ListRegistryCandidates :many
-- What the resolver needs to choose a credential for an environment: the instance credentials
-- and that environment's own, with the environment's name. Never the sealed secret, and never
-- another environment's credential.
SELECT c.id, c.env_id, c.name, c.host, c.prefix, c.hub_fallback, COALESCE(e.name, '') AS env_name
FROM registry_credentials c
LEFT JOIN environments e ON e.id = c.env_id
WHERE c.env_id IS NULL OR c.env_id = sqlc.arg(env_id)
ORDER BY (c.env_id IS NOT NULL), c.host, c.prefix, c.name, c.id;

-- name: GetRegistryCredentialSecret :one
-- The sealed secret and nothing else: only the resolver reads it, and only to open it for one pull.
SELECT secret_sealed FROM registry_credentials WHERE id = ?;

-- name: SetRegistryCredentialFailure :execrows
-- Remembers the most recent pull or browse that the registry refused to sign in (D-07).
UPDATE registry_credentials SET last_failure_at = ?, last_failure_code = ? WHERE id = ?;

-- name: ClearRegistryCredentialFailure :execrows
-- A pull that worked with the credential clears the failure it had. Writes only when there is one.
UPDATE registry_credentials SET last_failure_at = NULL, last_failure_code = ''
WHERE id = ? AND last_failure_at IS NOT NULL;
