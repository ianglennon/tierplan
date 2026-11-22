// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package config_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
)

// TestIngressEndpoint pins the address the ingress proxy dials (D-08): the host, port and base path of the configured agent
// or public URL, https as wss, with /ingress/connect on the end. It shares every rule of AgentEndpoint, so a base that one
// refuses the other refuses with the same reason, and an empty base is an error rather than an address with no host.
func TestIngressEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, base, want string }{
		{"a host and port", "https://tierplan.example:7480", "wss://tierplan.example:7480/ingress/connect"},
		{"a host alone", "https://tierplan.example", "wss://tierplan.example/ingress/connect"},
		{"a base path keeps the host and port", "https://example.com:8443/tierplan", "wss://example.com:8443/tierplan/ingress/connect"},
		{"a trailing slash is dropped", "https://example.com/tierplan/", "wss://example.com/tierplan/ingress/connect"},
		{"the scheme's case does not matter", "HTTPS://example.com", "wss://example.com/ingress/connect"},
		{"surrounding space is dropped", "  https://example.com  ", "wss://example.com/ingress/connect"},
	} {
		got, err := config.IngressEndpoint(tc.base)
		if err != nil || got != tc.want {
			t.Errorf("%s: IngressEndpoint(%q) = %q, %v; want %q", tc.name, tc.base, got, err, tc.want)
		}
		// The agent address of the same base differs only by its path.
		if agent, aerr := config.AgentEndpoint(tc.base); aerr != nil || agent == got {
			t.Errorf("%s: AgentEndpoint(%q) = %q, %v; want an address that differs from the ingress one", tc.name, tc.base, agent, aerr)
		}
	}

	for _, base := range []string{
		"",
		"   ",
		"http://tierplan.example:7480",
		"ws://tierplan.example",
		"tierplan.example:7480",
		"https://",
	} {
		_, err := config.IngressEndpoint(base)
		_, aerr := config.AgentEndpoint(base)
		if err == nil {
			t.Errorf("IngressEndpoint(%q) is accepted, want an error", base)
		}
		if (err == nil) != (aerr == nil) {
			t.Errorf("IngressEndpoint(%q) = %v but AgentEndpoint = %v: the two must share one rule", base, err, aerr)
		}
	}
}
