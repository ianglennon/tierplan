-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- How each user likes to open a shell in an image (OBS-03, D-26): the shell, the
-- custom command, the user to run as and the working directory of their last
-- successful open, keyed by user and image reference. The server writes a row when an
-- open succeeds and the launcher reads it back, so the choice follows the user to
-- another browser or device. Nothing here is a secret. At most 200 rows are kept per
-- user, the oldest trimmed on write. A removed user takes their rows with them.
-- Timestamps are UTC unix nanoseconds.
CREATE TABLE user_shell_prefs (
    user_id     TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    image       TEXT NOT NULL,
    shell       TEXT NOT NULL CHECK (shell IN ('auto', 'bash', 'sh', 'ash', 'custom')),
    command     TEXT NOT NULL DEFAULT '',
    run_as      TEXT NOT NULL DEFAULT '',
    working_dir TEXT NOT NULL DEFAULT '',
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (user_id, image)
);

CREATE INDEX user_shell_prefs_newest ON user_shell_prefs (user_id, updated_at DESC, image);

-- +goose Down
DROP INDEX user_shell_prefs_newest;
DROP TABLE user_shell_prefs;
