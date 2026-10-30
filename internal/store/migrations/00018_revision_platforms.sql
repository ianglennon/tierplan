-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The Coaming Authors

-- +goose Up
-- The swarm platforms each service of a revision was deployed with, a JSON object by swarm service name
-- (service -> list of {Architecture, OS}). A roll back sends them again without asking a registry (Phase 8,
-- G-08-4, STK-13). A revision stored before this migration reads '{}': it has none, and nothing about it changes.
ALTER TABLE stack_snapshots ADD COLUMN platforms TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE stack_snapshots DROP COLUMN platforms;
