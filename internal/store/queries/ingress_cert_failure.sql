/* SPDX-License-Identifier: AGPL-3.0-or-later
   SPDX-FileCopyrightText: 2026 The TierPlan Authors */

-- name: RecordIngressCertFailure :exec
-- The record of a failed run: the class, the capped detail for the Admin API, the ledger (failures, last and next
-- attempt, the CA's Retry-After) and the newest failure instants. The row goes back to queued to wait for its next
-- attempt and its chain, if it has one, is not touched: a failed renewal keeps the working certificate. Only a row
-- that is ordering is written, so a row that was stopped or retired while its run was failing stays that way.
UPDATE ingress_certs SET
    last_error_class = ?, last_error_at = ?, last_error_detail = ?, consecutive_failures = ?, last_attempt_at = ?,
    next_attempt_at = ?, retry_after_until = ?, failure_times = ?, phase = 'queued', updated_at = ?
WHERE id = ? AND phase = 'ordering' AND retired_at = 0;

-- name: HoldIngressCertsOfDomain :exec
-- After a failure that is the domain's token (refused, out of scope, zone not found) every other live row that is
-- queued or waits for its renewal under the same domain is held until the same next attempt, so one bad token does
-- not spend an order for every hostname. A later time already in force is kept, and a row whose run is in flight
-- is left to record its own result.
UPDATE ingress_certs SET
    next_attempt_at = MAX(next_attempt_at, CAST(sqlc.arg(until) AS INTEGER)), updated_at = sqlc.arg(updated_at)
WHERE domain_id = sqlc.arg(domain_id) AND retired_at = 0 AND source = 'acme' AND phase IN ('queued', 'idle');

-- name: RetryIngressCertNow :execrows
-- An Admin's retry now: the row waits for the next round with nothing in front of it, as an order that is due. A
-- certificate that holds a chain (an idle one included) keeps it. The rows affected say whether the row was still
-- one that can be queued.
UPDATE ingress_certs SET next_attempt_at = ?, phase = 'queued', updated_at = ?
WHERE id = ? AND phase IN ('queued', 'idle') AND retired_at = 0 AND source = 'acme';
