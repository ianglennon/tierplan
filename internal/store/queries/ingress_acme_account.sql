/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: UpsertIngressACMEAccount :exec
-- The per-hostname manager's account for one directory. The own account lives in acme_account and is never
-- read or written here.
INSERT INTO ingress_acme_account (directory_url, account_url, key_sealed, contact, tos_url, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (directory_url) DO UPDATE SET
    account_url = excluded.account_url, key_sealed = excluded.key_sealed, contact = excluded.contact,
    tos_url = excluded.tos_url, created_at = excluded.created_at;

-- name: GetIngressACMEAccount :one
SELECT * FROM ingress_acme_account WHERE directory_url = ?;

-- name: DeleteIngressACMEAccount :exec
DELETE FROM ingress_acme_account WHERE directory_url = ?;
