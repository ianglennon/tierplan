// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"slices"
	"time"
)

// AlertKind is what is wrong with HTTPS, for the header bell (phase 17 D-01 to D-04).
type AlertKind string

const (
	// AlertFallback is the self-signed certificate served because the Let's Encrypt one expired.
	AlertFallback AlertKind = "fallback"
	// AlertRenewalFailed is a Let's Encrypt order that failed and is being retried.
	AlertRenewalFailed AlertKind = "renewal_failed"
	// AlertPaused is Let's Encrypt waiting: after a restore, or under a Retry-After or the failure guard.
	AlertPaused AlertKind = "paused"
	// AlertClockOff is the clock measured against the CA to be too far out.
	AlertClockOff AlertKind = "clock_off"
	// AlertExpiring is a served certificate that stops being valid soon.
	AlertExpiring AlertKind = "expiring"
	// AlertTXTLeft is a DNS-01 record left in the zone that could not be removed.
	AlertTXTLeft AlertKind = "txt_left"
)

// rank orders the kinds as the bell lists them (D-04): the worst first.
func (k AlertKind) rank() int {
	switch k {
	case AlertFallback:
		return 0
	case AlertRenewalFailed:
		return 1
	case AlertPaused:
		return 2
	case AlertClockOff:
		return 3
	case AlertExpiring:
		return 4
	case AlertTXTLeft:
		return 5
	default:
		return 6
	}
}

// Alert is one HTTPS problem that needs an instance admin's attention. It is derived from the stored
// state, the served certificate and the clock, never stored, so it is there exactly while its cause is
// (D-02). Every field is a closed-vocabulary fact, a name or a time. The CA's own problem text
// (ErrorView.Detail) is never copied into an Alert: it stays on the HTTPS page (T-17-02).
type Alert struct {
	Kind AlertKind
	// Class, Host, Profile, TermsURL, Zone, Target and Resolvers are the facts of a failed renewal, as
	// ErrorView carries them, without the detail.
	Class, Host, Profile, TermsURL, Zone, Target string
	Resolvers                                    []string
	// NoCertificate is true when the failing order is the first one: there is no Let's Encrypt
	// certificate yet.
	NoCertificate bool
	// NextAttemptAt is when the failed renewal is tried again.
	NextAttemptAt time.Time
	// Reason is, for a pause, PausedRestore, BlockToldToWait or BlockFailureGuard; Until is when a
	// Retry-After or the guard ends.
	Reason string
	Until  time.Time
	// Source, NotAfter are the served certificate's source and expiry, for the expiring item; for the
	// fallback NotAfter is when the Let's Encrypt certificate expired.
	Source   string
	NotAfter time.Time
	// Offset and MeasuredAt are the clock difference to the CA (positive when TierPlan's clock is
	// ahead) and when it was measured.
	Offset     time.Duration
	MeasuredAt time.Time
	// Record, More and RetryPaused describe left-over TXT records: the first one, how many others, and
	// whether the retry has stopped.
	Record      string
	More        int
	RetryPaused bool
}

// ExpiringWarnWithin is the longest time before a served certificate ends that it is listed as expiring
// (D-04.5). A certificate with a lifetime under three times this is listed at a third of its lifetime
// left instead, which lands after the renewal point of a certificate TierPlan renews itself (FallbackRenewAt
// renews at half or two thirds of the lifetime), so a healthy short-lived certificate never raises it.
const ExpiringWarnWithin = 14 * 24 * time.Hour

// Served is the certificate the listener serves, as far as the alerts need it.
type Served struct {
	Source              string
	NotBefore, NotAfter time.Time
}

// DeriveAlerts reports what is wrong with HTTPS at now, the worst first. It is pure: the state, the
// served certificate and the clock come in as arguments.
func DeriveAlerts(now time.Time, st Status, sv Served) []Alert {
	var out []Alert
	if st.Fallback {
		out = append(out, Alert{Kind: AlertFallback, NotAfter: st.ExpiredAt})
	}
	if a, ok := expiring(now, st, sv); ok {
		out = append(out, a)
	}
	if a, ok := renewalFailed(st); ok {
		out = append(out, a)
	}
	if a, ok := paused(st); ok {
		out = append(out, a)
	}
	if a, ok := txtLeft(st); ok {
		out = append(out, a)
	}
	slices.SortStableFunc(out, func(a, b Alert) int { return a.Kind.rank() - b.Kind.rank() })
	return out
}

// renewalFailed is the failed-renewal item (D-04.3): the last order failed, whether it is a renewal on
// an active install or the first issue the environment (TIERPLAN_ACME=on) keeps retrying. An admin's
// manual Issue that fails with no certificate is not one: it is answered on the page.
func renewalFailed(st Status) (Alert, bool) {
	if st.LastError == nil || st.Failures <= 0 || st.CertFiles {
		return Alert{}, false
	}
	firstIssue := st.Certificate == nil && st.LastRun != nil &&
		st.LastRun.Trigger == TriggerSchedule && st.LastRun.Kind == KindIssue
	if st.Mode != ModeActive && !firstIssue {
		return Alert{}, false
	}
	e := st.LastError
	return Alert{
		Kind: AlertRenewalFailed, Class: e.Class, Host: e.Host, Profile: e.Profile, TermsURL: e.TermsURL,
		Zone: e.Zone, Target: e.Target, Resolvers: slices.Clone(e.Resolvers),
		NoCertificate: st.Mode != ModeActive, NextAttemptAt: st.NextAttemptAt,
	}, true
}

// paused is the pause item (D-04.4): Let's Encrypt waits after a restore, or under a CA Retry-After or
// the three-failures guard. It reads neither the mode nor the certificate: a backoff is listed whether or
// not a certificate exists. The pause certificate files cause (14 D-13) is a deliberate configuration and
// gives none.
func paused(st Status) (Alert, bool) {
	if st.CertFiles {
		return Alert{}, false
	}
	switch {
	case st.PausedReason == PausedRestore:
		return Alert{Kind: AlertPaused, Reason: PausedRestore}, true
	case st.Block != nil:
		return Alert{Kind: AlertPaused, Reason: st.Block.Reason, Until: st.Block.Until}, true
	}
	return Alert{}, false
}

// expiring is the expiring item (D-04.5, D-05): the served certificate, from Let's Encrypt or from
// certificate files, has strictly less than min(ExpiringWarnWithin, lifetime/3) left. A certificate
// that has already ended is included: the time left is then negative. While the fallback is listed the
// served certificate is TierPlan's own, and the fallback item says it all. TierPlan's ten-year
// self-signed certificate never crosses the threshold, so it needs no case of its own.
func expiring(now time.Time, st Status, sv Served) (Alert, bool) {
	if st.Fallback || sv.Source == "" || sv.NotAfter.IsZero() {
		return Alert{}, false
	}
	threshold := min(ExpiringWarnWithin, sv.NotAfter.Sub(sv.NotBefore)/3)
	if sv.NotAfter.Sub(now) >= threshold {
		return Alert{}, false
	}
	return Alert{Kind: AlertExpiring, Source: sv.Source, NotAfter: sv.NotAfter}, true
}

// txtLeft is the left-over record item (D-04.6): a TXT record TierPlan created and could not delete. A
// row whose delete has not failed (Class empty) belongs to a run still in flight, so it counts for
// nothing. The item names the oldest such record (dnsView lists them oldest first) and how many others
// there are; neither the record's value nor the provider's ID for it is carried (T-17-08).
func txtLeft(st Status) (Alert, bool) {
	var failed []LeftOverView
	for _, r := range st.DNS.LeftOver {
		if r.Class != "" {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return Alert{}, false
	}
	return Alert{Kind: AlertTXTLeft, Record: failed[0].Name, More: len(failed) - 1, RetryPaused: failed[0].RetryPaused}, true
}

// Alerts reports what is wrong with HTTPS now, for the instance warnings feed. sv is the certificate
// the listener serves.
func (m *Manager) Alerts(ctx context.Context, sv Served) ([]Alert, error) {
	st, err := m.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := DeriveAlerts(m.clock.Now(), st, sv)
	for i := range out {
		if out[i].Kind == AlertRenewalFailed && out[i].Host == "" {
			out[i].Host = canonicalHost(hostOf(m.publicURL()))
		}
	}
	return out, nil
}
