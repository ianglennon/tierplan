// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressstore

import (
	"context"
	"strings"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// InstallClaimHosts is every claim in the install, of every environment, keyed by its lower-cased
// hostname: the self route's system claim and every stack's. It is what the settings routes check
// an agent URL and a public URL against, so neither can name a hostname the ingress serves (D-10).
// It reads the same rows as InstallClaims; a hostname has at most one claim in the install.
func (s *Store) InstallClaimHosts(ctx context.Context) (map[string]ingress.ClaimInfo, error) {
	claims, err := s.InstallClaims(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ingress.ClaimInfo, len(claims))
	for _, c := range claims {
		out[strings.ToLower(c.Hostname)] = c
	}
	return out, nil
}
