// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"reflect"
	"testing"
	"time"
)

var alertsNow = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// leftCert is a live certificate with a chain that ends d from alertsNow.
func leftCert(host, source, issuer string, d time.Duration) Cert {
	return Cert{
		ID: "id-" + host, Hostname: host, EnvID: "env-" + host, Source: source, Issuer: issuer, Phase: PhaseIdle,
		ChainPEM: []byte("chain"), NotBefore: alertsNow.Add(d - 90*24*time.Hour), NotAfter: alertsNow.Add(d),
	}
}

func claimedAll(certs ...Cert) map[string]bool {
	m := map[string]bool{}
	for _, c := range certs {
		m[c.Hostname] = true
	}
	return m
}

func TestDeriveAlerts(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	next := alertsNow.Add(20 * time.Minute)

	failing := func(c Cert, n int) Cert {
		c.ConsecutiveFailures, c.LastErrorClass, c.NextAttemptAt = n, "rate_limited", next
		return c
	}
	unref := func(c Cert, since time.Duration) Cert {
		c.UnreferencedSince = alertsNow.Add(-since)
		return c
	}
	retired := func(c Cert) Cert { c.RetiredAt = alertsNow.Add(-time.Hour); return c }

	acmeProd := func(host string, d time.Duration) Cert { return leftCert(host, SourceACME, IssuerProduction, d) }
	acmeStg := func(host string, d time.Duration) Cert { return leftCert(host, SourceACME, IssuerStaging, d) }
	upload := func(host string, d time.Duration) Cert { return leftCert(host, SourceUpload, "", d) }

	expiring := func(c Cert, days, more int) Alert {
		return Alert{Kind: AlertExpiring, Hostname: c.Hostname, EnvID: c.EnvID, Issuer: c.Issuer, Source: c.Source, NotAfter: c.NotAfter, Days: days, More: more}
	}
	failed := func(c Cert, more int) Alert {
		return Alert{
			Kind: AlertRenewalFailed, Hostname: c.Hostname, EnvID: c.EnvID, Issuer: c.Issuer, Source: c.Source,
			Class: "rate_limited", NotAfter: c.NotAfter, NextAttemptAt: next, More: more,
		}
	}

	cases := []struct {
		name    string
		state   State
		certs   []Cert
		claimed map[string]bool // nil claims every certificate
		want    []Alert
	}{
		{name: "15 days left raises nothing", certs: []Cert{acmeProd("a.example.com", 15*day)}},
		{name: "14 days left is the first warning", certs: []Cert{acmeProd("a.example.com", 14*day)},
			want: []Alert{expiring(acmeProd("a.example.com", 14*day), 14, 0)}},
		{name: "8 days left is still the first warning", certs: []Cert{acmeProd("a.example.com", 8*day)},
			want: []Alert{expiring(acmeProd("a.example.com", 8*day), 14, 0)}},
		{name: "7 days left is the second warning", certs: []Cert{acmeProd("a.example.com", 7*day)},
			want: []Alert{expiring(acmeProd("a.example.com", 7*day), 7, 0)}},
		{name: "1 day left is the second warning", certs: []Cert{acmeProd("a.example.com", day)},
			want: []Alert{expiring(acmeProd("a.example.com", day), 7, 0)}},
		{name: "an expired certificate raises no expiry item", certs: []Cert{acmeProd("a.example.com", -time.Hour)}},
		{name: "an upload 10 days out is listed with source upload", certs: []Cert{upload("u.example.com", 10*day)},
			want: []Alert{expiring(upload("u.example.com", 10*day), 14, 0)}},
		{name: "a referenced staging certificate 3 days out raises no expiry item (D-45)", certs: []Cert{acmeStg("s.example.com", 3*day)}},
		{name: "an unreferenced certificate past the grace raises nothing",
			certs:   []Cert{unref(acmeProd("a.example.com", 3*day), Grace+day)},
			claimed: map[string]bool{}},
		{name: "an unreferenced certificate within the grace is still listed",
			certs:   []Cert{unref(acmeProd("a.example.com", 3*day), 2*day)},
			claimed: map[string]bool{},
			want:    []Alert{expiring(acmeProd("a.example.com", 3*day), 7, 0)}},
		{name: "a retired production certificate raises nothing", certs: []Cert{retired(acmeProd("a.example.com", 3*day))}},
		{name: "three certificates within 7 days are one item naming the soonest, more 2",
			certs: []Cert{acmeProd("c.example.com", 6*day), acmeProd("a.example.com", 2*day), upload("b.example.com", 4*day)},
			want:  []Alert{expiring(acmeProd("a.example.com", 2*day), 7, 2)}},
		{name: "a production certificate failing twice raises renewal_failed with its issuer, class and next attempt",
			certs: []Cert{failing(acmeProd("a.example.com", 30*day), 2)},
			want:  []Alert{failed(failing(acmeProd("a.example.com", 30*day), 2), 0)}},
		{name: "two failing certificates are one item with more 1",
			certs: []Cert{failing(acmeProd("b.example.com", 40*day), 1), failing(acmeProd("a.example.com", 30*day), 3)},
			want:  []Alert{failed(failing(acmeProd("a.example.com", 30*day), 3), 1)}},
		{name: "an upload is never renewal_failed", certs: []Cert{failing(upload("u.example.com", 30*day), 2)}},
		{name: "a failing staging certificate raises renewal_failed as staging and no expiry item (D-45)",
			certs: []Cert{failing(acmeStg("s.example.com", 3*day), 2)},
			want:  []Alert{failed(failing(acmeStg("s.example.com", 3*day), 2), 0)}},
		{name: "a failing production certificate near its end raises both, the failure first",
			certs: []Cert{failing(acmeProd("a.example.com", 5*day), 2)},
			want:  []Alert{failed(failing(acmeProd("a.example.com", 5*day), 2), 0), expiring(failing(acmeProd("a.example.com", 5*day), 2), 7, 0)}},
		{name: "a retired failing row raises nothing", certs: []Cert{retired(failing(acmeProd("a.example.com", 30*day), 2))}},
		{name: "a paused restore is one paused item with the reason", state: State{PausedReason: PausedRestore},
			want: []Alert{{Kind: AlertPaused, Reason: PausedRestore}}},
		{name: "not paused and nothing wrong raises nothing", certs: []Cert{acmeProd("a.example.com", 60*day)}},
		{name: "the pause comes first, then the failure, then the expiry",
			state: State{PausedReason: PausedRestore},
			certs: []Cert{failing(acmeProd("a.example.com", 30*day), 1), upload("u.example.com", 3*day)},
			want: []Alert{
				{Kind: AlertPaused, Reason: PausedRestore},
				failed(failing(acmeProd("a.example.com", 30*day), 1), 0),
				expiring(upload("u.example.com", 3*day), 7, 0),
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claimed := tc.claimed
			if claimed == nil {
				claimed = claimedAll(tc.certs...)
			}
			got := DeriveAlerts(alertsNow, tc.state, tc.certs, claimed)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}
