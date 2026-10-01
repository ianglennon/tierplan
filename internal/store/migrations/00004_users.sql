-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- Local users (AUTH-01, D-29, D-35). Usernames are stored lowercase so the
-- UNIQUE constraint is case-insensitive. failed_count and locked_until carry the
-- per-account lockout state that plan 12 enforces; disabled is the soft off
-- switch. Timestamps are UTC unix nanoseconds.
CREATE TABLE users (
    id                  TEXT PRIMARY KEY,
    username            TEXT NOT NULL UNIQUE,
    password_hash       TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    password_changed_at INTEGER NOT NULL,
    failed_count        INTEGER NOT NULL DEFAULT 0,
    locked_until        INTEGER,
    disabled            INTEGER NOT NULL DEFAULT 0
);
