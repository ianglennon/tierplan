/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetImageUpdateDefaults :one
-- The instance defaults of the update checks: the one row the migration seeds.
SELECT enabled, schedule, schedule_preset, version, updated_by, updated_at
FROM image_update_defaults WHERE id = 1;

-- name: GetImageUpdateEnvPolicy :one
-- One environment's overrides; no row means it follows every default.
SELECT env_id, enabled, schedule, schedule_preset, version, updated_by, updated_at
FROM image_update_env_policy WHERE env_id = ?;

-- name: InsertImageUpdateEnvPolicy :execrows
-- The first save of an environment's overrides (NULL for every field it follows), at version 1. Zero
-- rows means the environment already has a row, so the save was made from a stale read.
INSERT INTO image_update_env_policy (env_id, enabled, schedule, schedule_preset, version, updated_by, updated_at)
VALUES (?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (env_id) DO NOTHING;

-- name: UpdateImageUpdateEnvPolicy :execrows
-- Saves an environment's overrides only when the version is the one the caller read; zero rows means it
-- was not.
UPDATE image_update_env_policy
SET enabled = ?, schedule = ?, schedule_preset = ?,
    version = version + 1, updated_by = ?, updated_at = ?
WHERE env_id = ? AND version = ?;

-- name: UpdateImageUpdateDefaults :execrows
-- Saves the instance defaults only when the version is the one the caller read; zero rows means it was
-- not. The row is seeded by the migration, so there is always one.
UPDATE image_update_defaults
SET enabled = ?, schedule = ?, schedule_preset = ?,
    version = version + 1, updated_by = ?, updated_at = ?
WHERE id = 1 AND version = ?;

-- name: CountImageUpdateFollowers :one
-- How many environments follow at least one instance default: those with no policy row and those whose
-- row leaves a field NULL (the schedule and its preset are set together, so the schedule stands for both).
SELECT COUNT(*) FROM environments e
LEFT JOIN image_update_env_policy p ON p.env_id = e.id
WHERE p.env_id IS NULL OR p.enabled IS NULL OR p.schedule IS NULL;
