// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/x509"
	"slices"
	"sync"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender"
)

// The server's wiring of TierPlan's own route (D-04, D-07, D-12, D-14): the reconciler's self input from the holder and the
// trust set, the deployer's URLs, local connect and certificate state, and the install hook that wakes the reconciler.

const (
	wiringPublicURL = "https://tierplan.example.org:7480"
	wiringAgentURL  = "https://tierplan.internal:7480"
)

// notified records every environment the reconciler is woken for.
type notified struct {
	mu   sync.Mutex
	envs []string
}

func (n *notified) notify(envID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.envs = append(n.envs, envID)
}

func (n *notified) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.envs)
}

// wiringRig is a holder wired as the server wires it, over a trust set the test changes and settable URLs.
type wiringRig struct {
	h      *selfRoute
	src    *claimSource
	clk    *holderClock
	trust  *trustRig
	notes  *notified
	http01 bool
	urls   ingressdeploy.SelfURLs
}

func newWiringRig(t *testing.T, claimed, http01 bool) *wiringRig {
	t.Helper()
	h, src, clk := newHolderRig(t, claimed, nil)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := &wiringRig{
		h: h, src: src, clk: clk, notes: &notified{}, http01: http01,
		trust: newTrustRig(trustCert(t, "tierplan.example.org"), trustCert(t, "tierplan.example.org")),
		urls:  ingressdeploy.SelfURLs{PublicURL: wiringPublicURL, AgentURL: wiringAgentURL, ListenPort: 7480},
	}
	h.wire(selfInputs{
		trust:  r.trust.trust,
		urls:   func() ingressdeploy.SelfURLs { return r.urls },
		http01: func(context.Context) (bool, error) { return r.http01, nil },
		notify: r.notes.notify,
	})
	return r
}

func TestSelfInputForTheReconciler(t *testing.T) {
	r := newWiringRig(t, true, true)
	r.h.Observe(attachedObs(holderEnv))
	wantPEM, wantVer := r.trust.trust.PEM()

	got, ver, ok := r.h.SelfInput(context.Background(), holderEnv)
	if !ok {
		t.Fatal("SelfInput(env-1) is not ok, want TierPlan's own route")
	}
	want := ingressrender.SelfInput{
		Host: holderHost, BackendHost: "tierplan_tierplan", BackendPort: 7480, ServerName: "tierplan.example.org",
		TrustPEM: wantPEM, ACMEHosts: []string{"tierplan.example.org", "tierplan.internal"}, Attached: true,
	}
	if got.Host != want.Host || got.BackendHost != want.BackendHost || got.BackendPort != want.BackendPort ||
		got.ServerName != want.ServerName || got.Attached != want.Attached || !slices.Equal(got.ACMEHosts, want.ACMEHosts) ||
		string(got.TrustPEM) != string(want.TrustPEM) || got.BasePath != "" {
		t.Fatalf("SelfInput(env-1) = %+v, want %+v", got, want)
	}
	if ver != wantVer || ver == 0 {
		t.Fatalf("version = %d, want ownTrust's %d", ver, wantVer)
	}
	// The input renders: the self route is there, and the carve-out with it.
	if _, err := ingressrender.Render(ingressrender.Input{Self: &got, OwnVersion: ver}); err != nil {
		t.Fatalf("the self input does not render: %v", err)
	}

	if _, _, ok := r.h.SelfInput(context.Background(), "env-2"); ok {
		t.Fatal("SelfInput(env-2) is ok, want nothing for an environment TierPlan's claim does not name")
	}

	t.Run("the server name falls back to the agent URL's host", func(t *testing.T) {
		r.urls.PublicURL = ""
		got, _, ok := r.h.SelfInput(context.Background(), holderEnv)
		if !ok || got.ServerName != "tierplan.internal" || !slices.Equal(got.ACMEHosts, []string{"tierplan.internal"}) {
			t.Fatalf("SelfInput with only the agent URL = %+v ok %t, want server name tierplan.internal", got, ok)
		}
		r.urls.AgentURL = ""
		if _, _, ok := r.h.SelfInput(context.Background(), holderEnv); ok {
			t.Fatal("SelfInput with neither URL is ok, want nothing (Pitfall 10)")
		}
		r.urls = ingressdeploy.SelfURLs{PublicURL: wiringPublicURL, AgentURL: wiringAgentURL, ListenPort: 7480}
	})

	t.Run("a certificate change moves the version", func(t *testing.T) {
		_, before, _ := r.h.SelfInput(context.Background(), holderEnv)
		r.trust.chain = []*x509.Certificate{trustCert(t, "tierplan.example.org")}
		_, after, ok := r.h.SelfInput(context.Background(), holderEnv)
		if !ok || after == before {
			t.Fatalf("version %d after a certificate change, want other than %d", after, before)
		}
	})

	t.Run("not attached: the route is there, unattached, and the carve-out is not", func(t *testing.T) {
		o := attachedObs(holderEnv)
		o.Attached = false
		r.h.Observe(o)
		got, _, ok := r.h.SelfInput(context.Background(), holderEnv)
		if !ok || got.Attached || len(got.ACMEHosts) != 0 || got.Host != holderHost {
			t.Fatalf("SelfInput unattached = %+v ok %t, want the claim's host, not attached, no ACME hosts", got, ok)
		}
		// The change of attachment woke the environment's reconciler, since the version did not move.
		if n := r.notes.got(); !slices.Contains(n, holderEnv) {
			t.Fatalf("notified %v, want env-1 woken by the change", n)
		}
	})

	t.Run("carve-out alone: no claim, TierPlan runs here and renews over HTTP-01", func(t *testing.T) {
		r := newWiringRig(t, false, true)
		r.h.Observe(attachedObs(holderEnv))
		got, _, ok := r.h.SelfInput(context.Background(), holderEnv)
		if !ok || got.Host != "" || !got.Attached || len(got.ACMEHosts) != 2 {
			t.Fatalf("SelfInput carve-out = %+v ok %t, want no host and both ACME hosts", got, ok)
		}
		if _, err := ingressrender.Render(ingressrender.Input{Self: &got}); err != nil {
			t.Fatalf("the carve-out input does not render: %v", err)
		}
	})
}

func TestNoSelfInputWithoutAClaimOrCarveOut(t *testing.T) {
	r := newWiringRig(t, false, false)
	r.h.Observe(attachedObs(holderEnv))
	for _, env := range []string{holderEnv, "env-2", ""} {
		if got, ver, ok := r.h.SelfInput(context.Background(), env); ok || ver != 0 {
			t.Fatalf("SelfInput(%q) = %+v, %d, %t; want nothing with no claim and no carve-out", env, got, ver, ok)
		}
	}
	// TierPlan running nowhere this server can see: nothing either, even with HTTP-01 in use.
	r2 := newWiringRig(t, false, true)
	if _, _, ok := r2.h.SelfInput(context.Background(), holderEnv); ok {
		t.Fatal("SelfInput with no claim and no observation is ok, want nothing")
	}
	// An unwired holder answers nothing.
	h, _, _ := newHolderRig(t, true, nil)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := h.SelfInput(context.Background(), holderEnv); ok {
		t.Fatal("an unwired holder's SelfInput is ok, want nothing")
	}
}
