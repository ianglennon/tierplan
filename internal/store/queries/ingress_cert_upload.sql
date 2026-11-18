/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: DeleteReplacedIngressCert :one
-- Deletes the live certificate row an upload replaces, by its id (never by hostname: the row was read inside
-- the same write, and a hostname key could take a row that is not the one decided on). Its orders and their
-- sealed keys go with it (CASCADE). It returns the source of the row it deleted, and no row when the id is
-- not a live row.
DELETE FROM ingress_certs WHERE id = ? AND retired_at = 0 RETURNING source;

-- name: InsertIngressCertUploaded :exec
-- A live row for a certificate an administrator uploaded: source upload, no issuer, idle. The chain and the
-- sealed key are written next by UpdateIngressCertIssued, in the same transaction.
INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, issuer, phase, ready, created_by, created_at, updated_at)
VALUES (?, ?, ?, ?, 'upload', '', 'idle', ?, ?, ?, ?);

-- name: RetireIngressCert :exec
-- Retires a live row: it stops being the hostname's live certificate but keeps serving until it expires or
-- its replacement is ready. Its orders are stopped with it.
UPDATE ingress_certs SET retired_at = ?, retired_reason = ?, phase = 'stopped', updated_at = ?
WHERE id = ? AND retired_at = 0;
