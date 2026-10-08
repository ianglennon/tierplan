-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Bayplan Authors

-- +goose Up
-- The Phase 3 schema in one migration: team access, safe admin and backups.
--
--   D-01   a per-user instance_admin flag holds instance authority; the old global
--          admin assignment becomes that flag and the assignment row goes.
--   D-05   an assignment with a NULL env_id applies to every environment.
--   D-06   a user's access is the union of direct, team and all-environment grants.
--   D-14   one-time invite and reset links (user_links).
--   D-15   dangerous extras ride on an assignment (assignment_extras).
--   D-22   users are tombstoned, never deleted: deleted_at keeps the row, the
--          username and every audit reference.
--   D-23   sessions get a public id the UI can show and revoke by, without the
--          secret hash.
--   D-32   resources owned by a user (owned_resources) pause when the owner loses
--          the permission that created them.
--   D-28   the backup schedule and its run history.
--
-- The registry in Go, not the database, defines what each role may do (D-15b), so
-- role_permissions is retired and the roles table holds only the three role ids.
-- Timestamps are UTC unix nanoseconds, IDs are UUIDv7 strings.

-- Users: the instance flag, profile fields, the tombstone and the last sign-in.
ALTER TABLE users ADD COLUMN instance_admin INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN deleted_at INTEGER;
ALTER TABLE users ADD COLUMN last_login_at INTEGER;

-- Every user who held the global admin assignment becomes an instance admin; the
-- assignment is then redundant and is removed. An environment-scoped admin row
-- (env_id set) is a different thing and stays.
UPDATE users SET instance_admin = 1
 WHERE id IN (SELECT principal_id FROM role_assignments
               WHERE principal_type = 'user' AND role_id = 'admin' AND env_id IS NULL);
DELETE FROM role_assignments WHERE role_id = 'admin' AND env_id IS NULL;

-- The three fixed roles. The admin row exists on installs that have started
-- before; a fresh database has none yet.
INSERT INTO roles (id, name, builtin) VALUES ('admin', 'admin', 1) ON CONFLICT (id) DO NOTHING;
INSERT INTO roles (id, name, builtin) VALUES ('operator', 'operator', 1) ON CONFLICT (id) DO NOTHING;
INSERT INTO roles (id, name, builtin) VALUES ('viewer', 'viewer', 1) ON CONFLICT (id) DO NOTHING;
DROP TABLE role_permissions;

-- One assignment per subject and scope: a subject holds one role on one
-- environment (or on all of them), never two.
DROP INDEX role_assignments_unique;
CREATE UNIQUE INDEX role_assignments_subject_scope
    ON role_assignments (principal_type, principal_id, COALESCE(env_id, ''));

-- Dangerous permissions an Operator assignment carries on top of its role.
CREATE TABLE assignment_extras (
    assignment_id TEXT NOT NULL REFERENCES role_assignments (id) ON DELETE CASCADE,
    permission    TEXT NOT NULL,
    PRIMARY KEY (assignment_id, permission)
);

-- Teams are a second kind of principal: an assignment with principal_type 'team'
-- reaches every member.
CREATE TABLE teams (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX teams_name ON teams (lower(name));

CREATE TABLE team_members (
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_members_user ON team_members (user_id);

-- One live invite or reset link per user; only the hash of the token is stored.
CREATE TABLE user_links (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('invite', 'reset')),
    token_hash BLOB NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    created_by TEXT NOT NULL
);
CREATE UNIQUE INDEX user_links_user ON user_links (user_id);

-- A session's public id: how the UI names a session to revoke without ever
-- seeing the hash that is its primary key.
ALTER TABLE sessions ADD COLUMN public_id TEXT;
UPDATE sessions SET public_id = lower(hex(randomblob(16))) WHERE public_id IS NULL;
CREATE UNIQUE INDEX sessions_public_id ON sessions (public_id);

-- Resources a user created that keep running on their authority (webhooks, git
-- syncs and the like). They pause when the owner loses the permission.
CREATE TABLE owned_resources (
    kind            TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    env_id          TEXT NOT NULL,
    name            TEXT NOT NULL,
    owner_id        TEXT NOT NULL REFERENCES users (id),
    state           TEXT NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'paused_lacks_permission', 'paused_disabled', 'paused_deleted')),
    needs_env_admin INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (kind, resource_id)
);
CREATE INDEX owned_resources_owner ON owned_resources (owner_id);

-- The scheduled backup: a single row (id is always 1).
CREATE TABLE backup_schedule (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    enabled           INTEGER NOT NULL DEFAULT 0,
    frequency         TEXT NOT NULL DEFAULT 'daily' CHECK (frequency IN ('daily', 'weekly')),
    weekday           INTEGER NOT NULL DEFAULT 0 CHECK (weekday BETWEEN 0 AND 6),
    time_utc          TEXT NOT NULL DEFAULT '03:00',
    keep              INTEGER NOT NULL DEFAULT 7 CHECK (keep BETWEEN 1 AND 365),
    passphrase_sealed TEXT NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL DEFAULT 0,
    updated_by        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE backup_runs (
    id          TEXT PRIMARY KEY,
    source      TEXT NOT NULL CHECK (source IN ('schedule', 'cli', 'ui')),
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    ok          INTEGER NOT NULL DEFAULT 0,
    file_name   TEXT NOT NULL DEFAULT '',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    error       TEXT NOT NULL DEFAULT ''
);

-- The audit log is read per environment by environment admins.
CREATE INDEX audit_log_env ON audit_log (env_id);
