-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- A stack someone excluded from the janitor (Phase 11, D-10): 1 means everything the stack owns is left
-- alone by every cleanup, scheduled or by Clean now, until it is included again. The other way to exclude a
-- stack is x-tierplan: {keep: true} in its compose file, which is read from the file and never stored here.
ALTER TABLE stacks ADD COLUMN janitor_excluded INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE stacks DROP COLUMN janitor_excluded;
