// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"errors"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// reconcile is a scan over the claims of the install and the live certificates: a claimed hostname that has no
// live certificate gets one queued row, in the claim's environment and under the claim's domain, with the
// issuer of that domain (D-39). The environment's ingress switch is not read: a claim in an environment whose
// ingress is off still gets a certificate (D-40). A row created here is not a request ahead.
func (m *Manager) reconcile(ctx context.Context) {
	claims, err := m.d.Domains.InstallClaims(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the claims", "error", err)
		return
	}
	live, err := m.d.Store.LiveCerts(ctx)
	if err != nil {
		m.log.Error("the certificate manager could not list the certificates", "error", err)
		return
	}
	have := make(map[string]struct{}, len(live))
	for _, c := range live {
		have[c.Hostname] = struct{}{}
	}
	for _, claim := range claims {
		if ctx.Err() != nil {
			return
		}
		if _, ok := have[claim.Hostname]; ok {
			continue
		}
		dom, err := m.d.Domains.DomainByID(ctx, claim.DomainID)
		if err != nil {
			if !errors.Is(err, ingressstore.ErrDomainNotFound) {
				m.log.Error("the certificate manager could not read the domain of a claim", "host", claim.Hostname, "error", err)
			}
			continue
		}
		if _, _, err := m.d.Store.CreatePending(ctx, PendingInput{
			Hostname: claim.Hostname, EnvID: claim.EnvID, DomainID: claim.DomainID, Issuer: string(dom.Issuer),
			CreatedBy: JobOwner, NotRequested: true,
		}, nil); err != nil {
			m.log.Error("the certificate manager could not queue a certificate", "host", claim.Hostname, "error", err)
		}
	}
}
