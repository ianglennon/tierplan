-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- When TierPlan first saw each network and last saw it in use (NET-04, D-09). Docker keeps no
-- such fact, and a network's creation time alone would flag a long-lived network the moment it
-- is freed, so the orphan check measures its 24-hour grace period from the later of the two.
-- node_id is '' for a swarm-scoped network, which is one network for the whole cluster, and
-- the node's ID for a local one, since another node's network can carry the same ID. Timestamps
-- are UTC unix nanoseconds; last_in_use_at is 0 for a network never seen in use. The Phase 11
-- janitor reads this table too, and a backup carries it.
CREATE TABLE network_seen (
    env_id         TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id        TEXT NOT NULL DEFAULT '',
    network_id     TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    first_seen_at  INTEGER NOT NULL,
    last_in_use_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (env_id, node_id, network_id)
);

-- Networks a person asked TierPlan never to list as orphaned or unused (D-11). A keep is keyed
-- by the network's name (and its node, for a local one, '' for a swarm-scoped one) and not by
-- its ID, so the exemption follows a network of the same name that is made again. kept_by is
-- the user's ID. Timestamps are UTC unix nanoseconds.
CREATE TABLE network_keeps (
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id TEXT NOT NULL DEFAULT '',
    name    TEXT NOT NULL,
    kept_by TEXT NOT NULL,
    kept_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, node_id, name)
);

-- +goose Down
DROP TABLE network_keeps;
DROP TABLE network_seen;
