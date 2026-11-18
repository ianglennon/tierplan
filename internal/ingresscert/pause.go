// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// PausedRestore is the paused_reason a restore writes (D-32). While it is set a round makes no call to the
// certificate authority and none to a DNS provider.
const PausedRestore = "restore"

// ErrPaused is what an operation that would order or renew returns while issuing is paused.
var ErrPaused = errors.New("ingresscert: issuing is paused after a restore until an Admin resumes it")

// ErrNotPaused is what Resume returns when there is no pause to lift.
var ErrNotPaused = errors.New("ingresscert: issuing is not paused")

// Resume lifts the pause a restore set, for the whole install. It reads the state and clears the pause in one write
// transaction, so two Admins resuming at once get one success and one ErrNotPaused. hook, which may be nil, runs in
// the same transaction after the write with the reason that was lifted, and an error from it rolls the lift back, so
// the pause stays: the API records https.ingress_resumed there, and a resume never exists without its audit row.
// Nothing is ordered by it; the next round does that.
func (s *Store) Resume(ctx context.Context, hook func(ctx context.Context, tx *sql.Tx, reason string) error) error {
	now := toNanos(s.now())
	// Lifting the pause is not a change to any certificate, so it raises no certs_version.
	return s.write(ctx, nil, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := q.GetIngressCertState(ctx)
		if err != nil {
			return false, fmt.Errorf("ingresscert: read the certificate state: %w", err)
		}
		if row.PausedReason == "" {
			return false, ErrNotPaused
		}
		n, err := q.ResumeIngressCerts(ctx, now)
		if err != nil {
			return false, fmt.Errorf("ingresscert: lift the pause: %w", err)
		}
		if n == 0 {
			return false, ErrNotPaused
		}
		if hook != nil {
			if err := hook(ctx, tx, row.PausedReason); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}

// Resume lifts the pause (Store.Resume) and wakes the manager, so the next round orders without waiting for the
// tick. A resume that is refused or rolled back by its hook wakes nothing.
func (m *Manager) Resume(ctx context.Context, hook func(ctx context.Context, tx *sql.Tx, reason string) error) error {
	if err := m.d.Store.Resume(ctx, hook); err != nil {
		return err
	}
	m.Wake()
	return nil
}
