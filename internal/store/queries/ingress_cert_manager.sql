/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: ResetIngressCertOrdering :execrows
-- A process that stopped mid-order left its rows in phase ordering, and nothing is ordering now: they go back to
-- queued, so a crash never holds a reservation. The open order is kept and the next run resumes it.
UPDATE ingress_certs SET phase = 'queued', updated_at = ? WHERE phase = 'ordering';

-- name: ListOrderingIngressCertIDs :many
-- The rows reserved for a run. The scheduler gives back the reservation of any that has no run in this process.
SELECT id FROM ingress_certs WHERE phase = 'ordering' ORDER BY id;

-- name: ListQueuedIngressCerts :many
-- The live ACME rows that wait for an order and whose turn has come: neither the soft backoff nor a CA's
-- Retry-After is still in the future. Oldest first.
SELECT * FROM ingress_certs
WHERE phase = 'queued' AND retired_at = 0 AND source = 'acme'
  AND next_attempt_at <= sqlc.arg(now) AND retry_after_until <= sqlc.arg(now)
ORDER BY created_at, id;

-- name: MarkIngressCertOrdering :execrows
-- Reserves a queued row for a run. The rows affected say who won: 0 means another caller took it, or the row
-- was retired, replaced or removed since it was listed. A row that is ordering waits for nothing, so its queue
-- reason clears.
UPDATE ingress_certs SET phase = 'ordering', last_attempt_at = ?, updated_at = ?,
    queue_code = '', queue_used = 0, queue_cap = 0, queue_domain = '', queue_next_slot_at = 0
WHERE id = ? AND phase = 'queued' AND retired_at = 0 AND source = 'acme';

-- name: ReleaseIngressCertOrdering :exec
-- Gives the reservation of a run that ended without a result back (a person cancelled the job).
UPDATE ingress_certs SET phase = 'queued', updated_at = ? WHERE id = ? AND phase = 'ordering';

-- name: SetIngressCertQueue :exec
-- Puts a row in phase queued with the reason it waits: the code, and for a rate-limit budget how much of which cap
-- is used and when a slot opens.
UPDATE ingress_certs SET
    phase = 'queued', queue_code = ?, queue_used = ?, queue_cap = ?, queue_domain = ?, queue_next_slot_at = ?,
    updated_at = ?
WHERE id = ? AND retired_at = 0;

-- name: StopIngressCert :exec
-- Stops a row until a person (or a claim that resolves to a live domain) acts; stopped_reason says why.
UPDATE ingress_certs SET phase = 'stopped', stopped_reason = ?, updated_at = ? WHERE id = ? AND retired_at = 0;
