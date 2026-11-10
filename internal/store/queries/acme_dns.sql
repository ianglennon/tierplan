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
