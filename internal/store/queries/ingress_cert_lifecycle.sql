/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: RepointIngressCert :exec
-- Hands the live row of a hostname to the environment and domain its claim resolves to (D-04). Any stop the row
-- carried (its domain was removed, nothing referred to it) and the time it became unreferenced clear, and the row
-- waits for its renewal (a chain) or for its first order (none). A row that is ordering is never re-pointed: the
-- caller skips it.
UPDATE ingress_certs SET
    env_id = ?, domain_id = ?, stopped_reason = '', unreferenced_since = 0,
    phase = CASE WHEN chain_pem <> '' THEN 'idle' ELSE 'queued' END,
    updated_at = ?
WHERE id = ? AND retired_at = 0;

-- name: SetIngressCertUnreferenced :exec
-- Records since when nothing refers to the hostname.
UPDATE ingress_certs SET unreferenced_since = ?, updated_at = ? WHERE id = ? AND retired_at = 0;

-- name: ClearIngressCertUnreferenced :exec
-- The hostname is referred to again: the time it became unreferenced clears, and a row that was stopped for being
-- unreferenced goes back to waiting for its renewal (a chain) or its first order (none).
UPDATE ingress_certs SET
    unreferenced_since = 0,
    phase = CASE WHEN stopped_reason = 'unreferenced' THEN (CASE WHEN chain_pem <> '' THEN 'idle' ELSE 'queued' END) ELSE phase END,
    stopped_reason = CASE WHEN stopped_reason = 'unreferenced' THEN '' ELSE stopped_reason END,
    updated_at = ?
WHERE id = ? AND retired_at = 0;

-- name: SetIngressCertRequested :exec
-- An administrator asked for the certificate ahead of any claim: the request counts as a reference for Grace from
-- now, so the time it was unreferenced clears and a row that was stopped for being unreferenced is brought back
-- (D-08).
UPDATE ingress_certs SET
    requested_at = ?, requested_by = ?, unreferenced_since = 0,
    phase = CASE WHEN stopped_reason = 'unreferenced' THEN (CASE WHEN chain_pem <> '' THEN 'idle' ELSE 'queued' END) ELSE phase END,
    stopped_reason = CASE WHEN stopped_reason = 'unreferenced' THEN '' ELSE stopped_reason END,
    updated_at = ?
WHERE id = ? AND retired_at = 0;

-- name: DeleteIngressCert :execrows
-- Deletes a row by its id. Its orders and their sealed keys go with it (CASCADE). The rows affected say whether
-- it was still there.
DELETE FROM ingress_certs WHERE id = ?;

-- name: ListRetiredIngressCerts :many
-- Every retired row of the install: they stopped being the live certificate of their hostname and are kept only
-- while they still serve.
SELECT * FROM ingress_certs WHERE retired_at <> 0 ORDER BY hostname, retired_at, id;

-- name: MoveIngressCertsToEnv :execrows
-- The fold of a Local environment moves every certificate row of that environment, live or retired and whatever
-- its phase, to the swarm environment, because the 00045 cascade would otherwise delete them with the Local row.
-- Certificates are keyed by hostname for the whole install (Phase 20 D-01) and a key is sealed under the row's own
-- id, so nothing is re-sealed; domain_id, phase and every other column stay as they are. The rows affected are how
-- many moved.
UPDATE ingress_certs SET
    env_id = sqlc.arg(to_env_id),
    updated_at = sqlc.arg(updated_at)
WHERE env_id = sqlc.arg(from_env_id);
