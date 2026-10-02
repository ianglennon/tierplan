-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- Managed Docker environments (ENV-04, D-19). Phase 1 knows one kind, the
-- Docker socket on the server's own host, and it exists only after an admin
-- opts in: nothing is inserted at startup. Phase 2 widens the kind check for
-- agent environments. The partial unique index is what makes two concurrent
-- opt-ins create one environment: the loser's insert fails and the service
-- maps that to 409 local_environment_exists. Timestamps are UTC unix
-- nanoseconds.
CREATE TABLE environments (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL CHECK (kind IN ('local')),
    name        TEXT NOT NULL,
    socket_path TEXT NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX environments_one_local ON environments (kind) WHERE kind = 'local';
