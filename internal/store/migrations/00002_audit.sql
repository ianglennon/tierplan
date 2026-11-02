-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The audit log is an HMAC chain (D-40): each row's hmac covers prev_hmac plus
-- the row's content, keyed from the master key identified by key_id. seq, not
-- ts, orders the chain, because two entries can share a nanosecond.
-- details is a JSON object whose values are all strings.
CREATE TABLE audit_log (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT NOT NULL UNIQUE,
    ts          INTEGER NOT NULL,
    actor_type  TEXT NOT NULL,
    actor_id    TEXT NOT NULL,
    actor_name  TEXT NOT NULL,
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id   TEXT NOT NULL,
    env_id      TEXT,
    result      TEXT NOT NULL,
    reason      TEXT NOT NULL,
    ip          TEXT NOT NULL,
    user_agent  TEXT NOT NULL,
    details     TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    prev_hmac   BLOB NOT NULL,
    hmac        BLOB NOT NULL
);

CREATE INDEX audit_log_ts ON audit_log (ts);
CREATE INDEX audit_log_actor_name ON audit_log (actor_name);
CREATE INDEX audit_log_action ON audit_log (action);
