// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"slices"
	"strings"
	"time"
)

// AlertKind is what needs an instance admin's attention among the per-hostname certificates, for the header bell
// (D-10, D-32). Facts only: an Alert carries names, times, a failure class and counts, never the certificate
// authority's own text (T-17-02). Alerts are derived on every read from the stored rows, the install state and
// the clock, and are never stored, so one goes with its cause and there is nothing to acknowledge.
type AlertKind string

const (
	// AlertExpiring is a certificate that will be served and ends within ExpiringWarnFirst.
	AlertExpiring AlertKind = "cert_expiring"
	// AlertRenewalFailed is an ACME certificate whose orders keep failing.
	AlertRenewalFailed AlertKind = "cert_renewal_failed"
	// AlertPaused is issuing paused for the whole install; Reason says why.
	AlertPaused AlertKind = "certs_paused"
)

// rank orders the kinds as the bell lists them: the install-wide pause, then failing renewals, then expiry.
func (k AlertKind) rank() int {
	switch k {
	case AlertPaused:
		return 0
	case AlertRenewalFailed:
		return 1
	case AlertExpiring:
		return 2
	default:
		return 3
	}
}

const (
	// ExpiringWarnFirst is how long before it ends a certificate is first listed as expiring (D-10, CRT-04).
	ExpiringWarnFirst = 14 * 24 * time.Hour
	// ExpiringWarnSecond is how long before it ends the listing is raised to its second, stronger warning.
	ExpiringWarnSecond = 7 * 24 * time.Hour
)

// Alert is one item for the bell. One Alert stands for every certificate of its kind: it names the most urgent
// hostname and More counts the others, so many certificates never flood the bell.
type Alert struct {
	Kind AlertKind
	// Hostname and EnvID name the most urgent certificate of the kind.
	Hostname, EnvID string
	// Issuer is staging or production for an ACME certificate; empty for an upload.
	Issuer string
	// Source is acme or upload.
	Source string
	// Class is, for a failed renewal, the stored failure class.
	Class string
	// Reason is, for a pause, why issuing is paused.
	Reason string
	// NotAfter is when the certificate ends; NextAttemptAt is when the failed order is tried again.
	NotAfter, NextAttemptAt time.Time
	// Days is, for an expiring item, the warning reached: 14 or 7.
	Days int
	// More is how many other certificates the same kind covers.
	More int
}

// DeriveAlerts reports what needs attention at now, in the bell's order. It is pure: the install state, the live
// certificates and the claimed hostnames (a hostname in claimed is claimed by some environment) come in as
// arguments.
//
// Expiry is raised for a certificate that will be served: a production ACME certificate or an upload, still
// valid, within ExpiringWarnFirst of its end, and not unreferenced for longer than Grace. A staging certificate
// is never servable (CRT-09, D-20), so its expiry is no serving risk and raises nothing (D-45). A failing
// renewal is raised for an ACME certificate of either issuer, staging included, since the failure is real and
// worth knowing about (D-45).
func DeriveAlerts(now time.Time, st State, certs []Cert, claimed map[string]bool) []Alert {
	var out []Alert
	if st.PausedReason != "" {
		out = append(out, Alert{Kind: AlertPaused, Reason: st.PausedReason})
	}

	var failing, expiring []Cert
	for _, c := range certs {
		if !c.RetiredAt.IsZero() || PastGrace(c, claimed[c.Hostname], now) {
			continue
		}
		if c.Source == SourceACME && c.ConsecutiveFailures > 0 && c.Phase != PhaseStopped {
			failing = append(failing, c)
		}
		servable := c.Source == SourceUpload || (c.Source == SourceACME && c.Issuer == IssuerProduction)
		if !servable || len(c.ChainPEM) == 0 || c.NotAfter.IsZero() {
			continue
		}
		if left := c.NotAfter.Sub(now); left > 0 && left <= ExpiringWarnFirst {
			expiring = append(expiring, c)
		}
	}

	if len(failing) > 0 {
		slices.SortFunc(failing, urgency)
		c := failing[0]
		out = append(out, Alert{
			Kind: AlertRenewalFailed, Hostname: c.Hostname, EnvID: c.EnvID, Issuer: c.Issuer, Source: c.Source,
			Class: c.LastErrorClass, NotAfter: c.NotAfter, NextAttemptAt: c.NextAttemptAt, More: len(failing) - 1,
		})
	}
	if len(expiring) > 0 {
		slices.SortFunc(expiring, urgency)
		c := expiring[0]
		days := 14
		if c.NotAfter.Sub(now) <= ExpiringWarnSecond {
			days = 7
		}
		out = append(out, Alert{
			Kind: AlertExpiring, Hostname: c.Hostname, EnvID: c.EnvID, Issuer: c.Issuer, Source: c.Source,
			NotAfter: c.NotAfter, Days: days, More: len(expiring) - 1,
		})
	}
	slices.SortStableFunc(out, func(a, b Alert) int { return a.Kind.rank() - b.Kind.rank() })
	return out
}

// urgency orders certificates most urgent first: the earliest end, a row without a chain after every row with
// one, then the hostname so the choice is the same on every read.
func urgency(a, b Cert) int {
	switch {
	case a.NotAfter.IsZero() && !b.NotAfter.IsZero():
		return 1
	case !a.NotAfter.IsZero() && b.NotAfter.IsZero():
		return -1
	case a.NotAfter.Before(b.NotAfter):
		return -1
	case a.NotAfter.After(b.NotAfter):
		return 1
	}
	return strings.Compare(a.Hostname, b.Hostname)
}

// Alerts reports what needs attention among the certificates now, for the instance warnings feed. The state, the
// live certificates and the claims are read on each call.
func (m *Manager) Alerts(ctx context.Context) ([]Alert, error) {
	st, err := m.d.Store.State(ctx)
	if err != nil {
		return nil, err
	}
	certs, err := m.d.Store.LiveCerts(ctx)
	if err != nil {
		return nil, err
	}
	claims, err := m.d.Domains.InstallClaims(ctx)
	if err != nil {
		return nil, err
	}
	claimed := make(map[string]bool, len(claims))
	for _, c := range claims {
		claimed[c.Hostname] = true
	}
	return DeriveAlerts(m.clock.Now(), st, certs, claimed), nil
}
