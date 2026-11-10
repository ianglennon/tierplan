// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"time"

	legoacme "github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/acme/api"
)

const (
	// Tick is how often the scheduler looks at the certificate.
	Tick = time.Minute
	// ariErrorRetry is how long the scheduler waits before asking for renewal information again after
	// a failure to get it. Such a failure is not a renewal failure and is not charged to the ledger.
	ariErrorRetry = time.Hour
)

// Run is the scheduler: once a Tick on the injected clock it keeps renew_at current and starts the
// renewals nobody asked for, until ctx ends. It never waits on anything but the clock and ctx, and a
// run it started is a jobs.Hub job like any other, so it ends with the hub.
func (m *Manager) Run(ctx context.Context) {
	for {
		m.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.clock.After(Tick):
		}
	}
}

// round is one look at the certificate. With certificate files set it does nothing (D-13). While
// paused after a restore, or while a run is in progress, it makes no request to the CA.
func (m *Manager) round(ctx context.Context) {
	if m.d.CertFiles || ctx.Err() != nil {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
		return
	}
	if st.PausedReason != "" || m.runActive() {
		return
	}
	if st.Mode != ModeActive {
		return
	}
	leaf, found, err := m.storedLeaf(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the stored Let's Encrypt certificate", "error", err)
		return
	}
	if !found {
		return
	}
	m.renewalRound(ctx, st, leaf)
}

// renewalRound keeps renew_at current from the CA's renewal information, and starts the renewal when
// it is due and the ledger allows an automatic attempt.
func (m *Manager) renewalRound(ctx context.Context, st State, leaf *x509.Certificate) {
	now := m.clock.Now()
	// The question to the CA is asked before the admin's lock is taken, so a slow CA never holds up a
	// click. Its answer is applied to the state as it is by then.
	var change func(*State)
	if now.Before(leaf.NotAfter) {
		change = m.askARI(ctx, st, leaf, now)
	}

	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
		return
	}
	if st.PausedReason != "" || st.Mode != ModeActive || m.runActive() {
		return
	}
	cur, found, err := m.storedLeaf(ctx)
	if err != nil || !found || cur.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		// The certificate changed while the CA was asked about it: the answer belongs to the old one.
		return
	}
	now = m.clock.Now()
	if change != nil {
		change(&st)
		st.UpdatedAt = now
		if err := m.d.Store.SaveState(ctx, st); err != nil {
			m.log.Error("the scheduler could not save the renewal time", "error", err)
			return
		}
	}

	if now.Before(st.RenewAt) || !AutomaticAllowed(ledgerOf(st), now) {
		return
	}
	req := m.renewRequest(ctx, st, leaf)
	m.log.Info("starting a scheduled renewal of the Let's Encrypt certificate", "renew_at", st.RenewAt)
	if _, err := m.startRun(runParams{trigger: TriggerSchedule, req: req}); err != nil && !errors.Is(err, ErrRunActive) {
		m.log.Error("the scheduler could not start a renewal", "error", err)
	}
}

// askARI returns the change to make to the state once the CA has been asked about leaf, or nil when
// it is not yet time to ask. A CA that offers no renewal information, or none that is usable, gives
// the certificate's own two thirds (or half) of its lifetime. A failure to get an answer keeps what is
// there and asks again in an hour; it is not a renewal failure.
func (m *Manager) askARI(ctx context.Context, st State, leaf *x509.Certificate, now time.Time) func(*State) {
	if !st.RenewAt.IsZero() && now.Before(st.AriNextCheckAt) {
		return nil
	}
	fallback := func(s *State, next time.Time) {
		at, basis := FallbackRenewAt(leaf.NotBefore, leaf.NotAfter)
		s.RenewAt, s.RenewBasis = at, string(basis)
		s.AriWindowStart, s.AriWindowEnd = time.Time{}, time.Time{}
		s.AriCheckedAt, s.AriNextCheckAt = now, next
	}
	noInfo := func(s *State) { fallback(s, NextARICheck(now, 0)) }
	failed := func(s *State) {
		if s.RenewAt.IsZero() {
			fallback(s, now.Add(ariErrorRetry))
			return
		}
		s.AriNextCheckAt = now.Add(ariErrorRetry)
	}

	meta, err := m.d.Client.Meta(ctx)
	if err != nil {
		m.log.Warn("the scheduler could not read the ACME directory for renewal information", "error", err)
		return failed
	}
	if !meta.RenewalInfo {
		return noInfo
	}
	core, _, _, err := m.d.Client.core(ctx)
	if err != nil {
		m.log.Warn("the scheduler could not open the ACME account for renewal information", "error", err)
		return failed
	}
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		m.log.Warn("the stored certificate has no renewal identifier", "error", err)
		return noInfo
	}
	info, err := core.Certificates.GetRenewalInfo(ctx, id)
	var problem *legoacme.ProblemDetails
	switch {
	case errors.Is(err, api.ErrNoARI), errors.As(err, &problem) && problem.HTTPStatus == http.StatusNotFound:
		return noInfo
	case err != nil:
		m.log.Warn("the scheduler could not get renewal information", "error", err)
		return failed
	}
	next := NextARICheck(now, info.RetryAfter)
	return func(s *State) {
		var kept time.Time
		if s.RenewBasis == string(BasisARI) {
			kept = s.RenewAt
		}
		s.RenewAt = PickInWindow(info.SuggestedWindow.Start, info.SuggestedWindow.End, kept, m.d.Rand)
		s.RenewBasis = string(BasisARI)
		s.AriWindowStart, s.AriWindowEnd = info.SuggestedWindow.Start, info.SuggestedWindow.End
		s.AriCheckedAt, s.AriNextCheckAt = now, next
	}
}
