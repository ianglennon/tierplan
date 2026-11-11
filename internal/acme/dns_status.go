// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"slices"
	"time"
)

// DNSView is the DNS-01 settings as the page shows them. The token is never in it: TokenSaved says only
// whether one is saved (CHAL-01, D-03).
type DNSView struct {
	Provider   string
	TokenSaved bool
	// Resolvers are the normalised resolver list as saved; empty, never nil, when the system's are used.
	Resolvers []string
	// LeftOver lists every TXT record TierPlan created and could not yet delete, oldest first (D-18).
	LeftOver []LeftOverView
}

// LeftOverView is one TXT record still at the DNS provider: what it is called, the provider's ID for it,
// the class of its last delete error and when it was tried and will be tried again.
type LeftOverView struct {
	Name, RecordID, Class string
	CreatedAt             time.Time
	LastTryAt, NextTryAt  time.Time
	// RetryPaused is true while no delete is tried, and PausedReason says why: PausedRestore or
	// PausedCertFiles (D-23). NextTryAt is then zero: no try is promised.
	RetryPaused  bool
	PausedReason string
}

// dnsView reads the DNS settings and the record ledger for the status. paused is why deletes are not
// being tried ("" when they are).
func (m *Manager) dnsView(ctx context.Context, st State, paused string) (DNSView, error) {
	provider := st.DNSProvider
	if provider == "" {
		provider = defaultDNSProvider
	}
	saved, err := m.d.Store.DNSTokenSaved(ctx, provider)
	if err != nil {
		return DNSView{}, err
	}
	rows, err := m.d.Store.DNSRecords(ctx)
	if err != nil {
		return DNSView{}, err
	}
	out := DNSView{
		Provider: provider, TokenSaved: saved,
		Resolvers: slices.Clone(st.DNSResolvers), LeftOver: make([]LeftOverView, 0, len(rows)),
	}
	if out.Resolvers == nil {
		out.Resolvers = []string{}
	}
	for _, r := range rows {
		v := LeftOverView{
			Name: r.Name, RecordID: r.RecordID, Class: r.LastErrorClass,
			CreatedAt: r.CreatedAt, LastTryAt: r.LastTryAt, NextTryAt: r.NextTryAt,
			RetryPaused: paused != "", PausedReason: paused,
		}
		if paused != "" {
			v.NextTryAt = time.Time{}
		}
		out.LeftOver = append(out.LeftOver, v)
	}
	return out, nil
}
