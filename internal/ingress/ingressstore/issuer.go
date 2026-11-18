// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// IssuerChangePreview is what a change of a domain's issuer would do, as an Admin sees it before confirming
// (D-25).
type IssuerChangePreview struct {
	Domain ingress.Domain
	Target ingress.Issuer
	// Certificates is how many live ACME certificates under the domain were ordered from the other issuer: the
	// change retires each of them and orders a new one from Target.
	Certificates int
	// Uploads are the hostnames of the live uploaded certificates under the domain, sorted. An issuer change
	// leaves them alone (D-24); they are listed so the Admin can see that.
	Uploads []string
}

// IssuerCountMismatchError is a change refused because the count the caller confirmed is not the count now: a
// certificate was added or retired since the Admin looked. Current is the count to show again.
type IssuerCountMismatchError struct {
	Current int
}

// Error implements error.
func (e *IssuerCountMismatchError) Error() string {
	return fmt.Sprintf("ingressstore: the number of certificates the issuer change affects is now %d", e.Current)
}

// IssuerHook runs in the transaction of an issuer change, after the domain's row was updated and before the
// commit, so an audit row written there lands with the change or not at all. d is the domain as changed, p the
// preview the change was confirmed against and previous the issuer the domain had.
type IssuerHook func(ctx context.Context, tx *sql.Tx, d ingress.Domain, p IssuerChangePreview, previous ingress.Issuer) error

// preview works out the preview for domain row dom with the queries q, which may belong to a transaction.
func preview(ctx context.Context, q *sqlc.Queries, dom sqlc.IngressDomain, target ingress.Issuer) (IssuerChangePreview, error) {
	n, err := q.CountIngressCertsForIssuerChange(ctx, sqlc.CountIngressCertsForIssuerChangeParams{DomainID: dom.ID, Issuer: string(target)})
	if err != nil {
		return IssuerChangePreview{}, fmt.Errorf("ingressstore: count the certificates of %q that an issuer change affects: %w", dom.Suffix, err)
	}
	uploads, err := q.ListIngressUploadsForDomain(ctx, dom.ID)
	if err != nil {
		return IssuerChangePreview{}, fmt.Errorf("ingressstore: list the uploaded certificates of %q: %w", dom.Suffix, err)
	}
	slices.Sort(uploads)
	return IssuerChangePreview{Domain: domainOf(dom), Target: target, Certificates: int(n), Uploads: slices.Clip(uploads)}, nil
}

// PreviewIssuerChange says what changing the issuer of a domain to target would affect. It writes nothing. A
// domain of another environment is ErrDomainNotFound, like a missing id.
func (s *Store) PreviewIssuerChange(ctx context.Context, envID, domainID string, target ingress.Issuer) (IssuerChangePreview, error) {
	if !target.Valid() {
		return IssuerChangePreview{}, fmt.Errorf("ingressstore: the issuer %q is not staging or production", target)
	}
	row, err := domainInEnv(ctx, s.st.RQ, envID, domainID)
	if err != nil {
		return IssuerChangePreview{}, err
	}
	return preview(ctx, s.st.RQ, row, target)
}

// SetDomainIssuer changes the issuer of a domain. confirmCount is the number of certificates the Admin was shown
// (IssuerChangePreview.Certificates); it is counted again inside the write transaction and a change whose count
// differs is refused with *IssuerCountMismatchError and nothing is written (D-25). A change to the issuer the
// domain already has writes nothing and reports changed false. Otherwise the new issuer is stored, hook (which
// may be nil) runs in the same transaction, the environment's config_version rises and the notifier is told. The
// certificates themselves are not touched here: the certificate manager retires the ones of the other issuer on
// its next round.
func (s *Store) SetDomainIssuer(ctx context.Context, envID, domainID string, target ingress.Issuer, confirmCount int, actor string, hook IssuerHook) (ingress.Domain, bool, error) {
	if !target.Valid() {
		return ingress.Domain{}, false, fmt.Errorf("ingressstore: the issuer %q is not staging or production", target)
	}
	now := s.now().UTC()
	var (
		out     ingress.Domain
		changed bool
	)
	err := s.write(ctx, envID, actor, now, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		row, err := domainInEnv(ctx, q, envID, domainID)
		if err != nil {
			return false, err
		}
		if ingress.Issuer(row.Issuer) == target {
			out = domainOf(row)
			return false, nil
		}
		p, err := preview(ctx, q, row, target)
		if err != nil {
			return false, err
		}
		if p.Certificates != confirmCount {
			return false, &IssuerCountMismatchError{Current: p.Certificates}
		}
		n, err := q.SetIngressDomainIssuer(ctx, sqlc.SetIngressDomainIssuerParams{
			Issuer: string(target), UpdatedAt: toNanos(now), ID: row.ID, EnvID: envID,
		})
		if err != nil {
			return false, fmt.Errorf("ingressstore: change the issuer of domain %q: %w", row.Suffix, err)
		}
		if n == 0 {
			return false, ErrDomainNotFound
		}
		previous := ingress.Issuer(row.Issuer)
		row.Issuer, row.UpdatedAt = string(target), toNanos(now)
		out = domainOf(row)
		p.Domain = out
		if hook != nil {
			if err := hook(ctx, tx, out, p, previous); err != nil {
				return false, err
			}
		}
		changed = true
		return true, nil
	})
	if err != nil {
		return ingress.Domain{}, false, err
	}
	return out, changed, nil
}
