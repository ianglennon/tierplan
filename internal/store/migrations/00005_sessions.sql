-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- Server-side sessions (AUTH-02, D-32). The primary key is the SHA-256 of the
-- opaque 256-bit session ID, so a copy of the database never contains a usable
-- cookie value. absolute_expires_at is set for "remember me" sessions only;
-- the others end by idle timeout from last_seen_at (D-31). Timestamps are UTC
-- unix nanoseconds.
CREATE TABLE sessions (
    id_hash             BLOB PRIMARY KEY,
    user_id             TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    absolute_expires_at INTEGER,
    remember            INTEGER NOT NULL,
    ip                  TEXT NOT NULL,
    user_agent          TEXT NOT NULL
);

CREATE INDEX sessions_user_id ON sessions (user_id);
