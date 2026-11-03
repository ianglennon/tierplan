-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- What the janitor does, at three levels (D-04). Each field follows the level above until a level
-- sets it: a node follows its environment, an environment follows the instance defaults. A NULL
-- field means "follow", so a default is never copied into a lower row and changing a default changes
-- every environment that follows it. A backup carries all three tables. Timestamps are UTC unix
-- nanoseconds.

-- The instance defaults: always exactly one row, seeded with the policy of an environment nobody has
-- configured (D-01). The CHECK limits are the ones the settings page accepts.
--   enabled                    1 when the schedule runs the janitor.
--   schedule                   a five-field cron expression, read in the server's time zone.
--   schedule_preset            the name the page shows it by: hourly, every_6h, daily, weekly or custom.
--   keep_n                     how many of the newest exited task containers of each service slot are kept.
--   grace_hours                how long an object must have been stopped or idle before it can be removed.
--   remove_unused_networks     1 to also remove networks no stack ever owned (D-11).
--   remove_stopped_containers  1 to also remove stopped containers no stack or project ever owned (D-11).
--   retention_days             how long the run history is kept ...
--   retention_runs             ... or the newest this many runs of an environment, whichever keeps more.
--   version                    counts the saves, so a save made from a stale read is refused.
--   updated_by                 who saved it last; updated_at when (0 for the seeded row).
CREATE TABLE janitor_defaults (
    id                        INTEGER PRIMARY KEY CHECK (id = 1),
    enabled                   INTEGER NOT NULL,
    schedule                  TEXT NOT NULL,
    schedule_preset           TEXT NOT NULL,
    keep_n                    INTEGER NOT NULL CHECK (keep_n BETWEEN 0 AND 20),
    grace_hours               INTEGER NOT NULL CHECK (grace_hours BETWEEN 1 AND 720),
    remove_unused_networks    INTEGER NOT NULL,
    remove_stopped_containers INTEGER NOT NULL,
    retention_days            INTEGER NOT NULL CHECK (retention_days BETWEEN 7 AND 3650),
    retention_runs            INTEGER NOT NULL CHECK (retention_runs BETWEEN 10 AND 1000),
    version                   INTEGER NOT NULL DEFAULT 1,
    updated_by                TEXT NOT NULL DEFAULT '',
    updated_at                INTEGER NOT NULL DEFAULT 0
);

INSERT INTO janitor_defaults (id, enabled, schedule, schedule_preset, keep_n, grace_hours,
                              remove_unused_networks, remove_stopped_containers, retention_days, retention_runs)
VALUES (1, 1, '0 3 * * *', 'daily', 2, 24, 0, 0, 90, 100);

-- One environment's overrides of the defaults. A missing row and a row of NULLs both mean "follow
-- every default". The fields are those of janitor_defaults; schedule and schedule_preset are set
-- together or not at all.
--   version  0 while the environment has no row; each save counts one up, so a save made from a
--            stale read is refused.
CREATE TABLE janitor_env_policy (
    env_id                    TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    enabled                   INTEGER,
    schedule                  TEXT,
    schedule_preset           TEXT,
    keep_n                    INTEGER CHECK (keep_n IS NULL OR keep_n BETWEEN 0 AND 20),
    grace_hours               INTEGER CHECK (grace_hours IS NULL OR grace_hours BETWEEN 1 AND 720),
    remove_unused_networks    INTEGER,
    remove_stopped_containers INTEGER,
    version                   INTEGER NOT NULL DEFAULT 0,
    updated_by                TEXT NOT NULL DEFAULT '',
    updated_at                INTEGER NOT NULL DEFAULT 0
);

-- One swarm node's overrides of its environment: whether the node is cleaned at all, and how many
-- exited containers of a service slot it keeps. A NULL field follows the environment. The node is
-- named by its swarm node ID, which stays the same when the node's agent is enrolled again.
CREATE TABLE janitor_node_policy (
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    node_id    TEXT NOT NULL,
    enabled    INTEGER,
    keep_n     INTEGER CHECK (keep_n IS NULL OR keep_n BETWEEN 0 AND 20),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (env_id, node_id)
);

-- +goose Down
DROP TABLE janitor_node_policy;
DROP TABLE janitor_env_policy;
DROP TABLE janitor_defaults;
