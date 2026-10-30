-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- Registry credentials (IMG-01, D-01 to D-05): the sign-in Coaming uses when it pulls an
-- image or lists a repository's tags. A row with a NULL env_id is an instance credential,
-- one with an env_id belongs to that environment only and goes with it. A credential
-- applies to a registry host plus an optional namespace prefix (host 'ghcr.io', prefix
-- 'acme'); an empty prefix means the whole registry.
--
-- secret_sealed is a vault envelope sealed under the context registry-cred/v1/<id>, so a
-- value copied into another row does not open. It is the only place the secret lives and
-- it is never returned by the API. secret_last4 is what the page may show of it: the last
-- four characters, or nothing for a short secret. hub_fallback marks the Docker Hub
-- credential that serves every docker.io pull no more specific credential matches.
-- last_test_* and last_failure_* are written by later plans; they hold no secret.
-- Timestamps are UTC unix nanoseconds.
--
-- Uniqueness is per layer, so each rule is two partial indexes: one for the instance rows
-- (env_id IS NULL) and one for the rows of an environment.
CREATE TABLE registry_credentials (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    type               TEXT NOT NULL CHECK (type IN ('dockerhub', 'ghcr', 'gitlab', 'generic')),
    host               TEXT NOT NULL,
    prefix             TEXT NOT NULL DEFAULT '',
    username           TEXT NOT NULL,
    secret_sealed      TEXT NOT NULL,
    secret_last4       TEXT NOT NULL DEFAULT '',
    hub_fallback       INTEGER NOT NULL DEFAULT 0,
    ca_pem             TEXT NOT NULL DEFAULT '',
    insecure           INTEGER NOT NULL DEFAULT 0,
    expires_at         INTEGER,
    last_test_at       INTEGER,
    last_test_ok       INTEGER,
    last_test_detail   TEXT NOT NULL DEFAULT '',
    last_failure_at    INTEGER,
    last_failure_code  TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    created_by         TEXT NOT NULL
);

CREATE UNIQUE INDEX registry_cred_host_prefix_inst ON registry_credentials (host, prefix) WHERE env_id IS NULL;
CREATE UNIQUE INDEX registry_cred_host_prefix_env ON registry_credentials (env_id, host, prefix) WHERE env_id IS NOT NULL;
CREATE UNIQUE INDEX registry_cred_name_inst ON registry_credentials (name) WHERE env_id IS NULL;
CREATE UNIQUE INDEX registry_cred_name_env ON registry_credentials (env_id, name) WHERE env_id IS NOT NULL;
CREATE UNIQUE INDEX registry_cred_hubfb_inst ON registry_credentials (host) WHERE env_id IS NULL AND hub_fallback = 1;
CREATE UNIQUE INDEX registry_cred_hubfb_env ON registry_credentials (env_id) WHERE env_id IS NOT NULL AND hub_fallback = 1;

-- +goose Down
DROP INDEX registry_cred_hubfb_env;
DROP INDEX registry_cred_hubfb_inst;
DROP INDEX registry_cred_name_env;
DROP INDEX registry_cred_name_inst;
DROP INDEX registry_cred_host_prefix_env;
DROP INDEX registry_cred_host_prefix_inst;
DROP TABLE registry_credentials;
