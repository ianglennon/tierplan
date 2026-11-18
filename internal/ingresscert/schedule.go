// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// schedule starts a run for each queued row whose turn has come, renewals first and the oldest first within each
// group, while fewer than MaxConcurrent runs are active (D-16). Every candidate passes the governor before it is
// reserved (D-14, D-15): a refusal is written on the row, with why and when the next slot opens (D-17), and no
// order is placed for it. A row is reserved (phase ordering) before its job starts, and the rows affected say who
// won, so two callers never run one row; the candidates are chosen serially under schedMu and a reserved new name
// counts against its registered domain's budget, so two names never take the last slot. Every run is a hub job of
// kind JobKind owned by JobOwner, and it ends with ctx: the manager's context cancels its runs.
func (m *Manager) schedule(ctx context.Context, st State) {
	m.schedMu.Lock()
	defer m.schedMu.Unlock()
	due, err := m.d.Store.DueQueued(ctx, m.clock.Now())
	if err != nil {
		m.log.Error("the certificate manager could not list the queued certificates", "error", err)
		return
	}
	// Renewals first (D-14): a row that holds a chain is a certificate that is being served and is about to run
	// out, which matters more than a name that has none. The order within each group is the oldest first.
	slices.SortStableFunc(due, func(a, b Cert) int {
		return cmp.Compare(rank(a), rank(b))
	})
	now := m.clock.Now()
	for _, c := range due {
		if ctx.Err() != nil {
			return
		}
		// The ledger decides whether this row may run: neither its backoff nor a CA's Retry-After is still ahead.
		// The listing already asks the same, so this is the rule stated where the run is started.
		if !acme.AutomaticAllowed(LedgerOf(c), now) {
			continue
		}
		// A row that waits for the terms, while the terms are not acknowledged and no account exists, would only
		// start a job to find that out again.
		if c.QueueCode == QueueNoAccount && m.waitsForTerms(ctx, st, c) {
			continue
		}
		d, err := m.decide(ctx, c, now)
		if err != nil {
			m.log.Error("the certificate manager could not weigh a certificate against the rate limits", "host", c.Hostname, "error", err)
			continue
		}
		if !d.OK {
			m.queue(ctx, c, QueueState{Code: d.Code, Used: d.Used, Cap: d.Cap, Domain: d.Domain, NextSlot: d.NextSlot})
			continue
		}
		if !m.take(c.ID) {
			if m.full() {
				m.queue(ctx, c, QueueState{Code: QueueConcurrency})
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

// decide gathers what the governor reads for candidate c and asks Decide. A hostname with no registered domain
// has no budget; its run fails on its own account.
func (m *Manager) decide(ctx context.Context, c Cert, now time.Time) (Decision, error) {
	rd, err := RegisteredDomain(c.Hostname)
	if err != nil {
		return Decision{OK: true}, nil
	}
	in := DecideInput{Now: now, Renewal: len(c.ChainPEM) > 0, Issuer: c.Issuer, RegisteredDomain: rd}
	if in.HostIssued, err = m.d.Store.hostIssuances(ctx, c.Issuer, c.Hostname, now); err != nil {
		return Decision{}, err
	}
	if !in.Renewal {
		if in.DomainIssued, err = m.domainIssued(ctx, c.Issuer, rd, now); err != nil {
			return Decision{}, err
		}
		if _, dir := m.client(c.Issuer); dir.URL != "" {
			if in.OrderStarts, err = m.d.Store.orderStarts(ctx, dir.URL, now); err != nil {
				return Decision{}, err
			}
		}
		governed, err := m.d.Store.governed(ctx, now)
		if err != nil {
			return Decision{}, err
		}
		in.Reserved = reservedOf(governed, c.Issuer, rd, c.ID)
		in.CAHoldUntil = holdOf(governed, c.Issuer, rd, c.ID, now)
	}
	return Decide(in), nil
}

// queue records why row c waits, unless the row already says so: a round that finds the same answer writes nothing.
func (m *Manager) queue(ctx context.Context, c Cert, q QueueState) {
	if c.QueueCode == q.Code && c.QueueUsed == q.Used && c.QueueCap == q.Cap && c.QueueDomain == q.Domain &&
		c.QueueNextSlotAt.Equal(q.NextSlot) {
		return
	}
	if err := m.d.Store.SetQueue(ctx, c.ID, q); err != nil {
		m.log.Error("the certificate manager could not record why a certificate waits", "host", c.Hostname, "error", err)
	}
}

// rank orders the rows the scheduler starts: a renewal (a row with a chain) before a new name.
func rank(c Cert) int {
	if len(c.ChainPEM) > 0 {
		return 0
	}
	return 1
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
