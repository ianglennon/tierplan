// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// hints is what a person is told about each class of failure: one fixed sentence per class, never the certificate
// authority's or the DNS provider's own words (T-17-02). The class is the stored contract; these sentences are
// copy and may be reworded.
var hints = map[string]string{
	acme.ClassRateLimited:           "The certificate authority is limiting how often TierPlan may ask. TierPlan tries again once it allows.",
	acme.ClassValidationUnreachable: "The certificate authority could not reach the host to prove it is yours.",
	acme.ClassValidationDNS:         "The certificate authority could not look up the host's DNS records.",
	acme.ClassCAAForbids:            "A CAA record on the domain does not allow this certificate authority to issue.",
	acme.ClassIdentifierRejected:    "The certificate authority will not issue a certificate for this hostname.",
	acme.ClassContactInvalid:        "The certificate authority rejected the contact email of the ACME account.",
	acme.ClassTOSActionRequired:     "The certificate authority needs its terms of service accepted again.",
	acme.ClassAccountGone:           "The certificate authority no longer knows the ACME account. TierPlan registers a new one for the next try.",
	acme.ClassProfileInvalid:        "The certificate authority does not offer the certificate profile TierPlan asked for.",
	acme.ClassCAUnavailable:         "The certificate authority had an error of its own. TierPlan tries again later.",
	acme.ClassCAUntrusted:           "TierPlan does not trust the certificate authority's own certificate.",
	acme.ClassInternal:              "The order failed for a reason TierPlan could not classify. The log has the details.",
	acme.ClassDNSTokenInvalid:       "The DNS provider refused the domain's API token. Replace the token on the domain.",
	acme.ClassDNSTokenScope:         "The domain's API token is not allowed to edit the DNS zone. Give it permission to edit DNS records.",
	acme.ClassDNSZoneNotFound:       "The domain's API token cannot see the DNS zone of the hostname. Check the token's zone access.",
	acme.ClassDNSDelegationTarget:   "The challenge name is a CNAME whose target zone TierPlan cannot edit with the domain's token.",
	acme.ClassDNSNotVisible:         "The challenge record was written but did not appear in DNS in time.",
	acme.ClassDNSRateLimited:        "The DNS provider is limiting how often TierPlan may change records. TierPlan tries again later.",
	acme.ClassDNSUnavailable:        "The DNS provider could not be reached or had an error. TierPlan tries again later.",
	acme.ClassValidationTXT:         "The certificate authority did not find the challenge record in DNS.",
}

// genericHint is what a class this build does not know is told.
const genericHint = "The certificate order failed. TierPlan tries again later."

// FailureHint is the fixed sentence for an acme failure class. A class it does not know gets a generic sentence;
// it never returns text from outside the program.
func FailureHint(class string) string {
	if h, ok := hints[class]; ok {
		return h
	}
	return genericHint
}

// tokenFailure reports whether a class is the domain's DNS token failing: refused, out of scope, or unable to see
// the zone. Every certificate of the domain would meet it, so one failure holds them all (D-11).
func tokenFailure(class string) bool {
	switch class {
	case acme.ClassDNSTokenInvalid, acme.ClassDNSTokenScope, acme.ClassDNSZoneNotFound:
		return true
	}
	return false
}

// recordFailure records a failed order on the certificate: the class, the capped detail for the Admin API, the
// ledger moved on by acme.RecordFailure (backoff, the CA's Retry-After), and the newest failure instants the
// manual guard counts. The row waits queued for its next attempt with its chain untouched. A token failure also
// holds every other queued or idle certificate of the domain until the same instant. The audit row says the
// class and nothing else about why. All of it is one write.
func (m *Manager) recordFailure(ctx context.Context, c Cert, f acme.Failure, now time.Time) error {
	lifetime := time.Duration(0)
	if len(c.ChainPEM) > 0 && c.NotAfter.After(c.NotBefore) {
		lifetime = c.NotAfter.Sub(c.NotBefore)
	}
	l := acme.RecordFailure(LedgerOf(c), now, f.RetryAfter, lifetime, m.d.Jitter())
	return m.d.Store.writeFailure(ctx, c, f, l, failureTimesAfter(c.FailureTimes, now), now)
}

// failureTimesAfter is the failure instants with now added, newest acme.FailureGuardRuns of them, oldest first.
func failureTimesAfter(times []time.Time, now time.Time) []time.Time {
	out := append(slices.Clone(times), now)
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	if len(out) > acme.FailureGuardRuns {
		out = out[len(out)-acme.FailureGuardRuns:]
	}
	return out
}

// encodeTimes is the stored form of a list of instants: a JSON array of unix nanoseconds.
func encodeTimes(times []time.Time) (string, error) {
	nanos := make([]int64, 0, len(times))
	for _, t := range times {
		nanos = append(nanos, toNanos(t))
	}
	b, err := json.Marshal(nanos)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// writeFailure stores a failed run on row c (see Manager.recordFailure) and writes the ingress.cert_failed audit
// row as system:ingress in the same transaction. Nothing here changes what is served, so it raises no
// certs_version.
func (s *Store) writeFailure(ctx context.Context, c Cert, f acme.Failure, l acme.Ledger, times []time.Time, now time.Time) error {
	ts := toNanos(now)
	encoded, err := encodeTimes(times)
	if err != nil {
		return fmt.Errorf("ingresscert: encode the failure times of certificate %q: %w", c.ID, err)
	}
	return s.write(ctx, nil, func(tx *sql.Tx, q *sqlc.Queries) (bool, error) {
		if err := q.RecordIngressCertFailure(ctx, sqlc.RecordIngressCertFailureParams{
			LastErrorClass: f.Class, LastErrorAt: ts, LastErrorDetail: f.Detail,
			ConsecutiveFailures: int64(l.ConsecutiveFailures), LastAttemptAt: toNanos(l.LastAttemptAt),
			NextAttemptAt: toNanos(l.NextAttemptAt), RetryAfterUntil: toNanos(l.RetryAfterUntil),
			FailureTimes: encoded, UpdatedAt: ts, ID: c.ID,
		}); err != nil {
			return false, fmt.Errorf("ingresscert: record the failure of certificate %q: %w", c.ID, err)
		}
		if tokenFailure(f.Class) {
			if err := q.HoldIngressCertsOfDomain(ctx, sqlc.HoldIngressCertsOfDomainParams{
				Until: toNanos(l.NextAttemptAt), UpdatedAt: ts, DomainID: c.DomainID,
			}); err != nil {
				return false, fmt.Errorf("ingresscert: hold the certificates of domain %q: %w", c.DomainID, err)
			}
		}
		if s.audit != nil {
			if err := s.audit.RecordTx(ctx, tx, audit.Entry{
				Actor: audit.SystemIngress, Action: audit.ActionIngressCertFailed, TargetType: auditTargetCertificate,
				TargetID: c.ID, EnvID: c.EnvID, Result: audit.ResultFailure,
				Details: map[string]string{"hostname": c.Hostname, "issuer": c.Issuer, "reason": f.Class},
			}); err != nil {
				return false, fmt.Errorf("ingresscert: record the failure of %q: %w", c.Hostname, err)
			}
		}
		return false, nil
	})
}

// forgetAccount drops the ingress account of the certificate's directory, which the CA says it no longer knows.
// The next run registers a new one (the terms are already acknowledged, or the row would not have run).
func (m *Manager) forgetAccount(ctx context.Context, c Cert) {
	client, dir := m.client(c.Issuer)
	if client == nil {
		return
	}
	if err := client.Forget(ctx); err != nil {
		m.log.Error("a certificate run could not forget the account the certificate authority no longer knows", "host", c.Hostname, "error", err)
		return
	}
	m.log.Warn("the certificate authority no longer knows the ingress account, which was forgotten", "directory", acme.DirectoryHost(dir.URL))
}
