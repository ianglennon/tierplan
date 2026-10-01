/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The Bayplan Authors */

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: GetUserByUsername :one
-- Usernames are stored lowercase; callers lower-case before looking up (D-35).
SELECT * FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: InsertUser :exec
INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at)
VALUES (?, ?, ?, ?, ?, ?);
