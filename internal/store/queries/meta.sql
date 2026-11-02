/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetMeta :one
SELECT value FROM app_meta WHERE key = ?;

-- name: UpsertMeta :exec
INSERT INTO app_meta (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;

-- name: GetSetting :one
SELECT value FROM settings WHERE key = ?;

-- name: UpsertSetting :exec
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = ?;
