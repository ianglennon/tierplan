-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The policy model (D-45): roles hold permissions, assignments bind a role to a
-- principal. A NULL env_id means every environment. Phase 3 adds teams,
-- Operator and Viewer roles and per-environment assignments on these tables.
-- Permission names are validated against the Go registry, not by a foreign key,
-- so the registry can add a permission without a migration.
CREATE TABLE roles (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL UNIQUE,
    builtin INTEGER NOT NULL
);

CREATE TABLE role_permissions (
    role_id    TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    permission TEXT NOT NULL,
    PRIMARY KEY (role_id, permission)
);

CREATE TABLE role_assignments (
    id             TEXT PRIMARY KEY,
    principal_type TEXT NOT NULL,
    principal_id   TEXT NOT NULL,
    role_id        TEXT NOT NULL REFERENCES roles (id) ON DELETE CASCADE,
    env_id         TEXT,
    created_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX role_assignments_unique
    ON role_assignments (principal_type, principal_id, role_id, COALESCE(env_id, ''));
CREATE INDEX role_assignments_principal
    ON role_assignments (principal_type, principal_id);
