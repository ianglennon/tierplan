-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The ingress node identity (Phase 22): the proxy nodes an environment enrols over the ingress channel,
-- and the join token they enrol with. These are a second kind of enrolled identity beside the agent's
-- (agents, env_join_tokens): a different trust (a node holds every private key of its environment), a
-- different credential (a stored Ed25519 public key, no CA, no renewal) and a different protocol.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- ingress_join_tokens: the secret a proxy node enrols with, prefix tpit_. One current token per
-- environment, usable many times. Only the SHA-256 of the secret is stored, so a stolen database cannot
-- enrol a node. Rotation inserts a new generation and supersedes the previous one.
--   id             UUIDv7.
--   env_id         the environment; its tokens go with it (CASCADE).
--   token_hash     SHA-256 of the secret, exactly 32 bytes (CHECK).
--   generation     1 for the first token an environment ever had, then one higher per rotation.
--   created_at, created_by  when it was minted and by whom.
--   superseded_at  when a rotation replaced it (0 = it is the current token). The partial unique index
--                  ingress_join_tokens_current admits one row with superseded_at = 0 per environment.
--   disabled_at    when an admin turned the current token off (0 = enabled).
--
-- ingress_nodes: every enrolled proxy node. The public key is not secret and is stored as it is.
--   id             UUIDv7.
--   env_id         the environment; its nodes go with it (CASCADE).
--   name           the node's display name, '' until it reports one.
--   swarm_node_id  the swarm node the proxy runs on; NULL for a standalone environment's node.
--   public_key     the node's Ed25519 public key, exactly 32 bytes (CHECK).
--   status         'active' or 'revoked'.
--   version        the proxy build the node last reported, '' until its first hello.
--   schemas        the config schema versions the node accepts, a JSON array as text.
--   applied_bundle_id, applied_at      the last bundle the node applied and when.
--   rejected_bundle_id, rejected_reason, rejected_at  the last bundle the node rejected, why and when.
--   health, health_at  '' (unknown), 'ok' or 'unhealthy', and when it was reported.
--   held_hostnames the hostnames whose certificates the node holds, a JSON array as text.
--   last_seen_at   when the node last spoke on its channel.
--   created_at     when it enrolled.
--   revoked_at, revoked_by, revoke_reason  when, by whom and why an admin revoked it.
--   reenrol_blocked 1 when the revoke also blocks the same host from enrolling again (the default
--                  revoke), 0 when the admin allowed re-enrolment.
--
-- The partial unique indexes are what make "one live enrolment per node" a rule of the database under
-- the single writer, not a check of the service:
--   ingress_nodes_active_swarm_node  one active row per (env_id, swarm_node_id) of a swarm environment.
--   ingress_nodes_active_standalone  one active row with a NULL swarm_node_id per environment.
-- A revoked row falls out of both, so the host may enrol again once its row is revoked.

-- +goose Up
CREATE TABLE ingress_join_tokens (
    id            TEXT PRIMARY KEY,
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    token_hash    BLOB NOT NULL CHECK (length(token_hash) = 32),
    generation    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    created_by    TEXT NOT NULL,
    superseded_at INTEGER NOT NULL DEFAULT 0,
    disabled_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX ingress_join_tokens_env ON ingress_join_tokens (env_id, generation);
CREATE UNIQUE INDEX ingress_join_tokens_current ON ingress_join_tokens (env_id) WHERE superseded_at = 0;

CREATE TABLE ingress_nodes (
    id                 TEXT PRIMARY KEY,
    env_id             TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name               TEXT NOT NULL DEFAULT '',
    swarm_node_id      TEXT,
    public_key         BLOB NOT NULL CHECK (length(public_key) = 32),
    status             TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
    version            TEXT NOT NULL DEFAULT '',
    schemas            TEXT NOT NULL DEFAULT '[]',
    applied_bundle_id  TEXT NOT NULL DEFAULT '',
    applied_at         INTEGER NOT NULL DEFAULT 0,
    rejected_bundle_id TEXT NOT NULL DEFAULT '',
    rejected_reason    TEXT NOT NULL DEFAULT '',
    rejected_at        INTEGER NOT NULL DEFAULT 0,
    health             TEXT NOT NULL DEFAULT '' CHECK (health IN ('', 'ok', 'unhealthy')),
    health_at          INTEGER NOT NULL DEFAULT 0,
    held_hostnames     TEXT NOT NULL DEFAULT '[]',
    last_seen_at       INTEGER NOT NULL DEFAULT 0,
    created_at         INTEGER NOT NULL,
    revoked_at         INTEGER NOT NULL DEFAULT 0,
    revoked_by         TEXT NOT NULL DEFAULT '',
    revoke_reason      TEXT NOT NULL DEFAULT '',
    reenrol_blocked    INTEGER NOT NULL DEFAULT 0 CHECK (reenrol_blocked IN (0, 1))
);

CREATE INDEX ingress_nodes_env ON ingress_nodes (env_id, created_at);
CREATE UNIQUE INDEX ingress_nodes_active_swarm_node ON ingress_nodes (env_id, swarm_node_id)
    WHERE status = 'active' AND swarm_node_id IS NOT NULL;
CREATE UNIQUE INDEX ingress_nodes_active_standalone ON ingress_nodes (env_id)
    WHERE status = 'active' AND swarm_node_id IS NULL;

-- +goose Down
DROP TABLE ingress_nodes;
DROP TABLE ingress_join_tokens;
