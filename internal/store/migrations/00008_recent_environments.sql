-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- The environments a user used last (ENV-06, D-31), kept on the server so the
-- choice follows the user to another browser or device. One row per user and
-- environment, holding when it was last used as UTC unix nanoseconds; the newest
-- three per user are kept. A removed user or environment takes its rows with it.
CREATE TABLE user_recent_environments (
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    env_id  TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    used_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, env_id)
);

CREATE INDEX user_recent_environments_newest ON user_recent_environments (user_id, used_at DESC, env_id);
