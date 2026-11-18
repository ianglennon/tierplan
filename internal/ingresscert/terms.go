// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// State is the install-wide state of the per-hostname certificates (the singleton ingress_cert_state row).
// Every instant is the zero time.Time when it is not set; the zero State is a fresh install: nothing paused and
// the terms of the certificate authority not yet accepted.
type State struct {
	// PausedReason is why issuing is paused ("restore" after a restore); empty when it is not.
	PausedReason string
	// TermsAcceptedAt and TermsAcceptedBy record who acknowledged the CA's terms and when (D-36). An ingress
	// ACME account is only created after this.
	TermsAcceptedAt time.Time
	TermsAcceptedBy string
	UpdatedAt       time.Time
}

// State returns the install-wide certificate state.
func (s *Store) State(ctx context.Context) (State, error) {
	row, err := s.st.RQ.GetIngressCertState(ctx)
	if err != nil {
		return State{}, fmt.Errorf("ingresscert: read the certificate state: %w", err)
	}
	return State{
		PausedReason:    row.PausedReason,
		TermsAcceptedAt: fromNanos(row.TermsAcceptedAt),
		TermsAcceptedBy: row.TermsAcceptedBy,
		UpdatedAt:       fromNanos(row.UpdatedAt),
	}, nil
}

// AcceptTerms records that actor acknowledged the certificate authority's terms for the whole install. hook,
// which may be nil, runs in the same transaction after the write and an error from it rolls the write back: the
// API records https.ingress_terms_accepted there, so the acknowledgement never exists without its audit row.
func (s *Store) AcceptTerms(ctx context.Context, actor string, hook func(ctx context.Context, tx *sql.Tx) error) error {
	now := toNanos(s.now())
	// Accepting terms is not a change to any certificate, so it raises no certs_version.
	return s.write(ctx, nil, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.AcceptIngressCertTerms(ctx, sqlc.AcceptIngressCertTermsParams{
			TermsAcceptedAt: now, TermsAcceptedBy: actor, UpdatedAt: now,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: record the acceptance of the terms: %w", err)
		}
		if hook != nil {
			if err := hook(ctx, tx); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}
