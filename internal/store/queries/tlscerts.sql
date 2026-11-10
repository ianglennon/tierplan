/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetTLSCert :one
SELECT * FROM tls_certs WHERE id = ?;

-- name: InsertTLSCert :exec
INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);

-- name: UpdateTLSCert :exec
UPDATE tls_certs SET cert_pem = ?, updated_at = ? WHERE id = ?;

-- name: UpsertTLSCert :exec
-- Writes the certificate and sealed key of a row that may or may not exist yet (the ACME
-- certificate is replaced at every renewal). created_at keeps the first value.
INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET cert_pem = excluded.cert_pem, key_sealed = excluded.key_sealed,
                               updated_at = excluded.updated_at;
