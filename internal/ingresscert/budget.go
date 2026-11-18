// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"slices"
	"strings"
	"time"
)

// BudgetRow is the weekly new-certificate budget of one registered domain on one issuer, install-wide (D-18).
type BudgetRow struct {
	RegisteredDomain string
	Issuer           string
	// Used counts the new certificates in the window and the orders in flight, TierPlan's own certificate
	// included on production. Cap is where TierPlan stops, Limit the CA's figure, Headroom what is left to Cap.
	Used, Cap, Limit, Headroom int
	// NextSlot is when a slot opens, set only when Headroom is 0 and something in the window will expire.
	NextSlot time.Time
	// Domains are the environment's suffixes under the registered domain, sorted.
	Domains []string
	// Shared is true when another domain, in any environment, or TierPlan's own host sits under the same
	// registered domain on the same issuer: the budget is not this environment's alone.
	Shared bool
}

// budgetRow is the pure part of the budget: the row for a registered domain from the gathered issuances and
// reserved orders at now.
func budgetRow(now time.Time, issuer, rd string, issued []time.Time, reserved int) BudgetRow {
	cp := capsOf(issuer)
	_, used, next := Allow(now, issued, reserved, cp.newCap)
	return BudgetRow{
		RegisteredDomain: rd, Issuer: issuer, Used: used, Cap: cp.newCap, Limit: cp.newLimit,
		Headroom: max(0, cp.newCap-used), NextSlot: next, Domains: []string{},
	}
}

// Budget is the budget of every registered domain of environment envID, one row per registered domain and issuer
// in use, sorted by registered domain then issuer. The counts are install-wide (D-14); Domains lists only the
// environment's own suffixes, and Shared says another domain or TierPlan's own host shares the budget without
// naming it (T-20-33). An environment with no domains has no rows.
func (m *Manager) Budget(ctx context.Context, envID string) ([]BudgetRow, error) {
	all, err := m.d.Store.domainSuffixes(ctx)
	if err != nil {
		return nil, err
	}
	type key struct{ rd, issuer string }
	mine := map[key][]string{}
	others := map[key]bool{}
	for _, d := range all {
		rd, err := RegisteredDomain(d.Suffix)
		if err != nil {
			continue // a suffix that is itself a public suffix has no budget of its own
		}
		k := key{rd, d.Issuer}
		if d.EnvID == envID {
			mine[k] = append(mine[k], d.Suffix)
		}
	}
	for _, d := range all {
		rd, err := RegisteredDomain(d.Suffix)
		if err != nil {
			continue
		}
		k := key{rd, d.Issuer}
		if _, ours := mine[k]; ours && d.EnvID != envID {
			others[k] = true
		}
	}
	if len(mine) == 0 {
		return []BudgetRow{}, nil
	}
	now := m.clock.Now()
	governed, err := m.d.Store.governed(ctx, now)
	if err != nil {
		return nil, err
	}
	ownAny, err := m.d.Store.ownIssuances(ctx, time.Time{})
	if err != nil {
		return nil, err
	}
	rows := make([]BudgetRow, 0, len(mine))
	for k, suffixes := range mine {
		issued, err := m.domainIssued(ctx, k.issuer, k.rd, now)
		if err != nil {
			return nil, err
		}
		row := budgetRow(now, k.issuer, k.rd, issued, reservedOf(governed, k.issuer, k.rd, ""))
		slices.Sort(suffixes)
		row.Domains = suffixes
		row.Shared = others[k] || (k.issuer == IssuerProduction && len(ownInstantsUnder(ownAny, k.rd)) > 0)
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b BudgetRow) int {
		if c := strings.Compare(a.RegisteredDomain, b.RegisteredDomain); c != 0 {
			return c
		}
		return strings.Compare(a.Issuer, b.Issuer)
	})
	return rows, nil
}
