-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The identities of hosts an admin revoked (ENV-05, D-08, D-09). A revoke blocks the
-- engine ID and, on a swarm, the swarm node ID of the agent's host, so the host
-- cannot enrol again with a join token that is still valid. Only an unblock deletes
-- the rows. agent_id is the agent whose revoke made the block, so an unblock of
-- that agent lifts exactly its blocks. Removing the environment removes its blocks.
CREATE TABLE agent_blocks (
    env_id     TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('engine_id','swarm_node_id')),
    identity   TEXT NOT NULL,
    agent_id   TEXT,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (env_id, kind, identity)
);

CREATE INDEX agent_blocks_agent ON agent_blocks (agent_id);
