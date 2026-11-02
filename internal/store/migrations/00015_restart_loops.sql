-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- Restart-loop detection (RES-13, D-14, D-15, Q5). A service or container whose tasks keep
-- failing is flagged; how many failures in how many minutes makes a loop is set per
-- environment. restart_loop_failures is 2 to 50 (default 3) and restart_loop_window_minutes
-- is 1 to 120 (default 10), so every existing environment starts on the defaults.
ALTER TABLE environments ADD COLUMN restart_loop_failures INTEGER NOT NULL DEFAULT 3
    CHECK (restart_loop_failures BETWEEN 2 AND 50);
ALTER TABLE environments ADD COLUMN restart_loop_window_minutes INTEGER NOT NULL DEFAULT 10
    CHECK (restart_loop_window_minutes BETWEEN 1 AND 120);

-- A flagged target a user has muted: the flag stays on its row, but it raises no bell
-- entry or dashboard count. A mute is keyed by environment, kind and target (the service
-- or container ID), so it follows the target and goes with its environment. name is the
-- target's name when it was muted, for the list of mutes once the target is gone.
-- muted_by is the user's ID. Timestamps are UTC unix nanoseconds.
CREATE TABLE restart_loop_mutes (
    env_id    TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    kind      TEXT NOT NULL CHECK (kind IN ('service', 'container')),
    target_id TEXT NOT NULL,
    name      TEXT NOT NULL DEFAULT '',
    muted_by  TEXT NOT NULL,
    muted_at  INTEGER NOT NULL,
    PRIMARY KEY (env_id, kind, target_id)
);

-- +goose Down
DROP TABLE restart_loop_mutes;
ALTER TABLE environments DROP COLUMN restart_loop_window_minutes;
ALTER TABLE environments DROP COLUMN restart_loop_failures;
