-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- Git connections (GIT-01, Phase 9 D-01, D-02): the sign-in Coaming uses to read a repository,
-- and the repository itself, as two separate objects. A credential is an HTTPS token (username
-- and token) or an SSH deploy key. A repository is an address, an optional credential picked
-- explicitly (there is no matching by address) and an optional custom certificate authority.
-- Many repositories can share one credential.
--
-- A row with a NULL env_id is an instance row, one with an env_id belongs to that environment
-- only and goes with it. A repository of an environment may use a credential of its own
-- environment or of the instance; an instance repository only an instance credential. The
-- foreign key cannot say that, so the store checks it (credential_scope) and the key only
-- stops a credential that repositories still use from being deleted (ON DELETE NO ACTION).
--
-- secret_sealed is a vault envelope sealed under the context git-cred/v1/<id>, so a value
-- copied into another row does not open. It is the only place the token or the private key
-- lives and no query but GetGitCredentialSecret selects it. secret_last4 is what the page may
-- show of an HTTPS token; public_key and fingerprint are what it shows of an SSH key. The
-- custom CA is public material and lives on the repository, not the credential. host_key*
-- belong to the SSH plan. last_* columns hold no secret. Timestamps are UTC unix nanoseconds.
--
-- Uniqueness of a name is per layer, so each rule is two partial indexes.
CREATE TABLE git_credentials (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    type               TEXT NOT NULL CHECK (type IN ('https', 'ssh')),
    username           TEXT NOT NULL DEFAULT '',
    secret_sealed      TEXT NOT NULL,
    secret_last4       TEXT NOT NULL DEFAULT '',
    public_key         TEXT NOT NULL DEFAULT '',
    fingerprint        TEXT NOT NULL DEFAULT '',
    last_use_at        INTEGER,
    last_failure_at    INTEGER,
    last_failure_code  TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    created_by         TEXT NOT NULL
);

CREATE UNIQUE INDEX git_cred_name_inst ON git_credentials (name) WHERE env_id IS NULL;
CREATE UNIQUE INDEX git_cred_name_env ON git_credentials (env_id, name) WHERE env_id IS NOT NULL;

CREATE TABLE git_repos (
    id                    TEXT PRIMARY KEY,
    env_id                TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name                  TEXT NOT NULL,
    url                   TEXT NOT NULL,
    credential_id         TEXT REFERENCES git_credentials (id) ON DELETE NO ACTION,
    ca_pem                TEXT NOT NULL DEFAULT '',
    host_key              TEXT NOT NULL DEFAULT '',
    host_key_fp           TEXT NOT NULL DEFAULT '',
    host_key_trusted_by   TEXT NOT NULL DEFAULT '',
    host_key_trusted_at   INTEGER,
    last_test_at          INTEGER,
    last_test_ok          INTEGER,
    last_test_code        TEXT NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    created_by            TEXT NOT NULL
);

CREATE UNIQUE INDEX git_repo_name_inst ON git_repos (name) WHERE env_id IS NULL;
CREATE UNIQUE INDEX git_repo_name_env ON git_repos (env_id, name) WHERE env_id IS NOT NULL;
CREATE INDEX git_repo_credential ON git_repos (credential_id) WHERE credential_id IS NOT NULL;

-- +goose Down
DROP INDEX git_repo_credential;
DROP INDEX git_repo_name_env;
DROP INDEX git_repo_name_inst;
DROP TABLE git_repos;
DROP INDEX git_cred_name_env;
DROP INDEX git_cred_name_inst;
DROP TABLE git_credentials;
