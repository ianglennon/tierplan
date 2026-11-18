/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: UpdateIngressCertRenewal :exec
-- The renewal clock of one certificate: when it is due, what that was worked out from, and the certificate
-- authority's renewal window with the time it is asked again. All zero for a certificate that has no clock yet.
UPDATE ingress_certs SET
    renew_at = ?, renew_basis = ?, ari_window_start = ?, ari_window_end = ?, ari_checked_at = ?,
    ari_next_check_at = ?, updated_at = ?
WHERE id = ?;

-- name: SetIngressCertReady :exec
-- The stored readiness of a certificate: 1 when it may be served now (D-20, D-41).
UPDATE ingress_certs SET ready = ?, updated_at = ? WHERE id = ?;

-- name: ListRenewalDueIngressCerts :many
-- The live ACME certificates that hold a chain and wait for their renewal, and whose clock needs a look: it has
-- none yet, the renewal window is to be asked again, or the renewal is due. Oldest first.
SELECT * FROM ingress_certs
WHERE retired_at = 0 AND source = 'acme' AND phase = 'idle' AND chain_pem <> '' AND stopped_reason = ''
  AND (renew_at = 0 OR ari_next_check_at <= sqlc.arg(now) OR renew_at <= sqlc.arg(now))
ORDER BY created_at, id;

-- name: MarkIngressCertRenewalDue :execrows
-- A certificate that is due moves from idle to queued with its chain kept, so it is still served while the
-- renewal is ordered. The rows affected say whether the row was still idle.
UPDATE ingress_certs SET phase = 'queued', updated_at = ?
WHERE id = ? AND phase = 'idle' AND retired_at = 0 AND source = 'acme' AND chain_pem <> '';

-- name: ListReadyCandidateIngressCerts :many
-- Every row, live or retired, whose readiness can change by time alone: it holds a chain and is either ready now
-- or not yet expired.
SELECT * FROM ingress_certs
WHERE chain_pem <> '' AND (ready = 1 OR not_after > sqlc.arg(now))
ORDER BY env_id, hostname, id;
