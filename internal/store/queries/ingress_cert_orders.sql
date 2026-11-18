/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: InsertIngressCertOrder :exec
-- A new open order of a certificate row, with its certificate key sealed under ingress-order-key/v1/<id>.
INSERT INTO ingress_cert_orders (id, cert_id, directory_url, order_url, kind, names, profile, replaces, account_url,
                                 cert_key_sealed, stage, status, resume_count, expires_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?);

-- name: GetOpenIngressCertOrder :one
-- The newest open order of one certificate. Only one is ever open; the order makes a leftover harmless.
SELECT * FROM ingress_cert_orders WHERE cert_id = ? AND status = 'open' ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: GetOpenIngressCertOrderID :one
SELECT id FROM ingress_cert_orders WHERE cert_id = ? AND status = 'open' ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: GetIngressCertOrder :one
SELECT * FROM ingress_cert_orders WHERE id = ?;

-- name: UpdateIngressCertOrder :exec
UPDATE ingress_cert_orders SET stage = ?, resume_count = ?, updated_at = ? WHERE id = ?;

-- name: CloseIngressCertOrder :exec
UPDATE ingress_cert_orders SET status = 'closed', stage = ?, updated_at = ? WHERE id = ?;

-- name: PruneClosedIngressCertOrders :execrows
-- Deletes the closed orders last touched before a cutoff. An open order is never deleted here.
DELETE FROM ingress_cert_orders WHERE status = 'closed' AND updated_at < ?;

-- name: InsertIngressCertIssuance :exec
-- One issued certificate in the ledger the rate-limit budget is counted from. Written in the transaction that
-- stores the certificate.
INSERT INTO ingress_cert_issuances (id, issuer, registered_domain, hostname, kind, issued_at)
VALUES (?, ?, ?, ?, ?, ?);
