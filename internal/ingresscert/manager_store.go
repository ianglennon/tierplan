// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The store calls of the certificate manager. None of them changes what is served, so none raises
// certs_version or tells the notifier: the manager must not wake itself with its own bookkeeping.

// ResetOrdering puts every row that is ordering back to queued and returns how many there were. It is run at
// start: nothing is ordering in a new process, and a row a crash left in that phase would hold its reservation
// for ever. The open order stays, and the next run resumes it.
func (s *Store) ResetOrdering(ctx context.Context) (int, error) {
	n, err := s.st.Q.ResetIngressCertOrdering(ctx, toNanos(s.now()))
	if err != nil {
		return 0, fmt.Errorf("ingresscert: reset the rows that were ordering: %w", err)
	}
	return int(n), nil
}

// DueQueued returns the live ACME rows in phase queued whose next attempt has come (neither the backoff nor a
// Retry-After is in the future at now), oldest first.
func (s *Store) DueQueued(ctx context.Context, now time.Time) ([]Cert, error) {
	rows, err := s.st.RQ.ListQueuedIngressCerts(ctx, toNanos(now))
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the queued certificates: %w", err)
	}
	return certsOf(rows)
}

// MarkOrdering reserves a queued row for a run. won is false when another caller took it first or the row
// is no longer a live, queued ACME row.
func (s *Store) MarkOrdering(ctx context.Context, id string) (won bool, err error) {
	ts := toNanos(s.now())
	n, err := s.st.Q.MarkIngressCertOrdering(ctx, sqlc.MarkIngressCertOrderingParams{LastAttemptAt: ts, UpdatedAt: ts, ID: id})
	if err != nil {
		return false, fmt.Errorf("ingresscert: reserve certificate %q for an order: %w", id, err)
	}
	return n == 1, nil
}

// ReleaseOrdering gives a reservation back: the row returns to queued if it is still ordering.
func (s *Store) ReleaseOrdering(ctx context.Context, id string) error {
	if err := s.st.Q.ReleaseIngressCertOrdering(ctx, sqlc.ReleaseIngressCertOrderingParams{UpdatedAt: toNanos(s.now()), ID: id}); err != nil {
		return fmt.Errorf("ingresscert: release the reservation of certificate %q: %w", id, err)
	}
	return nil
}

// QueueReason is why a queued row waits: a code, and for a rate-limit budget how much of which cap is used and
// when a slot opens.
type QueueReason struct {
	Code     string
	Used     int
	Cap      int
	Domain   string
	NextSlot time.Time
}

// SetQueue puts the row in phase queued and records why it waits.
func (s *Store) SetQueue(ctx context.Context, id string, r QueueReason) error {
	if err := s.st.Q.SetIngressCertQueue(ctx, sqlc.SetIngressCertQueueParams{
		QueueCode: r.Code, QueueUsed: int64(r.Used), QueueCap: int64(r.Cap), QueueDomain: r.Domain,
		QueueNextSlotAt: toNanos(r.NextSlot), UpdatedAt: toNanos(s.now()), ID: id,
	}); err != nil {
		return fmt.Errorf("ingresscert: queue certificate %q: %w", id, err)
	}
	return nil
}

// Stop stops the row until a person, or a claim that resolves to a live domain, acts. reason is the stored
// stopped_reason.
func (s *Store) Stop(ctx context.Context, id, reason string) error {
	if err := s.st.Q.StopIngressCert(ctx, sqlc.StopIngressCertParams{StoppedReason: reason, UpdatedAt: toNanos(s.now()), ID: id}); err != nil {
		return fmt.Errorf("ingresscert: stop certificate %q: %w", id, err)
	}
	return nil
}

// RecordFailure stores the ledger of a failed run on the row, which returns to queued to wait for its next
// attempt, and writes the ingress.cert_failed audit row as system:ingress in the same transaction. class is
// the failure class and the only thing the audit row says about why: a CA's text never reaches it.
func (s *Store) RecordFailure(ctx context.Context, c Cert, class string, l acme.Ledger, now time.Time) error {
	ts := toNanos(now)
	return s.write(ctx, nil, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.RecordIngressCertFailureBasic(ctx, sqlc.RecordIngressCertFailureBasicParams{
			LastErrorClass: class, LastErrorAt: ts, ConsecutiveFailures: int64(l.ConsecutiveFailures),
			LastAttemptAt: toNanos(l.LastAttemptAt), NextAttemptAt: toNanos(l.NextAttemptAt),
			RetryAfterUntil: toNanos(l.RetryAfterUntil), UpdatedAt: ts, ID: c.ID,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: record the failure of certificate %q: %w", c.ID, err)
		}
		if s.audit != nil {
			if err := s.audit.RecordTx(ctx, tx, audit.Entry{
				Actor: audit.SystemIngress, Action: audit.ActionIngressCertFailed, TargetType: auditTargetCertificate,
				TargetID: c.ID, EnvID: c.EnvID, Result: audit.ResultFailure,
				Details: map[string]string{"hostname": c.Hostname, "issuer": c.Issuer, "reason": class},
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the failure of %q: %w", c.Hostname, err)
			}
		}
		return false, nil
	})
}

// LedgerOf is the backoff ledger the row carries.
func LedgerOf(c Cert) acme.Ledger {
	return acme.Ledger{
		ConsecutiveFailures: c.ConsecutiveFailures, LastAttemptAt: c.LastAttemptAt,
		NextAttemptAt: c.NextAttemptAt, RetryAfterUntil: c.RetryAfterUntil,
	}
}
