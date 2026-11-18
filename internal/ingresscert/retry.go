// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ErrCertStopped is what RetryNow returns for a certificate that is stopped: it waits for a claim or a request, not
// for a retry, and a retry would only be stopped again (or order a name nothing refers to).
var ErrCertStopped = errors.New("ingresscert: the certificate is stopped, and a retry does not start it")

// BlockedError is a retry now that the manual guards refuse: the certificate authority asked TierPlan to wait
// (Reason acme.BlockToldToWait), or three attempts have failed inside an hour (acme.BlockFailureGuard). Until is the
// first instant a retry is allowed.
type BlockedError struct {
	Until  time.Time
	Reason string
}

// Error is a fixed sentence and the instant; the certificate authority's own words are never in it.
func (e *BlockedError) Error() string {
	why := "a retry is not allowed yet"
	switch e.Reason {
	case acme.BlockToldToWait:
		why = "the certificate authority asked TierPlan to wait"
	case acme.BlockFailureGuard:
		why = "three attempts failed within an hour"
	}
	return fmt.Sprintf("ingresscert: %s, so a retry is not allowed until %s", why, e.Until.UTC().Format(time.RFC3339))
}

// RetryNow asks for another attempt at certificate certID of environment envID without waiting for its backoff (D-13).
// It refuses while issuing is paused (ErrPaused), for a certificate that is not a live ACME certificate of the
// environment (ErrCertNotFound: another environment's, retired, uploaded or gone), for a stopped one
// (ErrCertStopped), and inside the certificate authority's Retry-After or after three failures within an hour
// (*BlockedError, from acme.ManualBlock). Otherwise the row is queued with its next attempt now, its chain kept,
// and hook, which may be nil, runs in the same transaction with the row as queued; an error from it rolls the
// retry back. The manager is then woken. The order itself is placed by the next round, through the same
// scheduling as any other (it is never started here). A certificate whose order is already in flight is left
// alone and the retry succeeds without running the hook.
func (m *Manager) RetryNow(ctx context.Context, envID, certID string, hook CertHook) error {
	queued, err := m.d.Store.retryNow(ctx, envID, certID, m.clock.Now(), hook)
	if err != nil {
		return err
	}
	if queued {
		m.Wake()
	}
	return nil
}

// retryNow is RetryNow's write: the pause, the row and the guards are read in the same transaction that queues the
// row, so a retry never races a pause or a run that started in between. queued is false for an order in flight.
func (s *Store) retryNow(ctx context.Context, envID, certID string, now time.Time, hook CertHook) (queued bool, err error) {
	// Queueing a retry is not a change to what is served, so it raises no certs_version.
	err = s.write(ctx, nil, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		st, err := q.GetIngressCertState(ctx)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read the certificate state: %w", err)
		}
		if st.PausedReason != "" {
			return false, ErrPaused
		}
		row, err := q.GetIngressCert(ctx, certID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrCertNotFound
		}
		if err != nil {
			return false, fmt.Errorf("ingresscert: read certificate %q: %w", certID, err)
		}
		c, err := certOf(row)
		if err != nil {
			return false, err
		}
		if c.EnvID != envID || !c.RetiredAt.IsZero() || c.Source != SourceACME {
			return false, ErrCertNotFound
		}
		switch c.Phase {
		case PhaseStopped:
			return false, ErrCertStopped
		case PhaseOrdering:
			return false, nil
		}
		if until, reason := acme.ManualBlock(LedgerOf(c), c.FailureTimes, now); !until.IsZero() {
			return false, &BlockedError{Until: until, Reason: reason}
		}
		n, err := q.RetryIngressCertNow(ctx, sqlc.RetryIngressCertNowParams{NextAttemptAt: toNanos(now), UpdatedAt: toNanos(now), ID: certID})
		if err != nil {
			return false, fmt.Errorf("ingresscert: queue a retry of certificate %q: %w", certID, err)
		}
		if n != 1 {
			return false, ErrCertNotFound
		}
		if hook != nil {
			row, err := q.GetIngressCert(ctx, certID)
			if err != nil {
				return false, fmt.Errorf("ingresscert: read back certificate %q: %w", certID, err)
			}
			queuedCert, err := certOf(row)
			if err != nil {
				return false, err
			}
			if err := hook(ctx, tx, queuedCert); err != nil {
				return false, err
			}
		}
		queued = true
		return false, nil
	})
	return queued && err == nil, err
}
