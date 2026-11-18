// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/go-acme/lego/v5/acme/api"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ariErrorRetry is how long the manager waits before asking the certificate authority for renewal information
// again after a failure to get an answer. Such a failure is not a renewal failure.
const ariErrorRetry = time.Hour

// HalfLifeRenewAt is when a certificate is due for renewal when the certificate authority gave no renewal
// window: half way through its own lifetime (D-09), whatever that lifetime is, because a CA's lifetimes change.
// A lifetime of zero or less is due at once, at notBefore.
func HalfLifeRenewAt(notBefore, notAfter time.Time) (time.Time, acme.RenewBasis) {
	lifetime := notAfter.Sub(notBefore)
	if lifetime <= 0 {
		return notBefore, acme.BasisHalf
	}
	return notBefore.Add(lifetime / 2), acme.BasisHalf
}

// renewals looks at the renewal clock of every live ACME certificate that holds a chain and waits for its
// renewal, and queues the ones that are due (D-09). Nothing here is a change to what is served, so none of it
// raises certs_version.
func (m *Manager) renewals(ctx context.Context) {
	now := m.clock.Now()
	rows, err := m.d.Store.st.RQ.ListRenewalDueIngressCerts(ctx, toNanos(now))
	if err != nil {
		m.log.Error("the certificate manager could not list the certificates that wait for their renewal", "error", err)
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		c, err := certOf(row)
		if err != nil {
			m.log.Error("the certificate manager could not read a certificate", "error", err)
			continue
		}
		m.renewalClock(ctx, c, now)
	}
}

// renewalClock keeps renew_at of one certificate current from the certificate authority's renewal information,
// and queues the certificate for its renewal when that time has come. A CA that offers no renewal window, or
// none that is usable, gives the certificate's own half life. A failure to get an answer keeps what is there and
// asks again in an hour.
func (m *Manager) renewalClock(ctx context.Context, c Cert, now time.Time) {
	leaf, err := leafOf(c.ChainPEM)
	if err != nil {
		m.log.Error("the stored certificate cannot be read, so it has no renewal time", "host", c.Hostname, "error", err)
		return
	}
	cur := renewalState{
		at: c.RenewAt, basis: c.RenewBasis, windowStart: c.AriWindowStart, windowEnd: c.AriWindowEnd,
		checked: c.AriCheckedAt, nextCheck: c.AriNextCheckAt,
	}
	next := cur
	if (c.RenewAt.IsZero() || !now.Before(c.AriNextCheckAt)) && now.Before(leaf.NotAfter) {
		next = m.askWindow(ctx, c, leaf, now, cur)
	} else if c.RenewAt.IsZero() {
		// An expired certificate is not asked about: it is due at its half life, which has passed.
		next = halfLife(leaf.NotBefore, leaf.NotAfter, now, now.Add(ariErrorRetry))
	}
	if !next.same(cur) {
		if err := m.d.Store.saveRenewal(ctx, c.ID, next, now); err != nil {
			m.log.Error("the certificate manager could not save the renewal time", "host", c.Hostname, "error", err)
			return
		}
	}
	if now.Before(next.at) {
		return
	}
	if _, err := m.d.Store.markRenewalDue(ctx, c.ID, now); err != nil {
		m.log.Error("the certificate manager could not queue a renewal", "host", c.Hostname, "error", err)
	}
}

// renewalState is the stored renewal clock of a certificate.
type renewalState struct {
	at                     time.Time
	basis                  string
	windowStart, windowEnd time.Time
	checked, nextCheck     time.Time
}

// same reports whether two clocks hold the same instants and basis.
func (r renewalState) same(o renewalState) bool {
	return r.basis == o.basis && r.at.Equal(o.at) && r.windowStart.Equal(o.windowStart) && r.windowEnd.Equal(o.windowEnd) &&
		r.checked.Equal(o.checked) && r.nextCheck.Equal(o.nextCheck)
}

// halfLife is the clock of a certificate the authority has no window for.
func halfLife(notBefore, notAfter, now, nextCheck time.Time) renewalState {
	at, basis := HalfLifeRenewAt(notBefore, notAfter)
	return renewalState{at: at, basis: string(basis), checked: now, nextCheck: nextCheck}
}

// askWindow asks the certificate authority when to renew leaf and returns the clock to store, cur being the one
// stored now. A renewal time picked earlier inside the window is kept while it is still inside it.
func (m *Manager) askWindow(ctx context.Context, c Cert, leaf *x509.Certificate, now time.Time, cur renewalState) renewalState {
	failed := func() renewalState {
		if cur.at.IsZero() {
			return halfLife(leaf.NotBefore, leaf.NotAfter, now, now.Add(ariErrorRetry))
		}
		cur.nextCheck = now.Add(ariErrorRetry)
		return cur
	}
	client, _ := m.client(c.Issuer)
	if client == nil {
		return failed()
	}
	w, ok, err := client.RenewalWindow(ctx, leaf)
	if err != nil {
		m.log.Warn("the certificate manager could not get renewal information", "host", c.Hostname, "error", err)
		return failed()
	}
	if !ok {
		return halfLife(leaf.NotBefore, leaf.NotAfter, now, acme.NextARICheck(now, 0))
	}
	var kept time.Time
	if cur.basis == string(acme.BasisARI) {
		kept = cur.at
	}
	return renewalState{
		at: acme.PickInWindow(w.Start, w.End, kept, m.d.Rand), basis: string(acme.BasisARI),
		windowStart: w.Start, windowEnd: w.End, checked: now, nextCheck: acme.NextARICheck(now, w.RetryAfter),
	}
}

// saveRenewal stores the renewal clock of certificate id.
func (s *Store) saveRenewal(ctx context.Context, id string, r renewalState, now time.Time) error {
	if err := s.st.Q.UpdateIngressCertRenewal(ctx, sqlc.UpdateIngressCertRenewalParams{
		RenewAt: toNanos(r.at), RenewBasis: r.basis, AriWindowStart: toNanos(r.windowStart),
		AriWindowEnd: toNanos(r.windowEnd), AriCheckedAt: toNanos(r.checked), AriNextCheckAt: toNanos(r.nextCheck),
		UpdatedAt: toNanos(now), ID: id,
	}); err != nil {
		return fmt.Errorf("ingresscert: store the renewal time of certificate %q: %w", id, err)
	}
	return nil
}

// markRenewalDue moves an idle certificate to queued, chain kept, and says whether it was still idle.
func (s *Store) markRenewalDue(ctx context.Context, id string, now time.Time) (bool, error) {
	n, err := s.st.Q.MarkIngressCertRenewalDue(ctx, sqlc.MarkIngressCertRenewalDueParams{UpdatedAt: toNanos(now), ID: id})
	if err != nil {
		return false, fmt.Errorf("ingresscert: queue the renewal of certificate %q: %w", id, err)
	}
	return n == 1, nil
}

// replacesOf is the renewal identifier of the certificate row c, which a renewal order names as the certificate
// it replaces. It is empty unless the directory offers renewalInfo and c was issued to the account registered
// with the directory now: a CA refuses to let one account replace a certificate issued to another, so after the
// CA forgot the account and a new one was registered, and for a certificate whose account is not known, the
// renewal names nothing (Pitfall 4). A failure to tell is a no.
func (m *Manager) replacesOf(ctx context.Context, client *acme.Client, dirURL string, c Cert) string {
	meta, err := client.Meta(ctx)
	if err != nil || !meta.RenewalInfo || c.AccountURL == "" {
		return ""
	}
	url, found, err := m.d.Store.AccountURL(ctx, dirURL)
	if err != nil || !found || url != c.AccountURL {
		return ""
	}
	leaf, err := leafOf(c.ChainPEM)
	if err != nil {
		return ""
	}
	id, err := api.MakeARICertID(leaf)
	if err != nil {
		return ""
	}
	return id
}
