/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: UpsertACMEDNSCredential :exec
INSERT INTO acme_dns_credentials (provider, token_sealed, created_at) VALUES (?, ?, ?)
ON CONFLICT (provider) DO UPDATE SET token_sealed = excluded.token_sealed, created_at = excluded.created_at;

-- name: GetACMEDNSCredential :one
SELECT * FROM acme_dns_credentials WHERE provider = ?;

-- name: DeleteACMEDNSCredential :exec
DELETE FROM acme_dns_credentials WHERE provider = ?;

-- name: CountACMEDNSCredential :one
-- Whether a token is saved, without opening it.
SELECT COUNT(*) FROM acme_dns_credentials WHERE provider = ?;

-- name: InsertACMEDNSRecord :exec
INSERT INTO acme_dns_records (id, provider, zone_id, zone_name, account_id, record_id, name, value, comment,
                              owner_kind, owner_id, token, created_at, attempts, last_try_at, next_try_at,
                              last_error_class, credential_ref)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListACMEDNSRecordsByOwner :many
-- The rows of one owner (an order or a test) for one ACME challenge token, oldest first. Own rows only:
-- a row made for a hostname certificate carries a credential_ref and is never this manager's.
SELECT * FROM acme_dns_records WHERE credential_ref = '' AND owner_kind = ? AND owner_id = ? AND token = ? ORDER BY created_at, id;

-- name: ListACMEDNSRecordsByOwnerAndCredential :many
-- One credential's rows of one owner for one ACME challenge token, oldest first.
SELECT * FROM acme_dns_records WHERE credential_ref = ? AND owner_kind = ? AND owner_id = ? AND token = ? ORDER BY created_at, id;

-- name: GetPendingACMEDNSRecord :one
-- A row of this credential that already holds this exact record at the provider, newest first, so a
-- resumed order reuses it. The own manager passes the empty credential_ref.
SELECT * FROM acme_dns_records WHERE credential_ref = ? AND provider = ? AND zone_id = ? AND name = ? AND value = ?
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: RestampACMEDNSRecord :exec
-- Hands a row to a new owner and token, and clears its retry state: it is in use again.
UPDATE acme_dns_records SET owner_kind = ?, owner_id = ?, token = ?, attempts = 0, last_try_at = 0,
    next_try_at = 0, last_error_class = ''
WHERE id = ?;

-- name: DeleteACMEDNSRecord :exec
DELETE FROM acme_dns_records WHERE id = ?;

-- name: MarkACMEDNSRecordFailed :exec
UPDATE acme_dns_records SET attempts = ?, last_try_at = ?, next_try_at = ?, last_error_class = ? WHERE id = ?;

-- name: ListDueACMEDNSRecords :many
-- The own rows whose next try has come, oldest first.
SELECT * FROM acme_dns_records WHERE credential_ref = '' AND next_try_at <= ? ORDER BY created_at, id;

-- name: ListACMEDNSRecords :many
-- The own rows (credential_ref = ''), oldest first.
SELECT * FROM acme_dns_records WHERE credential_ref = '' ORDER BY created_at, id;

-- name: ListDueIngressDNSRecords :many
-- The hostname-certificate rows (a credential_ref is set) whose next try has come, oldest first.
SELECT * FROM acme_dns_records WHERE credential_ref <> '' AND next_try_at <= ? ORDER BY created_at, id;

-- name: ListIngressDNSRecords :many
-- Every hostname-certificate row, oldest first.
SELECT * FROM acme_dns_records WHERE credential_ref <> '' ORDER BY created_at, id;

-- name: CountACMEDNSRecordsByCredential :one
-- How many ledger rows one credential still holds.
SELECT COUNT(*) FROM acme_dns_records WHERE credential_ref = ?;

-- name: SetACMEValidation :exec
-- The only statement that writes the challenge type, the provider, the resolvers and the untested
-- flag; the state row's full update never does, so a stale read cannot undo a saved choice.
UPDATE acme_state SET challenge_type = ?, dns_provider = ?, dns_resolvers = ?, validation_untested = ?, updated_at = ?
WHERE id = 1;

-- name: SetACMEValidationUntested :exec
UPDATE acme_state SET validation_untested = ?, updated_at = ? WHERE id = 1;

-- name: ResetACMEDNSFailures :exec
-- Clears the backoff a Cloudflare-side failure put in force, and only that: a CA-side failure (any
-- other class) keeps its backoff. GLOB, not LIKE: the underscore is literal in a GLOB pattern, and sqlc's
-- SQLite parser does not accept LIKE ... ESCAPE in an UPDATE.
UPDATE acme_state SET consecutive_failures = 0, next_attempt_at = 0, retry_after_until = 0, updated_at = ?
WHERE id = 1 AND last_error_class GLOB 'dns_*';

-- name: CloseOpenACMEOrders :exec
-- Closes every open order: a changed validation setting must not resume an order made under the old one.
UPDATE acme_orders SET status = 'closed', stage = ?, updated_at = ? WHERE status = 'open';

-- name: GetOpenACMEOrderID :one
-- The newest open order's id, without opening its sealed key.
SELECT id FROM acme_orders WHERE status = 'open' ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: InsertACMEInstallID :exec
-- Mints the install id once: a second insert changes nothing.
INSERT INTO app_meta (key, value, updated_at) VALUES ('acme_install_id', ?, ?) ON CONFLICT (key) DO NOTHING;

-- name: GetACMEInstallID :one
SELECT value FROM app_meta WHERE key = 'acme_install_id';
