-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors
--
-- The ingress deployment (Phase 24): what the server needs to remember to put the proxy on a swarm and
-- to take it off again. Docker is the source of truth for what runs; these columns and this table hold
-- only the operator's choices and the duties that survive a restart.
--
-- Every instant is INTEGER unix nanoseconds, 0 meaning "not set".
--
-- ingress_env, four new columns (the table is Phase 18's):
--   access_logs   1 when the proxy writes a JSON access log to stdout (TIERPLAN_INGRESS_ACCESS_LOG=1),
--                 0 by default (D-30).
--   paused        1 while a restore has put the environment's ingress under observation: the ensure loop
--                 and the pending wipes only watch until an admin resumes (D-42). 0 by default.
--   ensure_at     when the last ensure pass over the environment ended (0 = never).
--   ensure_error  the class of the last ensure pass's failure, '' when it succeeded. An error class,
--                 never engine text, which could carry a secret (D-11).
--
-- ingress_pending_wipes: a certificate cache volume on a node the server could not reach when it
-- removed the ingress. The private keys in it stay on that node until the wipe is done, so the duty is
-- kept here and retried (D-27). It goes with the environment (CASCADE).
--   env_id         the environment.
--   swarm_node_id  the swarm node whose volume must be wiped.
--   volume         the volume's name.
--   created_at     when the wipe was queued.
-- The primary key is the three names, so queuing the same wipe twice is one row.

-- +goose Up
ALTER TABLE ingress_env ADD COLUMN access_logs INTEGER NOT NULL DEFAULT 0 CHECK (access_logs IN (0, 1));
ALTER TABLE ingress_env ADD COLUMN paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1));
ALTER TABLE ingress_env ADD COLUMN ensure_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ingress_env ADD COLUMN ensure_error TEXT NOT NULL DEFAULT '';

CREATE TABLE ingress_pending_wipes (
    env_id        TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    swarm_node_id TEXT NOT NULL,
    volume        TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (env_id, swarm_node_id, volume)
);

-- +goose Down
DROP TABLE ingress_pending_wipes;
ALTER TABLE ingress_env DROP COLUMN ensure_error;
ALTER TABLE ingress_env DROP COLUMN ensure_at;
ALTER TABLE ingress_env DROP COLUMN paused;
ALTER TABLE ingress_env DROP COLUMN access_logs;
