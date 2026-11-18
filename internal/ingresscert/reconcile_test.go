// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
)

// CRT-02, D-05, the tracer: a stack's hostname is released and claimed again, and the same certificate row and
// serial come back with no new order. The release only starts the clock of the grace; the claim stops it.
func TestTracerARedeployReusesTheCertificate(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	if !first.RequestedAt.IsZero() || !first.UnreferencedSince.IsZero() {
		t.Fatalf("a freshly issued row has requested_at %v unreferenced_since %v, want neither", first.RequestedAt, first.UnreferencedSince)
	}

	g.claim() // the stack's routes replaced with none: the hostname is released
	m.round(ctx)
	released := g.live("app.example.com")
	if released.UnreferencedSince.IsZero() || !released.UnreferencedSince.Equal(g.clock.Now()) {
		t.Errorf("unreferenced_since = %v after the release, want %v", released.UnreferencedSince, g.clock.Now())
	}
	if released.ID != first.ID || released.Serial != first.Serial || len(released.ChainPEM) == 0 || released.Phase != PhaseIdle {
		t.Errorf("released row = id %s serial %s phase %q, want the same certificate, idle", released.ID, released.Serial, released.Phase)
	}

	g.claim("app.example.com")
	m.round(ctx)
	again := g.live("app.example.com")
	if again.ID != first.ID || again.Serial != first.Serial {
		t.Errorf("re-claimed row = id %s serial %s, want id %s serial %s", again.ID, again.Serial, first.ID, first.Serial)
	}
	if !again.UnreferencedSince.IsZero() || again.Phase != PhaseIdle {
		t.Errorf("re-claimed row has unreferenced_since %v phase %q, want it cleared and idle", again.UnreferencedSince, again.Phase)
	}
	if n := g.fake.Count(acmetest.EndpointNewOrder); n != 1 {
		t.Errorf("the CA saw %d newOrder requests, want 1: a redeploy orders nothing", n)
	}
}
