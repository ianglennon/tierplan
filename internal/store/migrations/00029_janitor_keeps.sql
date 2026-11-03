-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- Everything a person asked the janitor, or the orphan check, never to remove or list: one table
-- for networks, containers and images (Phase 11, D-12). It generalises the Phase 10 network_keeps
-- table, whose rows are copied across below as kind 'network'. kind is network, container or
-- image. ref is what the keep names: the network's name (not its ID, so a network of the same
-- name that is made again stays kept, as in Phase 10), the container's ID, or the image's ID.
-- name is the label shown in the list (the network's name, a container's name, an image's
-- first tag). node_id is '' for a swarm-wide network and otherwise the ID of the node the object
-- lives on (envs.KeepNodeKey); an agent that is enrolled again gets a new node ID, so a keep on
-- a node-local object does not follow it there. kept_by is the user's ID. Timestamps are UTC
-- unix nanoseconds.
CREATE TABLE janitor_keeps (
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind    TEXT NOT NULL CHECK (kind IN ('network', 'container', 'image')),
    node_id TEXT NOT NULL DEFAULT '',
    ref     TEXT NOT NULL,
    name    TEXT NOT NULL DEFAULT '',
    kept_by TEXT NOT NULL,
    kept_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, node_id, ref)
);

-- Rows are copied before the old table is dropped, so no network a person chose Keep on is lost.
INSERT INTO janitor_keeps (env_id, kind, node_id, ref, name, kept_by, kept_at)
SELECT env_id, 'network', node_id, name, name, kept_by, kept_at FROM network_keeps;

DROP TABLE network_keeps;

-- +goose Down
-- Container and image keeps have nowhere to go in the older shape and are dropped with the table;
-- every network keep is restored.
CREATE TABLE network_keeps (
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id TEXT NOT NULL DEFAULT '',
    name    TEXT NOT NULL,
    kept_by TEXT NOT NULL,
    kept_at INTEGER NOT NULL,
    PRIMARY KEY (env_id, node_id, name)
);

INSERT INTO network_keeps (env_id, node_id, name, kept_by, kept_at)
SELECT env_id, node_id, ref, kept_by, kept_at FROM janitor_keeps WHERE kind = 'network';

DROP TABLE janitor_keeps;
