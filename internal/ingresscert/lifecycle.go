// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import "time"

// Grace is how long a certificate nobody refers to is kept, and how long a request-ahead holds its row
// (D-06, D-08). It is measured from the request for a requested row and from the release for a released one.
const Grace = 30 * 24 * time.Hour

// RequestHeld reports whether c is held by a request-ahead at now: an administrator asked for the
// certificate ahead of any claim (RequestedAt is set) and no more than Grace has passed since the request.
// While it holds, no other environment may take the row over (D-08, D-21). It is pure: now is the caller's
// clock.
func RequestHeld(c Cert, now time.Time) bool {
	if c.RequestedAt.IsZero() {
		return false
	}
	return !now.After(c.RequestedAt.Add(Grace))
}

// The stopped_reason of a row the lifecycle stops.
const (
	// StoppedUnreferenced is a certificate nothing has referred to for longer than Grace (D-06). It is never
	// renewed, and a claim or a request that refers to it again brings it back.
	StoppedUnreferenced = "unreferenced"
	// StoppedDomainRemoved is a certificate whose domain no longer exists (D-12). It is never retried; a claim that
	// resolves the hostname to a live domain re-points the row and clears the stop.
	StoppedDomainRemoved = StopDomainRemoved
)

// RetiredDomainChanged is the retired_reason of a row whose hostname's claim moved to a domain that cannot reuse
// it (another issuer, an expired chain, a key that does not open): the row stops being the live one and a new row
// is ordered under the claim's domain (D-04).
const RetiredDomainChanged = "domain_changed"

// RetiredIssuerChanged is the retired_reason of a row whose domain's issuer was changed to the other one (D-26): the
// certificate was ordered from an issuer the domain no longer uses, so it is never renewed or reused. The retired-row
// rules delete it like any other retired row: a staging one at once, a production one when its replacement is ready or
// it expires.
const RetiredIssuerChanged = "issuer_changed"

// Why a row was deleted, as the ingress.cert_deleted audit row says it.
const (
	// DeletedExpiredUnreferenced is an expired certificate nothing has referred to for longer than Grace (D-07).
	DeletedExpiredUnreferenced = "expired_unreferenced"
	// DeletedRetiredExpired is a retired certificate that has expired.
	DeletedRetiredExpired = "retired_expired"
	// DeletedReplaced is a retired certificate whose hostname's live certificate is ready.
	DeletedReplaced = "replaced"
	// DeletedStagingRetired is a retired staging certificate, which serves nothing and is not kept.
	DeletedStagingRetired = "staging_retired"
	// DeletedDomainRemoved is a row that held no certificate and whose domain was removed (D-12).
	DeletedDomainRemoved = "domain_removed"
	// DeletedNeverIssued is a row that never held a certificate and that nothing refers to any more.
	DeletedNeverIssued = "never_issued"
)

// Referenced reports whether something refers to certificate c's hostname at now: a claim does, and so does a
// request-ahead that Grace has not yet outlived (D-06, D-08). It is pure.
func Referenced(c Cert, claimed bool, now time.Time) bool {
	return claimed || RequestHeld(c, now)
}

// PastGrace reports whether certificate c has been unreferenced for longer than Grace at now (D-06): the hostname
// is not claimed, and more than Grace has passed since the later of unreferenced_since and requested_at, so a
// new request starts the time again. A row that has neither instant has not begun to count. It is pure.
func PastGrace(c Cert, claimed bool, now time.Time) bool {
	if claimed {
		return false
	}
	since := c.UnreferencedSince
	if c.RequestedAt.After(since) {
		since = c.RequestedAt
	}
	if since.IsZero() {
		return false
	}
	return now.Sub(since) > Grace
}

// Reusable reports whether row c may be handed to a claim whose domain has issuer, in place of ordering a new
// certificate (D-04): an upload may (it names no issuer and serves whoever holds the hostname), and an ACME row
// may when it was ordered from the same issuer and holds either no certificate yet or one that has not expired
// and whose key opens. keyOpens is worked out by the caller, so the rule needs no vault. It is pure.
func Reusable(c Cert, issuer string, now time.Time, keyOpens bool) bool {
	if c.Source == SourceUpload {
		return true
	}
	if c.Source != SourceACME || c.Issuer != issuer {
		return false
	}
	if len(c.ChainPEM) == 0 {
		return true
	}
	return c.NotAfter.After(now) && keyOpens
}
