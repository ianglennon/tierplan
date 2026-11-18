// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ownIssuance is one certificate TierPlan's own HTTPS manager obtained: when, and the names it covers.
type ownIssuance struct {
	At    time.Time
	Names []string
}

// domainSuffix is a registered domain of an environment (an ingress_domains row, less its token).
type domainSuffix struct {
	ID, EnvID, Suffix, Issuer string
}

// The store reads the governor and the budget view gather their inputs with. Each is a read of one query; the
// counting itself stays in the pure functions (Allow, Decide, budgetRow).

// domainIssuances are the instants of the new certificates of one issuer under registered domain rd in the
// window that ends at now.
func (s *Store) domainIssuances(ctx context.Context, issuer, rd string, now time.Time) ([]time.Time, error) {
	rows, err := s.st.RQ.ListIngressIssuancesForDomain(ctx, sqlc.ListIngressIssuancesForDomainParams{
		Issuer: issuer, RegisteredDomain: rd, IssuedAt: toNanos(now.Add(-Window)),
	})
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the issuances under %q: %w", rd, err)
	}
	return instantsOf(rows), nil
}

// hostIssuances are the instants of every certificate of one issuer for hostname in the window that ends at now.
func (s *Store) hostIssuances(ctx context.Context, issuer, hostname string, now time.Time) ([]time.Time, error) {
	rows, err := s.st.RQ.ListIngressIssuancesForHost(ctx, sqlc.ListIngressIssuancesForHostParams{
		Issuer: issuer, Hostname: hostname, IssuedAt: toNanos(now.Add(-Window)),
	})
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the issuances of %q: %w", hostname, err)
	}
	return instantsOf(rows), nil
}

// orderStarts are the instants the account of the directory started an order in the window that ends at now.
func (s *Store) orderStarts(ctx context.Context, directoryURL string, now time.Time) ([]time.Time, error) {
	rows, err := s.st.RQ.ListIngressOrderStarts(ctx, sqlc.ListIngressOrderStartsParams{
		DirectoryUrl: directoryURL, CreatedAt: toNanos(now.Add(-OrderStartsWindow)),
	})
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the order starts: %w", err)
	}
	return instantsOf(rows), nil
}

// ownIssuances are the certificates the own HTTPS manager obtained after since. It only reads the own run history.
func (s *Store) ownIssuances(ctx context.Context, since time.Time) ([]ownIssuance, error) {
	rows, err := s.st.RQ.ListOwnACMEIssuances(ctx, toNanos(since))
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the own certificate issuances: %w", err)
	}
	out := make([]ownIssuance, 0, len(rows))
	for _, r := range rows {
		var names []string
		if err := json.Unmarshal([]byte(r.Names), &names); err != nil {
			continue // a run whose names cannot be read counts for no domain
		}
		out = append(out, ownIssuance{At: fromNanos(r.FinishedAt), Names: names})
	}
	return out, nil
}

// governed are the live ACME rows with an order in flight or a CA's wait still ahead at now.
func (s *Store) governed(ctx context.Context, now time.Time) ([]Cert, error) {
	rows, err := s.st.RQ.ListGovernedIngressCerts(ctx, toNanos(now))
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the certificates under a budget: %w", err)
	}
	return certsOf(rows)
}

// PruneIssuances deletes the ledger rows issued before cutoff and returns how many there were.
func (s *Store) PruneIssuances(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.st.Q.PruneIngressIssuances(ctx, toNanos(cutoff))
	if err != nil {
		return 0, fmt.Errorf("ingresscert: prune the issuance ledger: %w", err)
	}
	return int(n), nil
}

// domainSuffixes lists every domain of every environment.
func (s *Store) domainSuffixes(ctx context.Context) ([]domainSuffix, error) {
	rows, err := s.st.RQ.ListIngressDomainSuffixesAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingresscert: list the domains: %w", err)
	}
	out := make([]domainSuffix, 0, len(rows))
	for _, r := range rows {
		out = append(out, domainSuffix{ID: r.ID, EnvID: r.EnvID, Suffix: r.Suffix, Issuer: r.Issuer})
	}
	return out, nil
}

func instantsOf(nanos []int64) []time.Time {
	out := make([]time.Time, 0, len(nanos))
	for _, n := range nanos {
		out = append(out, fromNanos(n))
	}
	return out
}

// ownInstantsUnder are the instants of the own issuances whose names include one under registered domain rd. One
// certificate counts once however many of its names sit under rd: the CA counts a certificate, not a name.
func ownInstantsUnder(own []ownIssuance, rd string) []time.Time {
	var out []time.Time
	for _, o := range own {
		for _, n := range o.Names {
			if d, err := RegisteredDomain(strings.TrimPrefix(n, "*.")); err == nil && d == rd {
				out = append(out, o.At)
				break
			}
		}
	}
	return out
}

// domainIssued are the instants counted against registered domain rd for an issuer, install-wide: the ledger's new
// certificates and, on production, the certificates of TierPlan's own HTTPS manager (D-38). The own certificate
// is counted as new, the conservative reading (RESEARCH A7).
func (m *Manager) domainIssued(ctx context.Context, issuer, rd string, now time.Time) ([]time.Time, error) {
	issued, err := m.d.Store.domainIssuances(ctx, issuer, rd, now)
	if err != nil {
		return nil, err
	}
	if issuer != IssuerProduction {
		return issued, nil
	}
	own, err := m.d.Store.ownIssuances(ctx, now.Add(-Window))
	if err != nil {
		return nil, err
	}
	return append(issued, ownInstantsUnder(own, rd)...), nil
}

// sameBudget reports whether row c counts against the budget of registered domain rd on an issuer as a new name:
// it holds no chain, it names that issuer and its hostname sits under rd.
func sameBudget(c Cert, issuer, rd string) bool {
	if len(c.ChainPEM) > 0 || c.Issuer != issuer {
		return false
	}
	d, err := RegisteredDomain(c.Hostname)
	return err == nil && d == rd
}

// reservedOf counts the new names with an order in flight under the budget; except is a row id left out.
func reservedOf(governed []Cert, issuer, rd, except string) int {
	n := 0
	for _, c := range governed {
		if c.ID != except && c.Phase == PhaseOrdering && sameBudget(c, issuer, rd) {
			n++
		}
	}
	return n
}

// holdOf is the latest instant a CA's Retry-After on a new-name order under the budget still holds it, or the zero
// time when none does. Only a rateLimited answer holds a registered domain.
func holdOf(governed []Cert, issuer, rd, except string, now time.Time) time.Time {
	var until time.Time
	for _, c := range governed {
		if c.ID != except && c.LastErrorClass == acme.ClassRateLimited && c.RetryAfterUntil.After(now) && c.RetryAfterUntil.After(until) &&
			sameBudget(c, issuer, rd) {
			until = c.RetryAfterUntil
		}
	}
	return until
}
