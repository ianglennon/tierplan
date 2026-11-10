/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetACMEAccount :one
SELECT * FROM acme_account WHERE directory_url = ?;

-- name: InsertACMEAccount :exec
INSERT INTO acme_account (directory_url, account_url, key_sealed, contact, tos_url, created_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: DeleteACMEAccount :exec
DELETE FROM acme_account WHERE directory_url = ?;

-- name: GetACMEState :one
SELECT * FROM acme_state WHERE id = 1;

-- name: UpdateACMEState :exec
UPDATE acme_state SET
    mode = ?, challenge_type = ?, profile = ?, paused_reason = ?, consecutive_failures = ?,
    last_attempt_at = ?, next_attempt_at = ?, retry_after_until = ?, renew_at = ?,
    ari_window_start = ?, ari_window_end = ?, ari_checked_at = ?, ari_next_check_at = ?,
    last_error_at = ?, last_success_at = ?, renew_basis = ?, last_error_class = ?,
    last_error_detail = ?, last_success_kind = ?, updated_at = ?
WHERE id = 1;

-- name: InsertACMEOrder :exec
INSERT INTO acme_orders (id, directory_url, order_url, kind, names, profile, replaces, cert_key_sealed,
                         stage, status, resume_count, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?);

-- name: GetOpenACMEOrder :one
-- The newest open order. Only one is ever open; the order makes a leftover harmless.
SELECT * FROM acme_orders WHERE status = 'open' ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: UpdateACMEOrderStage :exec
UPDATE acme_orders SET stage = ?, resume_count = ?, updated_at = ? WHERE id = ?;

-- name: CloseACMEOrder :exec
UPDATE acme_orders SET status = 'closed', stage = ?, updated_at = ? WHERE id = ?;

-- name: InsertACMERun :exec
INSERT INTO acme_runs (id, kind, trigger, actor, names, started_at, finished_at, outcome, error_class)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: FinishACMERun :exec
UPDATE acme_runs SET outcome = ?, error_class = ?, finished_at = ? WHERE id = ?;

-- name: ListACMERuns :many
-- Newest first; the same started_at is ordered by id.
SELECT * FROM acme_runs ORDER BY started_at DESC, id DESC LIMIT ?;

-- name: ListACMEFailedRunsSince :many
-- The start times of the failed runs at or after a time, oldest first.
SELECT started_at FROM acme_runs WHERE outcome = 'failed' AND started_at >= ? ORDER BY started_at ASC;

-- name: PruneACMERuns :exec
-- Keeps the newest 50 runs.
DELETE FROM acme_runs WHERE id NOT IN (
    SELECT id FROM acme_runs ORDER BY started_at DESC, id DESC LIMIT 50
);

-- name: InterruptACMERuns :exec
-- A run still marked running when the process starts was cut short by a stop or a crash.
UPDATE acme_runs SET outcome = 'failed', error_class = 'internal', finished_at = ? WHERE outcome = 'running';
