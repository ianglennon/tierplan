-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- What a snapshot of a git-backed stack was made from (Phase 9, D-08). A draft and every revision record the
-- commit and the tree hash of the folder it holds, where that came from (repository ID and name, ref kind, ref,
-- folder and compose files as a JSON object) and the ordered compose files, so a revision restores without the
-- mirror and survives a force-push. A snapshot of an inline or adopted stack keeps the defaults.
ALTER TABLE stack_snapshots ADD COLUMN git_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE stack_snapshots ADD COLUMN git_tree TEXT NOT NULL DEFAULT '';
ALTER TABLE stack_snapshots ADD COLUMN git_source TEXT NOT NULL DEFAULT '{}';
ALTER TABLE stack_snapshots ADD COLUMN compose_files TEXT NOT NULL DEFAULT '[]';

-- What started the deploy of a revision that no person started: a poll or a webhook. It is its own column
-- because the CHECK on origin (00017) cannot be altered.
ALTER TABLE stack_snapshots ADD COLUMN sync_trigger TEXT NOT NULL DEFAULT ''
    CHECK (sync_trigger IN ('', 'poll', 'webhook'));

-- Where a stack file came from: '' typed or uploaded into Coaming, repo from the repository, overlay a file
-- Coaming holds for a path the repository does not have.
ALTER TABLE stack_snapshot_files ADD COLUMN origin TEXT NOT NULL DEFAULT ''
    CHECK (origin IN ('', 'repo', 'overlay'));

-- +goose Down
ALTER TABLE stack_snapshot_files DROP COLUMN origin;
ALTER TABLE stack_snapshots DROP COLUMN sync_trigger;
ALTER TABLE stack_snapshots DROP COLUMN compose_files;
ALTER TABLE stack_snapshots DROP COLUMN git_source;
ALTER TABLE stack_snapshots DROP COLUMN git_tree;
ALTER TABLE stack_snapshots DROP COLUMN git_commit;
