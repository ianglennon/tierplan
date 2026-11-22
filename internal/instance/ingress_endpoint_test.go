// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package instance_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
)

// TestInstanceIngressEndpointFollowsTheAgentBase: the proxy dials the agent URL when one is set and the public URL
// otherwise, and nothing when neither is. The method takes no request, so a forged Host cannot reach it (T-24-23).
func TestInstanceIngressEndpointFollowsTheAgentBase(t *testing.T) {
	i, st := load(t, config.Config{})
	if got := i.IngressEndpoint(); got != "" {
		t.Errorf("IngressEndpoint with no URL = %q, want empty", got)
	}
	i.Apply("x", "https://example.com/tierplan")
	if got, want := i.IngressEndpoint(), "wss://example.com/tierplan/ingress/connect"; got != want {
		t.Errorf("IngressEndpoint = %q, want %q", got, want)
	}
	if err := setAgentURL(t, i, st, "https://tierplan.lan:7480"); err != nil {
		t.Fatal(err)
	}
	i.ApplyAgentURL("https://tierplan.lan:7480")
	if got, want := i.IngressEndpoint(), "wss://tierplan.lan:7480/ingress/connect"; got != want {
		t.Errorf("IngressEndpoint with an agent URL = %q, want %q", got, want)
	}
}
