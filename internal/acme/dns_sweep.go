// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// sweepDNS deletes the DNS records the ledger still holds, oldest first, each by the ID TierPlan stored
// for it and never by name or value. With dueOnly it takes only the rows whose next try has come;
// otherwise every row, which is what start-up and every order do.
//
// It does nothing while certificate files are set or after a restore until an admin resumes (D-23): no
// call is made to a provider then. An empty ledger opens no token and builds no provider. A row that
// cannot be deleted is stamped with the next try on the backoff schedule, and a failure is logged with
// the record's name and class only; nothing is returned, because a failed sweep never blocks an order.
func (m *Manager) sweepDNS(ctx context.Context, dueOnly bool) {
	if m.d.CertFiles || ctx.Err() != nil {
		return
	}
	st, err := m.d.Store.State(ctx)
	if err != nil {
		m.log.Error("the DNS sweep could not read the ACME state", "error", err)
		return
	}
	if st.PausedReason != "" {
		return
	}
	var rows []DNSRecord
	if dueOnly {
		rows, err = m.d.Store.DueDNSRecords(ctx, m.clock.Now())
	} else {
		rows, err = m.d.Store.DNSRecords(ctx)
	}
	if err != nil {
		m.log.Error("the DNS sweep could not read the record ledger", "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	// A provider is built once per sweep, from the token saved for it, the first time a row needs it.
	type opened struct {
		p     dns.Provider
		class string // why p is nil
	}
	providers := map[string]opened{}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		o, ok := providers[row.Provider]
		if !ok {
			o.p, o.class = m.sweepProvider(ctx, row.Provider)
			providers[row.Provider] = o
		}
		if o.p == nil {
			m.stampFailed(ctx, row, o.class)
			continue
		}
		if err := m.deleteRecord(ctx, o.p, row); err != nil {
			m.log.Warn("a DNS record could not be deleted", "name", row.Name, "class", deleteClass(err))
		}
	}
}

// sweepProvider builds the provider for a sweep from the effective token (the environment's, else the saved one). When it cannot, it returns the
// class the rows are stamped with: no token saved, or one that does not open, is a token the admin has
// to enter again.
func (m *Manager) sweepProvider(ctx context.Context, provider string) (dns.Provider, string) {
	token, found, err := m.token(ctx, provider)
	if err != nil {
		m.log.Error("the DNS sweep could not open the saved token", "provider", provider, "error", err)
		return nil, dns.ClassTokenInvalid
	}
	if !found {
		return nil, dns.ClassTokenInvalid
	}
	factory, ok := m.d.DNS.Providers[provider]
	if !ok {
		m.log.Error("the DNS sweep has no provider to delete with", "provider", provider)
		return nil, ClassInternal
	}
	return factory(token), ""
}
