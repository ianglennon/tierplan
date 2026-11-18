// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"

	"github.com/ianglennon/tierplan/internal/jobs"
)

// schedule starts a run for each queued row whose turn has come, oldest first, while fewer than MaxConcurrent
// runs are active (D-16). A row is reserved (phase ordering) before its job starts, and the rows affected say
// who won, so two callers never run one row. Every run is a hub job of kind JobKind owned by JobOwner, and it
// ends with ctx: the manager's context cancels its runs.
func (m *Manager) schedule(ctx context.Context, st State) {
	due, err := m.d.Store.DueQueued(ctx, m.clock.Now())
	if err != nil {
		m.log.Error("the certificate manager could not list the queued certificates", "error", err)
		return
	}
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		// A row that waits for the terms, while the terms are still not acknowledged and no account exists,
		// would only start a job to find that out again.
		if c.QueueCode == QueueNoAccount && m.waitsForTerms(ctx, st, c) {
			continue
		}
		if !m.take(c.ID) {
			if m.full() {
				return
			}
			continue
		}
		won, err := m.d.Store.MarkOrdering(ctx, c.ID)
		if err != nil || !won {
			if err != nil {
				m.log.Error("the certificate manager could not reserve a certificate", "host", c.Hostname, "error", err)
			}
			m.drop(c.ID)
			continue
		}
		id := c.ID
		_, err = m.d.Jobs.Start(jobs.Spec{
			Kind: JobKind, EnvID: c.EnvID, Owner: JobOwner, Meta: map[string]string{"hostname": c.Hostname},
		}, func(jctx context.Context, _ *jobs.Reporter) {
			m.runJob(jctx, ctx, id)
		})
		if err != nil {
			m.log.Error("the certificate manager could not start a run", "host", c.Hostname, "error", err)
			if rerr := m.d.Store.ReleaseOrdering(context.WithoutCancel(ctx), id); rerr != nil {
				m.log.Error("the certificate manager could not release a reservation", "host", c.Hostname, "error", rerr)
			}
			m.drop(id)
		}
	}
}

// waitsForTerms reports whether row c, which is queued for want of an account, still has no way forward: the
// terms are not acknowledged and the directory has no ingress account.
func (m *Manager) waitsForTerms(ctx context.Context, st State, c Cert) bool {
	if !st.TermsAcceptedAt.IsZero() {
		return false
	}
	_, dir := m.client(c.Issuer)
	_, found, err := m.d.Store.AccountURL(ctx, dir.URL)
	return err == nil && !found
}

// take claims a slot for certificate id; false when the cap is reached or a run of id is already active.
func (m *Manager) take(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) >= MaxConcurrent {
		return false
	}
	if _, dup := m.active[id]; dup {
		return false
	}
	m.active[id] = struct{}{}
	return true
}

// full reports whether every slot is taken.
func (m *Manager) full() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active) >= MaxConcurrent
}

// drop frees the slot of certificate id.
func (m *Manager) drop(id string) {
	m.mu.Lock()
	delete(m.active, id)
	m.mu.Unlock()
}
