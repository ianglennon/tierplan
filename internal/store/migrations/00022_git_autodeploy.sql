-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- The lines a deploy of a git stack put back to what the repository says, as a JSON array of strings: when an
-- automatic deploy runs over something that was changed by hand on the host (Phase 9, D-21). A revision that is not an
-- automatic deploy keeps the empty list.
ALTER TABLE stack_snapshots ADD COLUMN reverted_drift TEXT NOT NULL DEFAULT '[]';

-- The last thing an automatic deploy did that a person should hear about (Phase 9, D-19): a deploy that was held back,
-- one that put changes made by hand back, or an automation that was paused. Plans that show it read it; a good deploy
-- clears it.
ALTER TABLE stack_git_state ADD COLUMN event_kind TEXT NOT NULL DEFAULT ''
    CHECK (event_kind IN ('', 'autodeploy_skipped', 'autodeploy_reverted', 'autodeploy_paused'));
ALTER TABLE stack_git_state ADD COLUMN event_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE stack_git_state ADD COLUMN event_at INTEGER;

-- +goose Down
ALTER TABLE stack_git_state DROP COLUMN event_at;
ALTER TABLE stack_git_state DROP COLUMN event_detail;
ALTER TABLE stack_git_state DROP COLUMN event_kind;
ALTER TABLE stack_snapshots DROP COLUMN reverted_drift;
