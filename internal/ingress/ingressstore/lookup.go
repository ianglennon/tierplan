// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/vault"
)

// The reads in this file are for the certificate manager. A certificate row stores the id of the
// domain it was ordered under, not an environment, and certificates are keyed by hostname across the
// whole install, so the manager reads by id alone. Nothing the API serves to a person may use them
// with an id the person supplied: the API's reads stay environment-scoped.

// DomainByID reads a domain by its id alone, whichever environment holds it; the returned Domain
// carries that EnvID. An unknown id is ErrDomainNotFound. The token is not part of the value, only
// whether one is saved.
func (s *Store) DomainByID(ctx context.Context, domainID string) (ingress.Domain, error) {
	row, err := s.st.RQ.GetIngressDomainByID(ctx, domainID)
	if errors.Is(err, sql.ErrNoRows) {
		return ingress.Domain{}, ErrDomainNotFound
	}
	if err != nil {
		return ingress.Domain{}, fmt.Errorf("ingressstore: read the domain %q: %w", domainID, err)
	}
	return domainOf(row), nil
}

// DomainTokenByID opens the DNS token of the domain with this id. It is DomainToken for a caller
// that knows the domain's id but not its environment, with the same rules: ErrDomainNotFound for an
// unknown id, ErrNoToken when none is saved, and an error that names the suffix and no content when
// the sealed value does not open under this domain's id. The token is opened under the domain's own
// id, so a value copied from another row does not open. It is never logged and never put in an error.
func (s *Store) DomainTokenByID(ctx context.Context, domainID string) ([]byte, error) {
	row, err := s.st.RQ.GetIngressDomainByID(ctx, domainID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDomainNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("ingressstore: read the domain %q: %w", domainID, err)
	}
	if row.TokenSealed == "" {
		return nil, ErrNoToken
	}
	token, err := s.v.Open(vault.ContextIngressDNSToken+row.ID, row.TokenSealed)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: open the DNS token of domain %q: %w", row.Suffix, err)
	}
	return token, nil
}

// InstallClaims lists every claim in the install, of every environment, with the names of its
// environment and stack, for the certificate reconciler. A system claim has an empty StackID and
// StackName. The order is the query's.
func (s *Store) InstallClaims(ctx context.Context) ([]ingress.ClaimInfo, error) {
	rows, err := s.st.RQ.ListIngressClaimsInstall(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingressstore: list the claims: %w", err)
	}
	out := make([]ingress.ClaimInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, claimInfoOf(r))
	}
	return out, nil
}
