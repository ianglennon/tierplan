-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The virtual IP (Phase 26): one address an environment's proxy nodes share, held by one node at a time
-- through VRRP, and the bounded history of where it has been. Docker is the source of truth for what
-- runs; these tables hold only the operator's choice, the sealed VRRP key and the moves the server saw.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- ingress_vip: one row per environment that has a virtual IP, deleted with the environment (CASCADE).
--   env_id          the environment (primary key).
--   address         the IPv4 address as text, without a prefix length.
--   prefix_len      the prefix length of the network the address sits in, 1 to 32.
--   router_id       the VRRP virtual router id, 1 to 255.
--   router_derived  1 when the server derived router_id from the environment, 0 when the admin chose it.
--   key_sealed      the VRRP authentication key, sealed by the vault under ContextIngressVIPKey + env_id
--                   and re-wrapped by vault.RewrapAll. The caller seals it; this table never holds it in
--                   clear, and it is never read back in clear by the store.
--   key_generation  1 for the first key, one higher per rotation.
--   enabled         1 normally. 0 means a switch-off is in progress: the ensure loop finishes the removal
--                   and then deletes the row.
--   created_at, updated_at  when the row was made and last changed.
--   ensure_at       when the last ensure pass over the virtual IP ended (0 = never).
--   ensure_error    the class of the last ensure pass's failure, '' when it succeeded. An error class,
--                   never engine text, which could carry a secret.
--
-- ingress_vip_moves: where the address has been. The server keeps the newest 500 rows per environment
-- and prunes the oldest in the same transaction as the insert. The history goes with the environment.
--   id               integer, assigned in insertion order; it breaks ties between equal instants.
--   env_id           the environment.
--   at               when the move was seen.
--   from_node_id, from_node_name  the node that held the address before ('' = no node).
--   to_node_id, to_node_name      the node that holds it after ('' = no node).
--   reason           holder_failed, probe_failing, drain_handoff, disabled or unknown.
-- The index serves the newest-first read and the keyset page.

-- +goose Up
CREATE TABLE ingress_vip (
    env_id         TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    address        TEXT NOT NULL,
    prefix_len     INTEGER NOT NULL CHECK (prefix_len BETWEEN 1 AND 32),
    router_id      INTEGER NOT NULL CHECK (router_id BETWEEN 1 AND 255),
    router_derived INTEGER NOT NULL CHECK (router_derived IN (0, 1)),
    key_sealed     TEXT NOT NULL,
    key_generation INTEGER NOT NULL CHECK (key_generation >= 1),
    enabled        INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    ensure_at      INTEGER NOT NULL DEFAULT 0,
    ensure_error   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE ingress_vip_moves (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    env_id         TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    at             INTEGER NOT NULL,
    from_node_id   TEXT NOT NULL DEFAULT '',
    from_node_name TEXT NOT NULL DEFAULT '',
    to_node_id     TEXT NOT NULL DEFAULT '',
    to_node_name   TEXT NOT NULL DEFAULT '',
    reason         TEXT NOT NULL CHECK (reason IN ('holder_failed', 'probe_failing', 'drain_handoff', 'disabled', 'unknown'))
);

CREATE INDEX ingress_vip_moves_env_at ON ingress_vip_moves (env_id, at DESC, id DESC);

-- +goose Down
DROP INDEX ingress_vip_moves_env_at;
DROP TABLE ingress_vip_moves;
DROP TABLE ingress_vip;
