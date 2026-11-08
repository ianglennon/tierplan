/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: GetTLSCert :one
SELECT * FROM tls_certs WHERE id = ?;

-- name: InsertTLSCert :exec
INSERT INTO tls_certs (id, cert_pem, key_sealed, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);

-- name: UpdateTLSCert :exec
UPDATE tls_certs SET cert_pem = ?, updated_at = ? WHERE id = ?;
