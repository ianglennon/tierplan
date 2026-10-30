-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- Agent environments (ENV-01, ENV-05, PLAT-07, D-01, D-02, D-07, D-11).
--
-- SQLite cannot widen a CHECK constraint, so environments is rebuilt: the new
-- table is created, the rows are copied, the old table is dropped and the new
-- one renamed. Nothing referenced environments before this migration, so the
-- drop is safe. The kind check now admits 'agent'; environments_one_local keeps
-- allowing only one local host. env_type, swarm_cluster_id and engine_id are the
-- identity an agent environment locks to once its first agent reports it (D-11,
-- D-12, D-14). They are NULL until then.
CREATE TABLE environments_new (
    id                 TEXT PRIMARY KEY,
    kind               TEXT NOT NULL CHECK (kind IN ('local','agent')),
    name               TEXT NOT NULL,
    socket_path        TEXT NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    env_type           TEXT CHECK (env_type IN ('swarm','standalone')),
    swarm_cluster_id   TEXT,
    engine_id          TEXT,
    cluster_changed_at INTEGER,
    auto_update        INTEGER NOT NULL DEFAULT 1
);

INSERT INTO environments_new (id, kind, name, socket_path, enabled, created_at, updated_at)
    SELECT id, kind, name, socket_path, enabled, created_at, updated_at FROM environments;

DROP TABLE environments;
ALTER TABLE environments_new RENAME TO environments;

CREATE UNIQUE INDEX environments_one_local ON environments (kind) WHERE kind = 'local';
CREATE UNIQUE INDEX environments_cluster ON environments (swarm_cluster_id) WHERE swarm_cluster_id IS NOT NULL;
CREATE UNIQUE INDEX environments_engine ON environments (engine_id)
    WHERE kind = 'agent' AND env_type = 'standalone' AND engine_id IS NOT NULL;

-- One join token per environment, usable many times (D-01). Only the SHA-256 of
-- its secret part is stored, so a stolen database cannot enrol a host (D-02).
-- Rotation inserts a new generation and marks the previous one superseded.
CREATE TABLE env_join_tokens (
    id            TEXT PRIMARY KEY,
    env_id        TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    token_hash    BLOB NOT NULL,
    generation    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    created_by    TEXT NOT NULL,
    superseded_at INTEGER,
    disabled_at   INTEGER
);

CREATE INDEX env_join_tokens_env ON env_join_tokens (env_id, generation);

-- One issuing CA per environment (D-07). key_sealed is a vault envelope
-- v1.<keyID>.<ciphertext> sealed with the context agent-ca/v1/<envId>, so it
-- carries its key ID (D-56) and cannot be opened in another environment's row.
-- The certificate is public.
CREATE TABLE env_cas (
    env_id     TEXT PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
    cert_pem   TEXT NOT NULL,
    key_sealed TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- Every enrolled node (D-08). Public keys, fingerprints and serials are not
-- secret and are stored as they are (D-55).
CREATE TABLE agents (
    id               TEXT PRIMARY KEY,
    env_id           TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    role             TEXT NOT NULL,
    engine_id        TEXT NOT NULL,
    swarm_node_id    TEXT,
    public_key       BLOB NOT NULL,
    cert_serial      TEXT NOT NULL,
    cert_fingerprint TEXT NOT NULL,
    cert_not_after   INTEGER NOT NULL,
    agent_version    TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('active','revoked')),
    created_at       INTEGER NOT NULL,
    last_seen_at     INTEGER,
    revoked_at       INTEGER,
    revoked_by       TEXT,
    revoke_reason    TEXT
);

CREATE INDEX agents_env ON agents (env_id);
