// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"testing"
	"time"
)

// D-06, D-08 and D-04: the pure rules of the lifecycle, with every boundary pinned on an injected instant.
func TestLifecycleRules(t *testing.T) {
	now := time.Date(2026, time.March, 10, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	t.Run("referenced, held and past grace", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			c              Cert
			claimed        bool
			held           bool
			referenced     bool
			pastGraceAtNow bool
		}{
			{name: "claimed is referenced and never past grace", c: Cert{UnreferencedSince: now.Add(-90 * day)}, claimed: true, referenced: true},
			{name: "requested 29 days ago is held and referenced", c: Cert{RequestedAt: now.Add(-29 * day)}, held: true, referenced: true},
			{name: "requested exactly 30 days ago is still held", c: Cert{RequestedAt: now.Add(-Grace)}, held: true, referenced: true},
			{name: "requested 31 days ago is neither held nor referenced", c: Cert{RequestedAt: now.Add(-31 * day)}, pastGraceAtNow: true},
			{name: "never requested is not held", c: Cert{}},
			{name: "unreferenced for exactly the grace is not past it", c: Cert{UnreferencedSince: now.Add(-Grace)}},
			{name: "unreferenced for the grace and a minute is past it", c: Cert{UnreferencedSince: now.Add(-Grace - time.Minute)}, pastGraceAtNow: true},
			{name: "unreferenced 29 days is not past the grace", c: Cert{UnreferencedSince: now.Add(-29 * day)}},
			{
				name: "a later request moves the start",
				c:    Cert{UnreferencedSince: now.Add(-40 * day), RequestedAt: now.Add(-10 * day)}, held: true, referenced: true,
			},
			{
				name: "a request that has lapsed measures from the later of the two",
				c:    Cert{UnreferencedSince: now.Add(-5 * day), RequestedAt: now.Add(-40 * day)},
			},
			{name: "a row that has neither instant has not begun to count", c: Cert{}},
		} {
			if got := RequestHeld(tc.c, now); got != tc.held {
				t.Errorf("%s: RequestHeld = %v, want %v", tc.name, got, tc.held)
			}
			if got := Referenced(tc.c, tc.claimed, now); got != tc.referenced {
				t.Errorf("%s: Referenced = %v, want %v", tc.name, got, tc.referenced)
			}
			if got := PastGrace(tc.c, tc.claimed, now); got != tc.pastGraceAtNow {
				t.Errorf("%s: PastGrace = %v, want %v", tc.name, got, tc.pastGraceAtNow)
			}
		}
	})

	t.Run("reusable", func(t *testing.T) {
		valid := func(c Cert) Cert {
			c.Source, c.Issuer = SourceACME, IssuerProduction
			c.ChainPEM = []byte("chain")
			c.NotBefore, c.NotAfter = now.Add(-80*day), now.Add(10*day)
			return c
		}
		for _, tc := range []struct {
			name     string
			c        Cert
			issuer   string
			keyOpens bool
			want     bool
		}{
			{name: "an upload, whatever the issuer", c: Cert{Source: SourceUpload, ChainPEM: []byte("c")}, issuer: IssuerStaging, want: true},
			{name: "an expired upload", c: Cert{Source: SourceUpload, ChainPEM: []byte("c"), NotAfter: now.Add(-day)}, issuer: IssuerProduction, want: true},
			{name: "ACME of the same issuer valid for 10 more days with a key that opens", c: valid(Cert{}), issuer: IssuerProduction, keyOpens: true, want: true},
			{name: "ACME of the same issuer with no chain", c: Cert{Source: SourceACME, Issuer: IssuerProduction}, issuer: IssuerProduction, want: true},
			{name: "ACME of the same issuer with no chain and a key that does not open", c: Cert{Source: SourceACME, Issuer: IssuerProduction}, issuer: IssuerProduction, want: true},
			{name: "ACME of another issuer", c: valid(Cert{}), issuer: IssuerStaging, keyOpens: true},
			{name: "ACME of another issuer with no chain", c: Cert{Source: SourceACME, Issuer: IssuerStaging}, issuer: IssuerProduction},
			{name: "ACME of the same issuer past not_after", c: func() Cert { c := valid(Cert{}); c.NotAfter = now.Add(-time.Minute); return c }(), issuer: IssuerProduction, keyOpens: true},
			{name: "ACME of the same issuer expiring this instant", c: func() Cert { c := valid(Cert{}); c.NotAfter = now; return c }(), issuer: IssuerProduction, keyOpens: true},
			{name: "ACME of the same issuer whose key does not open", c: valid(Cert{}), issuer: IssuerProduction},
		} {
			if got := Reusable(tc.c, tc.issuer, now, tc.keyOpens); got != tc.want {
				t.Errorf("%s: Reusable = %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}
