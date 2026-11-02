-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SPDX-FileCopyrightText: 2026 The TierPlan Authors

-- +goose Up
-- The subnets a swarm deploy gave the networks it created for a revision under the environment's small stack subnet
-- setting, a JSON object by network name (network -> CIDR). A deploy or roll back that has to create one of those
-- networks again uses the recorded subnet while it is still free (Phase 10, D-20, NET-06). A revision stored before
-- this migration reads '{}': it recorded none, and nothing about it changes.
ALTER TABLE stack_snapshots ADD COLUMN subnets TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE stack_snapshots DROP COLUMN subnets;
