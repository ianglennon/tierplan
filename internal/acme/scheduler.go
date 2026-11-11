// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"bytes"
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

// round is one look at the certificate. With certificate files set it does nothing (D-13), and while a
// run is in progress it leaves everything to the run. What is served is put right first, because that
// is local and a lapsed certificate must not wait on anything (D-15). While paused after a restore
// nothing more is done: no request to the CA is made, whether to ask for renewal information, to
// renew or to continue an order (ACME-08), and no DNS record is deleted (D-23). The left-over DNS
// records that are due go next, then the renewal.
func (m *Manager) round(ctx context.Context) {
	if m.d.CertFiles || ctx.Err() != nil || m.runActive() {
		return
	}
	m.syncServed(ctx)
	if !m.settled(ctx) {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
		return
	}
	if st.PausedReason != "" {
		return
	}
	// Left-over DNS records are retried when their next try is due, whatever the saved challenge is and
	// whether or not Let's Encrypt is in use, before anything that could start a run (D-17, D-18).
	m.sweepDNS(ctx, true)
	if m.resumeRound(ctx) {
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

// settled writes the end of a run that could not be written when it ended, and reports whether
// nothing is left unwritten. Until it is written the stored ledger does not know that run, so the
// round starts nothing.
func (m *Manager) settled(ctx context.Context) bool {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.settle(ctx); err != nil {
		m.log.Error("the scheduler could not record the end of the last certificate run, so it starts nothing", "error", err)
		return false
	}
	return true
}

// syncServed makes what is served follow what is stored (D-15). A valid stored certificate that is not
// the one being served, because the Installer refused it, is offered again. A Let's Encrypt certificate
// being served that has reached its NotAfter is replaced by the self-signed one, once; renewals carry
// on, and the one that works is served by the run that made it. Expiry is judged on the certificate
// being served, not the stored one: when a renewal was stored but could not be switched to, the older
// certificate is still served, and it lapses first. It reads the clock and the database and nothing
// else, so it never depends on the CA being reachable.
func (m *Manager) syncServed(ctx context.Context) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.runActive() {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
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
	now := m.clock.Now()
	if now.Before(leaf.NotAfter) && !m.onWire().is(leaf) {
		err = m.serveStored(ctx)
		if err == nil {
			m.log.Info("the stored Let's Encrypt certificate is served again")
			return
		}
		m.noteInstallFailure(err)
	}
	if w := m.onWire(); w.acme && w.leaf != nil && !now.Before(w.leaf.NotAfter) {
		m.log.Warn("the Let's Encrypt certificate being served has expired, so the self-signed certificate is served until it is renewed",
			"expired_at", w.leaf.NotAfter)
		if err := m.d.Installer.ServeSelfSigned(); err != nil {
			m.log.Error("the self-signed certificate could not be served after the Let's Encrypt one expired", "error", err)
		} else {
			m.noteLapsed(w.leaf.NotAfter)
		}
	}
}

// serveStored asks the Installer to serve the stored Let's Encrypt certificate.
func (m *Manager) serveStored(ctx context.Context) error {
	chain, key, found, err := m.d.Store.Certificate(ctx)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("no Let's Encrypt certificate is stored")
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	return m.d.Installer.ServeACME(chain, keyPEM)
}

// noteInstallFailure logs a failure to serve the stored certificate, once for each distinct message, so
// a refusal that lasts does not fill the log at one line a minute.
func (m *Manager) noteInstallFailure(err error) {
	m.mu.Lock()
	same := m.installErr == err.Error()
	m.installErr = err.Error()
	m.mu.Unlock()
	if !same {
		m.log.Error("the stored Let's Encrypt certificate could not be served", "error", err)
	}
}

// servedCert is the certificate the Installer serves: a Let's Encrypt one, with its leaf, or the
// self-signed one.
type servedCert struct {
	acme bool
	// leaf is the Let's Encrypt leaf being served; nil when it is self-signed, or in the unlikely
	// case that a chain the Installer accepted cannot be parsed here.
	leaf *x509.Certificate
	// lapsed is set while the self-signed certificate is served in place of a Let's Encrypt one that
	// expired (D-15): when that one expired.
	lapsed time.Time
}

// is reports whether leaf is the Let's Encrypt certificate being served.
func (w servedCert) is(leaf *x509.Certificate) bool {
	return w.acme && w.leaf != nil && bytes.Equal(w.leaf.Raw, leaf.Raw)
}

// onWire is what the server serves, as far as this manager has seen. Only a swap that succeeded changes
// it: a refused swap leaves the certificate that was served before (Installer).
func (m *Manager) onWire() servedCert {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wire
}

func (m *Manager) setWire(w servedCert) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wire = w
	if w.acme {
		m.installErr = ""
	}
}

// noteLapsed records that the self-signed certificate now served stands in for a Let's Encrypt
// certificate that expired at expiredAt. The next swap clears it.
func (m *Manager) noteLapsed(expiredAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.wire.acme {
		m.wire.lapsed = expiredAt
	}
}

// fallback reports whether the self-signed certificate is served, or is about to be, because the Let's
// Encrypt certificate expired, and when that one expired (D-15). Like syncServed it is judged on the
// certificate being served, not on the stored one: a stored renewal that could not be switched to does
// not hide that the older certificate lapsed. stored is the stored leaf, nil when there is none.
func (m *Manager) fallback(stored *x509.Certificate, now time.Time) (bool, time.Time) {
	w := m.onWire()
	switch {
	case w.acme && w.leaf != nil:
		// A Let's Encrypt certificate is served. Once it has expired, the scheduler's next round
		// serves the self-signed one in its place.
		if !now.Before(w.leaf.NotAfter) {
			return true, w.leaf.NotAfter
		}
		return false, time.Time{}
	case w.acme:
		return false, time.Time{}
	case !w.lapsed.IsZero():
		return true, w.lapsed
	case stored != nil && !now.Before(stored.NotAfter):
		// Self-signed has been served from the start: an expired stored certificate is never installed.
		return true, stored.NotAfter
	}
	return false, time.Time{}
}

// trackedInstaller tells the manager what the Installer serves after every swap that succeeded.
type trackedInstaller struct {
	Installer
	m *Manager
}

func (t trackedInstaller) ServeACME(chainPEM, keyPEM []byte) error {
	if err := t.Installer.ServeACME(chainPEM, keyPEM); err != nil {
		return err
	}
	leaf, _ := leafOfChain(chainPEM)
	t.m.setWire(servedCert{acme: true, leaf: leaf})
	return nil
}

func (t trackedInstaller) ServeSelfSigned() error {
	if err := t.Installer.ServeSelfSigned(); err != nil {
		return err
	}
	t.m.setWire(servedCert{})
	return nil
}

// resumeRound continues an order that was left open, as a resume run, with no new order: the first
// round the ledger allows after a restart cut a run short, or after a failure that left the order
// open. It is the same continuation a retry makes, started without waiting for renew_at. An order
// whose run a person cancelled is left alone, and the pause is checked again under the admin lock. It
// reports whether a run was started.
func (m *Manager) resumeRound(ctx context.Context) bool {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the ACME state", "error", err)
		return false
	}
	if st.PausedReason != "" || m.runActive() {
		return false
	}
	ord, _, found, err := m.d.Store.OpenOrder(ctx)
	if err != nil {
		m.log.Error("the scheduler could not read the open ACME order", "error", err)
		return false
	}
	if !found || !AutomaticAllowed(ledgerOf(st), m.clock.Now()) {
		return false
	}
	if runs, err := m.d.Store.Runs(ctx, 1); err == nil && len(runs) > 0 && runs[0].Outcome == OutcomeCancelled {
		return false
	}
	if err := m.needAccount(ctx); err != nil {
		if !errors.Is(err, ErrNoAccount) {
			m.log.Error("the scheduler could not read the ACME account", "error", err)
		}
		return false
	}
	m.log.Info("continuing a Let's Encrypt order that was left open", "kind", ord.Kind)
	req := OrderRequest{Kind: ord.Kind, Names: ord.Names, Profile: ord.Profile, Replaces: ord.Replaces}
	if acct, found, err := m.d.Store.AccountInfo(ctx, m.d.Client.Directory().URL); err != nil || !found || acct.URL != ord.AccountURL {
		// The order was placed under an account the CA has since forgotten, so the run places a new
		// one, and the certificate it named is that account's: the new account may not replace it.
		req.Replaces = ""
	}
	if _, err := m.startRun(runParams{trigger: TriggerResume, challenge: challengeOf(st), req: req}); err != nil {
		if !errors.Is(err, ErrRunActive) {
			m.log.Error("the scheduler could not continue the open order", "error", err)
		}
		return false
	}
	return true
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
	// Without an account (the CA said it was gone, so it was forgotten) a run could only fail before
	// reaching the CA, and every such failure would be charged to the ledger and count toward the
	// manual-attempt guard. The renewal waits for an admin to register again, as a resume does.
	if err := m.needAccount(ctx); err != nil {
		if !errors.Is(err, ErrNoAccount) {
			m.log.Error("the scheduler could not read the ACME account", "error", err)
		}
		return
	}
	req := m.renewRequest(ctx, st, leaf)
	m.log.Info("starting a scheduled renewal of the Let's Encrypt certificate", "renew_at", st.RenewAt)
	if _, err := m.startRun(runParams{trigger: TriggerSchedule, challenge: challengeOf(st), req: req}); err != nil && !errors.Is(err, ErrRunActive) {
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
