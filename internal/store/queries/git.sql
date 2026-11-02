/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: InsertGitCredential :exec
-- The sealed secret is written here and never selected back by the list or get queries.
INSERT INTO git_credentials (
    id, env_id, name, type, username, secret_sealed, secret_last4, public_key, fingerprint,
    created_at, updated_at, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetGitCredential :one
SELECT c.id, c.env_id, c.name, c.type, c.username, c.secret_last4, c.public_key, c.fingerprint,
       c.last_use_at, c.last_failure_at, c.last_failure_code, c.created_at, c.updated_at, c.created_by,
       (SELECT count(*) FROM git_repos r WHERE r.credential_id = c.id) AS used_by
FROM git_credentials c
WHERE c.id = ?;

-- name: ListGitCredentials :many
-- Instance credentials first, then each environment's, by name.
SELECT c.id, c.env_id, c.name, c.type, c.username, c.secret_last4, c.public_key, c.fingerprint,
       c.last_use_at, c.last_failure_at, c.last_failure_code, c.created_at, c.updated_at, c.created_by,
       (SELECT count(*) FROM git_repos r WHERE r.credential_id = c.id) AS used_by
FROM git_credentials c
ORDER BY (c.env_id IS NOT NULL), c.env_id, c.name, c.id;

-- name: GetGitCredentialSecret :one
-- The sealed secret and nothing else: only Store.Remote reads it, and only to open it for one call.
SELECT secret_sealed FROM git_credentials WHERE id = ?;

-- name: UpdateGitCredentialInstance :execrows
-- Everything but the secret and the layer. Bounded to the instance layer: a credential of an
-- environment matches no row, so an instance route cannot change it.
UPDATE git_credentials
SET name = ?, type = ?, username = ?, updated_at = ?
WHERE id = ? AND env_id IS NULL;

-- name: UpdateGitCredentialInEnv :execrows
-- The same, bounded to one environment's layer.
UPDATE git_credentials
SET name = sqlc.arg(name), type = sqlc.arg(type), username = sqlc.arg(username), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND env_id = sqlc.arg(env_id);

-- name: UpdateGitCredentialSecret :execrows
-- Re-seals the secret and replaces what is shown of it. Only called when a new one was given;
-- otherwise the stored envelope is never touched.
UPDATE git_credentials
SET secret_sealed = ?, secret_last4 = ?, public_key = ?, fingerprint = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteGitCredential :one
-- Bounded to one layer: env_id NULL is the instance's.
DELETE FROM git_credentials WHERE id = sqlc.arg(id) AND env_id IS sqlc.narg(env_id) RETURNING id;

-- name: CountReposUsingCredential :one
SELECT count(*) FROM git_repos WHERE credential_id = ?;

-- name: SetGitCredentialUse :execrows
-- A call with the credential signed in: remembers when and forgets an earlier refusal.
UPDATE git_credentials SET last_use_at = ?, last_failure_at = NULL, last_failure_code = '' WHERE id = ?;

-- name: SetGitCredentialFailure :execrows
-- The most recent call the git server refused to sign in. code is a stable code, never text.
UPDATE git_credentials SET last_failure_at = ?, last_failure_code = ? WHERE id = ?;

-- name: InsertGitRepo :exec
INSERT INTO git_repos (
    id, env_id, name, url, credential_id, ca_pem, host_key, host_key_fp, host_key_trusted_by, host_key_trusted_at,
    created_at, updated_at, created_by
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetGitRepo :one
SELECT r.id, r.env_id, r.name, r.url, r.credential_id, r.ca_pem,
       r.host_key, r.host_key_fp, r.host_key_trusted_by, r.host_key_trusted_at,
       r.last_test_at, r.last_test_ok, r.last_test_code, r.created_at, r.updated_at, r.created_by,
       COALESCE(c.name, '') AS credential_name, c.env_id AS credential_env_id,
       (SELECT count(*) FROM stack_git g WHERE g.repo_id = r.id) AS used_by
FROM git_repos r
LEFT JOIN git_credentials c ON c.id = r.credential_id
WHERE r.id = ?;

-- name: ListGitRepos :many
-- Instance repositories first, then each environment's, by name.
SELECT r.id, r.env_id, r.name, r.url, r.credential_id, r.ca_pem,
       r.host_key, r.host_key_fp, r.host_key_trusted_by, r.host_key_trusted_at,
       r.last_test_at, r.last_test_ok, r.last_test_code, r.created_at, r.updated_at, r.created_by,
       COALESCE(c.name, '') AS credential_name, c.env_id AS credential_env_id,
       (SELECT count(*) FROM stack_git g WHERE g.repo_id = r.id) AS used_by
FROM git_repos r
LEFT JOIN git_credentials c ON c.id = r.credential_id
ORDER BY (r.env_id IS NOT NULL), r.env_id, r.name, r.id;

-- name: UpdateGitRepoInstance :execrows
-- Bounded to the instance layer.
UPDATE git_repos
SET name = ?, url = ?, credential_id = ?, ca_pem = ?, updated_at = ?
WHERE id = ? AND env_id IS NULL;

-- name: UpdateGitRepoInEnv :execrows
-- The same, bounded to one environment's layer.
UPDATE git_repos
SET name = sqlc.arg(name), url = sqlc.arg(url), credential_id = sqlc.arg(credential_id), ca_pem = sqlc.arg(ca_pem),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND env_id = sqlc.arg(env_id);

-- name: ClearGitRepoTest :execrows
-- Forgets the last test of a repository whose address, credential or CA just changed: it was
-- seen against something that no longer applies.
UPDATE git_repos SET last_test_at = NULL, last_test_ok = NULL, last_test_code = '' WHERE id = ?;

-- name: DeleteGitRepo :one
-- Bounded to one layer: env_id NULL is the instance's.
DELETE FROM git_repos WHERE id = sqlc.arg(id) AND env_id IS sqlc.narg(env_id) RETURNING id;

-- name: SetGitRepoTest :execrows
-- Remembers the outcome of the last test of the saved repository. code is a stable code, never
-- text from the git server.
UPDATE git_repos SET last_test_at = ?, last_test_ok = ?, last_test_code = ? WHERE id = ?;

-- name: ClearGitCredentialOutcome :execrows
-- Forgets when a credential last signed in and the last refusal after its secret, type or user
-- name changed: they were seen with something that no longer applies.
UPDATE git_credentials SET last_use_at = NULL, last_failure_at = NULL, last_failure_code = '' WHERE id = ?;

-- name: SetGitRepoHostKey :execrows
-- Stores the SSH host key a person trusted (an authorized_keys line, public material), its
-- fingerprint, who trusted it and when. An empty key forgets the trust. Not bounded to a layer:
-- the caller has already read the repository and checked its layer.
UPDATE git_repos
SET host_key = ?, host_key_fp = ?, host_key_trusted_by = ?, host_key_trusted_at = ?, updated_at = ?
WHERE id = ?;
