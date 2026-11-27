// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingresscert"
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

// fakeInstance is the instance's URLs and their locks.
type fakeInstance struct {
	public, agent             string
	publicLocked, agentLocked bool
}

func (f fakeInstance) PublicURL() string    { return f.public }
func (f fakeInstance) AgentURL() string     { return f.agent }
func (f fakeInstance) Locked() bool         { return f.publicLocked }
func (f fakeInstance) AgentURLLocked() bool { return f.agentLocked }

// fakeCerts is the ingress certificate store's state and its live certificates by hostname.
type fakeCerts struct {
	state ingresscert.State
	live  map[string]ingresscert.Cert
}

func (f fakeCerts) LiveCert(_ context.Context, host string) (ingresscert.Cert, bool, error) {
	c, ok := f.live[host]
	return c, ok, nil
}

func (f fakeCerts) State(context.Context) (ingresscert.State, error) { return f.state, nil }

func TestDeployerSelfDepsAreWired(t *testing.T) {
	inst := fakeInstance{public: wiringPublicURL, agent: wiringAgentURL, publicLocked: true}
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	accepted := ingresscert.State{TermsAcceptedAt: now.Add(-time.Hour), TermsAcceptedBy: "id-admin"}
	certs := fakeCerts{state: accepted, live: map[string]ingresscert.Cert{
		"valid.example.org": {
			EnvID: holderEnv, Hostname: "valid.example.org", Source: ingresscert.SourceACME, Issuer: ingresscert.IssuerProduction,
			ChainPEM: []byte("chain"), NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(60 * 24 * time.Hour), Ready: true,
		},
		"pending.example.org": {EnvID: holderEnv, Hostname: "pending.example.org", Source: ingresscert.SourceACME, Issuer: ingresscert.IssuerProduction},
		"other.example.org": {
			EnvID: "env-2", Hostname: "other.example.org", Source: ingresscert.SourceACME, Issuer: ingresscert.IssuerProduction,
			ChainPEM: []byte("chain"), NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(60 * 24 * time.Hour), Ready: true,
		},
	}}
	deps := ingressDeployDeps{
		SelfURLs:   instanceSelfURLs(inst, ":7480"),
		SocketPath: "/var/run/docker.sock", SelfID: "c0ffee",
		IngressCerts: certs, Clock: fixedClock{now},
	}
	got := ingressDeployOf(deps)

	if got.SelfURLs == nil {
		t.Fatal("the deployer has no SelfURLs")
	}
	wantURLs := ingressdeploy.SelfURLs{PublicURL: wiringPublicURL, AgentURL: wiringAgentURL, PublicLocked: true, AgentLocked: false, ListenPort: 7480}
	if u := got.SelfURLs(); u != wantURLs {
		t.Fatalf("SelfURLs() = %+v, want %+v", u, wantURLs)
	}
	if got.SelfConnect == nil || got.SelfDisconnect == nil {
		t.Fatal("SelfConnect or SelfDisconnect is nil with the socket and TierPlan's container known")
	}
	for _, d := range []ingressDeployDeps{{SocketPath: "/var/run/docker.sock"}, {SelfID: "c0ffee"}, {}} {
		if g := ingressDeployOf(d); g.SelfConnect != nil || g.SelfDisconnect != nil {
			t.Fatalf("SelfConnect/SelfDisconnect set with socket %q and container %q, want nil", d.SocketPath, d.SelfID)
		}
	}
	if g := ingressDeployOf(ingressDeployDeps{}); g.HostCertificate != nil {
		t.Fatal("HostCertificate set with no certificate store, want nil (unknown)")
	}

	ctx := context.Background()
	read := func(t *testing.T, get func(ctx context.Context, envID, host string) (ingressdeploy.HostCertState, error), env, host string) ingressdeploy.HostCertState {
		t.Helper()
		st, err := get(ctx, env, host)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := read(t, got.HostCertificate, holderEnv, "valid.example.org"); !st.Valid || !st.Known || st.TermsNeeded || st.Paused || st.Reason != "" {
		t.Fatalf("valid = %+v, want valid and known", st)
	}
	if st := read(t, got.HostCertificate, holderEnv, "pending.example.org"); st.Valid || !st.Known || st.Reason != ingresscert.StateQueued {
		t.Fatalf("pending = %+v, want known, not valid, reason queued", st)
	}
	if st := read(t, got.HostCertificate, holderEnv, "none.example.org"); st.Valid || st.Known {
		t.Fatalf("no certificate = %+v, want unknown", st)
	}
	if st := read(t, got.HostCertificate, holderEnv, "other.example.org"); st.Valid || st.Known {
		t.Fatalf("another environment's certificate = %+v, want unknown here", st)
	}

	terms := certs
	terms.state = ingresscert.State{}
	deps.IngressCerts = terms
	if st := read(t, ingressDeployOf(deps).HostCertificate, holderEnv, "pending.example.org"); !st.TermsNeeded || st.Valid {
		t.Fatalf("terms not accepted = %+v, want terms needed", st)
	}
	paused := certs
	paused.state = accepted
	paused.state.PausedReason = "restore"
	deps.IngressCerts = paused
	if st := read(t, ingressDeployOf(deps).HostCertificate, holderEnv, "pending.example.org"); !st.Paused || st.TermsNeeded || st.Valid {
		t.Fatalf("paused = %+v, want paused", st)
	}
}

// fixedClock is a clock that reads one time and never fires.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time                       { return c.t }
func (c fixedClock) After(time.Duration) <-chan time.Time { return nil }

// installRig is a holder wired to a real HTTPS runtime and its trust set, with the install hook registered as the server
// registers it.
func installRig(t *testing.T, claimed, http01 bool) (*selfRoute, *httpsRuntime, *ownTrust, *notified) {
	t.Helper()
	h, _, _ := newHolderRig(t, claimed, nil)
	if err := h.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	selfPair, selfLeaf := genCert(t, "tierplan.example.org")
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSelfSigned, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &selfPair, nil })
	trust := newOwnTrust(rt.servedChain, func() *x509.Certificate { return selfLeaf }, func() time.Time { return fixedNow })
	trust.PEM()
	notes := &notified{}
	h.wire(selfInputs{
		trust: trust,
		urls: func() ingressdeploy.SelfURLs {
			return ingressdeploy.SelfURLs{PublicURL: wiringPublicURL, ListenPort: 7480}
		},
		http01: func(context.Context) (bool, error) { return http01, nil },
		notify: notes.notify,
	})
	registerSelfInstallHook(rt, h)
	return h, rt, trust, notes
}

func TestInstallWakesTheReconciler(t *testing.T) {
	h, rt, trust, notes := installRig(t, true, false)
	h.Observe(attachedObs(holderEnv))
	_, before := trust.PEM()

	lePair, leLeaf := genCert(t, "le.tierplan.example.org")
	rt.Install(tlsKindACME, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &lePair, nil })

	if got := notes.got(); !slices.Equal(got, []string{holderEnv}) {
		t.Fatalf("notified %v after an install, want env-1 once", got)
	}
	pemBytes, after := trust.PEM()
	if after == before {
		t.Fatal("the trust version did not move with the install")
	}
	if !trust.has(t, leLeaf) {
		t.Fatalf("the trust set does not hold the installed certificate: %d bytes", len(pemBytes))
	}
	// The reconciler's self input carries the new version.
	if _, ver, ok := h.SelfInput(context.Background(), holderEnv); !ok || ver != after {
		t.Fatalf("SelfInput version %d ok %t, want %d", ver, ok, after)
	}

	t.Run("the carve-out alone wakes TierPlan's environment once HTTP-01 is known", func(t *testing.T) {
		h, rt, _, notes := installRig(t, false, true)
		h.Observe(attachedObs(holderEnv))
		// SelfInput's answer is what the hook reads; it reads no database itself.
		if _, _, ok := h.SelfInput(context.Background(), holderEnv); !ok {
			t.Fatal("no carve-out input")
		}
		pair, _ := genCert(t, "again.tierplan.example.org")
		rt.Install(tlsKindACME, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil })
		if got := notes.got(); !slices.Equal(got, []string{holderEnv}) {
			t.Fatalf("notified %v, want env-1 once", got)
		}
	})

	t.Run("no claim and no carve-out: nothing is woken", func(t *testing.T) {
		h, rt, trust, notes := installRig(t, false, false)
		h.Observe(attachedObs(holderEnv))
		_, _, _ = h.SelfInput(context.Background(), holderEnv)
		_, v0 := trust.PEM()
		pair, _ := genCert(t, "none.tierplan.example.org")
		rt.Install(tlsKindACME, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil })
		if _, v1 := trust.PEM(); v1 == v0 {
			t.Fatal("the install did not reach the trust set")
		}
		if got := notes.got(); len(got) != 0 {
			t.Fatalf("notified %v, want nothing with no claim and no carve-out need", got)
		}
	})
}
