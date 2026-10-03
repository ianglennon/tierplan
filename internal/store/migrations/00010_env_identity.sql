-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- The identity of an agent environment (ENV-02, D-11, D-12, D-14, D-23).
--
-- Migration 00007 already made the columns env_type, swarm_cluster_id, engine_id
-- and cluster_changed_at and the two unique indexes that stop one swarm, or one
-- standalone host, being two environments (environments_cluster on the cluster ID,
-- environments_engine on a standalone host's engine ID). This migration adds what
-- they were missing:
--
--   pending_cluster_id  the cluster a manager reported after the swarm was
--                       re-initialised. It is recorded together with
--                       cluster_changed_at and is what an admin adopts. Nothing is
--                       rebound until then.
--   name_auto           1 while the environment still carries the placeholder name
--                       "New environment", so its first host may name it. The first
--                       host to name it, or a rename, sets it to 0.
--
-- The two indexes on agents speed up the duplicate checks of an enrolment, which
-- look a host up by engine ID and by swarm node ID across every environment.
ALTER TABLE environments ADD COLUMN pending_cluster_id TEXT;
ALTER TABLE environments ADD COLUMN name_auto INTEGER NOT NULL DEFAULT 0;

-- An agent environment created without a name used to keep a blank name until its
-- first agent enrolled. It now stores the placeholder and the flag instead.
UPDATE environments SET name = 'New environment', name_auto = 1 WHERE kind = 'agent' AND name = '';

CREATE INDEX agents_engine ON agents (engine_id);
CREATE INDEX agents_swarm_node ON agents (swarm_node_id) WHERE swarm_node_id IS NOT NULL;
