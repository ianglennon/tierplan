// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"fmt"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// RegisteredDomain returns the registered domain of hostname: the public suffix plus one label (app.example.com
// gives example.com, app.example.co.uk gives example.co.uk). It is the key the certificate authority's per-domain
// rate limits are counted by, so the issuance ledger and the governor share it. The public suffix list used
// includes the private suffixes, which is what Boulder keys its limits by (RESEARCH S8): a hostname under a
// private suffix such as github.io counts against its own registered domain, not against the suffix. A hostname
// that is itself a public suffix, or has no suffix, is an error.
func RegisteredDomain(hostname string) (string, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return "", fmt.Errorf("ingresscert: the registered domain of %q: %w", hostname, err)
	}
	return domain, nil
}
