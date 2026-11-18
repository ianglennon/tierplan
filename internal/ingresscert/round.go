// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import "context"

// round is one look at the certificates: while issuing is paused (after a restore) it does nothing at all, so
// no request reaches the certificate authority and none reaches a DNS provider (D-32). Otherwise the
// reconciler gives every claimed hostname a row, the renewal clocks queue the certificates that are due for
// renewal, the stored readiness is brought up to date, and the scheduler starts the runs that are due.
func (m *Manager) round(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not read the certificate state", "error", err)
		return
	}
	if st.PausedReason != "" {
		return
	}
	// The ledger's leftover challenge records go first: a due row every round, every row after Start or SweepNow.
	m.sweep(ctx, !m.sweepAll.Swap(false))
	m.reconcile(ctx)
	m.renewals(ctx)
	m.refreshReady(ctx)
	m.schedule(ctx, st)
}
