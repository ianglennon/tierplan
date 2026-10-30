// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"slices"
	"strings"
	"time"
)

// AlertKind is what is wrong with a credential, for the header bell (D-07).
type AlertKind string

const (
	// AlertExpiring is a credential that stops working within ExpiryWarning.
	AlertExpiring AlertKind = "expiring"
	// AlertExpired is a credential whose expiry day has passed.
	AlertExpired AlertKind = "expired"
	// AlertAuthFailed is a credential its registry refused on the last use and that has not
	// worked since.
	AlertAuthFailed AlertKind = "auth_failed"
)

// ExpiryWarning is how long before its expiry a credential starts to be reported. It is measured
// from the same instant that decides Expired: the end of the expiry day in UTC.
const ExpiryWarning = 7 * 24 * time.Hour

// severity orders the kinds: expired is the worst, then a refusal, then an expiry to come.
func (k AlertKind) severity() int {
	switch k {
	case AlertExpired:
		return 3
	case AlertAuthFailed:
		return 2
	default:
		return 1
	}
}

// Alert is one credential that needs its managers' attention. It is derived from the stored
// credential and the clock, never stored, so it is there exactly while its cause is.
type Alert struct {
	Kind         AlertKind
	CredentialID string
	Name         string
	Host         string
	Prefix       string
	// Layer is LayerInstance or LayerEnvironment.
	Layer string
	// EnvID is the environment of an environment credential. EnvName is left for the caller to fill.
	EnvID   string
	EnvName string
	// Days is, for AlertExpiring, the whole days left, rounded up, so a credential that expires
	// later today reads 1. It is 0 for the other kinds.
	Days int
	// At is when the credential expires (expiring, expired) or when the registry refused it
	// (auth_failed).
	At time.Time
}

// Alerts reports the credentials that need attention at now, the most severe first and by name
// within a kind. Several problems with one credential collapse to the most severe. An expiry day
// counts as the end of that day in UTC (UI-SPEC default 17); the warning and Expired use that
// same instant. A refusal is forgotten once a test passed after it (a pull that worked clears the
// stored failure itself, see Store.ClearFailure). Nothing is renewed automatically.
func Alerts(now time.Time, creds []Credential) []Alert {
	var out []Alert
	for _, c := range creds {
		a, ok := alertOf(now, c)
		if ok {
			out = append(out, a)
		}
	}
	slices.SortStableFunc(out, func(a, b Alert) int {
		if d := b.Kind.severity() - a.Kind.severity(); d != 0 {
			return d
		}
		if d := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); d != 0 {
			return d
		}
		return strings.Compare(a.CredentialID, b.CredentialID)
	})
	return out
}

// alertOf is the most severe problem of one credential.
func alertOf(now time.Time, c Credential) (Alert, bool) {
	a := Alert{
		CredentialID: c.ID, Name: c.Name, Host: c.Host, Prefix: c.Prefix, Layer: c.Layer(), EnvID: c.EnvID,
	}
	if c.ExpiresAt != nil && now.After(*c.ExpiresAt) {
		a.Kind, a.At = AlertExpired, *c.ExpiresAt
		return a, true
	}
	if f := c.LastFailure; f != nil {
		worked := c.LastTest != nil && c.LastTest.OK && c.LastTest.At.After(f.At)
		if !worked {
			a.Kind, a.At = AlertAuthFailed, f.At
			return a, true
		}
	}
	if c.ExpiresAt != nil {
		if left := c.ExpiresAt.Sub(now); left <= ExpiryWarning {
			a.Kind, a.At = AlertExpiring, *c.ExpiresAt
			a.Days = max(1, int((left+24*time.Hour-1)/(24*time.Hour)))
			return a, true
		}
	}
	return Alert{}, false
}
