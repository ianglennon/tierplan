// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// SweepNow asks the manager for a full ingress sweep: the next round after this call reads every ledger row of a
// hostname certificate, not only the rows whose retry is due, and deletes them. It never blocks. The API calls it
// when a domain's delete or token replacement was refused for the records still held, so the cleanup starts at
// once and not at the next tick (D-27).
func (m *Manager) SweepNow() {
	m.sweepAll.Store(true)
	m.Wake()
}

// sweep deletes the DNS records the ledger holds for hostname certificates (the rows with a credential_ref), each
// with the token of the domain that made it. A due-only sweep reads the rows whose next try has come; a full one
// reads every row, which is what Start and SweepNow ask for. It does nothing while issuing is paused after a
// restore (D-32), so no call reaches a DNS provider, and nothing once ctx is done.
//
// A row is deleted by the provider's record id the ledger stored and never by name or value. The domain's token
// is opened once per (provider, credential) group, kept only for that sweep and never logged.
//
// Residual risk, accepted for Phase 20 (D-41): the domain a row's credential_ref names can be gone, because the
// environment that owned it was deleted. Such a row cannot be deleted at the provider, since no token is left
// that can. It is stamped dns_token_invalid and retried on the backoff schedule; it is never given up on silently
// and no "abandoned" state exists. The records stay at the provider until an operator removes them by hand.
func (m *Manager) sweep(ctx context.Context, dueOnly bool) {
	if ctx.Err() != nil {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the ingress DNS sweep could not read the certificate state", "error", err)
		return
	}
	if st.PausedReason != "" {
		return
	}
	rows, err := m.d.Ledger.IngressDNSRecords(ctx, m.clock.Now(), dueOnly)
	if err != nil {
		m.log.Error("the ingress DNS sweep could not read the ledger", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	m.engine.Sweep(ctx, rows, m.credentialProvider)
}

// credentialProvider is the acme.CredentialResolver of the ingress sweep: credentialRef is a domain id, and the
// provider is built with that domain's own token. A domain that is gone, or has no token, gives no provider and
// the class dns_token_invalid; a provider id nothing is built in for gives ClassInternal.
func (m *Manager) credentialProvider(ctx context.Context, provider, credentialRef string) (dns.Provider, string) {
	token, err := m.d.Domains.DomainTokenByID(ctx, credentialRef)
	switch {
	case errors.Is(err, ingressstore.ErrDomainNotFound), errors.Is(err, ingressstore.ErrNoToken):
		return nil, dns.ClassTokenInvalid
	case err != nil:
		m.log.Error("the ingress DNS sweep could not open a domain's token", "domain", credentialRef, "error", err)
		return nil, dns.ClassTokenInvalid
	}
	p, ok := m.engine.Provider(provider, string(token))
	if !ok {
		m.log.Error("the ingress DNS sweep has no provider to delete with", "provider", provider)
		return nil, acme.ClassInternal
	}
	return p, ""
}
