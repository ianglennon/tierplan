// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// TestDirectAddressIsAnOriginTierPlanAccepts: the direct address the card offers is one TierPlan accepts changes from
// (D-07, D-10). It is the public URL's origin while the public URL is not the route host, and the agent URL's once an
// Admin has moved the public URL to the route host, which Settings allow only while such an agent URL is set. It is empty,
// the page's own origin, only where it was before: no URL off the route host at all.
func TestDirectAddressIsAnOriginTierPlanAccepts(t *testing.T) {
	cases := []struct {
		name string
		urls ingressdeploy.SelfURLs
		want string
	}{
		{name: "public URL off the route host, no agent URL", urls: ingressdeploy.SelfURLs{PublicURL: srPublic}, want: srPublic},
		{name: "public URL off the route host, another agent URL", urls: ingressdeploy.SelfURLs{PublicURL: srPublic, AgentURL: srAgent}, want: srPublic},
		{name: "public URL with a path", urls: ingressdeploy.SelfURLs{PublicURL: srPublic + "/tierplan", AgentURL: srAgent}, want: srPublic},
		{name: "public URL on the route host, agent URL set", urls: ingressdeploy.SelfURLs{PublicURL: "https://" + srHost, AgentURL: srAgent}, want: srAgent},
		{name: "public URL on the route host in other case", urls: ingressdeploy.SelfURLs{PublicURL: "https://TP.Apps.Example.Org.", AgentURL: srAgent}, want: srAgent},
		{name: "public URL on the route host, no agent URL", urls: ingressdeploy.SelfURLs{PublicURL: "https://" + srHost}, want: ""},
		{name: "no public URL, agent URL set", urls: ingressdeploy.SelfURLs{AgentURL: srAgent}, want: srAgent},
		{name: "no URL at all", urls: ingressdeploy.SelfURLs{}, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ingressdeploy.DirectAddress(c.urls, srHost); got != c.want {
				t.Errorf("DirectAddress(%+v) = %q, want %q", c.urls, got, c.want)
			}
		})
	}
}
