-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- When each environment is checked for image updates (Phase 12, D-01), at two levels like the janitor's
-- policy (Phase 11, D-02): each field of an environment follows the instance default until the environment
-- sets it. A NULL field means "follow", so a default is never copied into an environment's row and changing
-- the default changes every environment that follows it. A backup carries both tables. Timestamps are UTC
-- unix nanoseconds.

-- The instance defaults: always exactly one row, seeded with the policy of an environment nobody has
-- configured: checked, every 6 hours (D-01).
--   enabled          1 when the schedule checks for updates.
--   schedule         a five-field cron expression, read in the server's time zone, firing no more often
--                    than every hour.
--   schedule_preset  the name the page shows it by: hourly, every_6h, daily, weekly or custom.
--   version          counts the saves, so a save made from a stale read is refused.
--   updated_by       who saved it last; updated_at when (0 for the seeded row).
CREATE TABLE image_update_defaults (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    enabled         INTEGER NOT NULL,
    schedule        TEXT NOT NULL,
    schedule_preset TEXT NOT NULL,
    version         INTEGER NOT NULL DEFAULT 1,
    updated_by      TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL DEFAULT 0
);

INSERT INTO image_update_defaults (id, enabled, schedule, schedule_preset)
VALUES (1, 1, '0 */6 * * *', 'every_6h');

-- One environment's overrides of the defaults. A missing row and a row of NULLs both mean "follow every
-- default". schedule and schedule_preset are set together or not at all.
--   enabled          NULL follows the default; 0 stops the scheduled checks of this environment (a person
--                    can still press Check now); 1 runs them.
--   schedule         NULL follows the default.
--   schedule_preset  the name of schedule; NULL together with schedule.
--   version          0 while the environment has no row; each save counts one up, so a save made from a
--                    stale read is refused.
--   updated_by       who saved it last; updated_at when.
CREATE TABLE image_update_env_policy (
    env_id          TEXT PRIMARY KEY REFERENCES environments (id) ON DELETE CASCADE,
    enabled         INTEGER,
    schedule        TEXT,
    schedule_preset TEXT,
    version         INTEGER NOT NULL DEFAULT 0,
    updated_by      TEXT NOT NULL DEFAULT '',
    updated_at      INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE image_update_env_policy;
DROP TABLE image_update_defaults;
