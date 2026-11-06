-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- What a person asked TierPlan to stop offering image updates for (Phase 12, D-15, D-16). A skip passes over
-- one remote version of one workload; an ignore hides every update of an image or of a workload. Nothing
-- is ignored across environments: every row belongs to one environment and goes with it. Timestamps are
-- UTC unix nanoseconds.
--   id      the row's own ID.
--   env_id  the environment the row applies to.
--   mode    skip (one remote version) or ignore (all updates).
--   scope   image (a repository and tag) or workload (one service or container). A skip is always scope
--           workload.
--   host, repo, tag
--           the image of an image-scoped row, as the registry check keys it; empty for a workload scope.
--   kind, name
--           the workload of a workload-scoped row: service or container, and its name; empty for an image
--           scope.
--   digest  the remote digest a skip passes over, so it stops applying when the tag points at another one;
--           empty for an ignore.
--   set_by  the user's ID; set_at when.
CREATE TABLE image_update_ignores (
    id     TEXT PRIMARY KEY,
    env_id TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    mode   TEXT NOT NULL CHECK (mode IN ('skip', 'ignore')),
    scope  TEXT NOT NULL CHECK (scope IN ('image', 'workload')),
    host   TEXT NOT NULL DEFAULT '',
    repo   TEXT NOT NULL DEFAULT '',
    tag    TEXT NOT NULL DEFAULT '',
    kind   TEXT NOT NULL DEFAULT '' CHECK (kind IN ('', 'service', 'container')),
    name   TEXT NOT NULL DEFAULT '',
    digest TEXT NOT NULL DEFAULT '',
    set_by TEXT NOT NULL,
    set_at INTEGER NOT NULL,
    UNIQUE (env_id, mode, scope, host, repo, tag, kind, name)
);

-- +goose Down
DROP TABLE image_update_ignores;
