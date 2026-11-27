// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TierPlan's own route as the renderer writes it, on the pinned Traefik (Phase 28, D-23, SELF-02,
// SELF-04). S9 and S12 proved the shapes with hand-written files; these apply what Render makes for a
// system claim and a SelfInput naming the stub TierPlan of traefiktest, so a change to the renderer's
// self router, its t-self transport, its trust file or its HTTP-01 carve-out that Traefik would not
// accept fails here. Every negative control is in this file beside the case it guards (T-28-56): a
// verification that was silently off would pass the positive cases, and fails the controls.
const (
	// selfFront is the system claim's hostname, the name TierPlan's route answers to.
	selfFront = "tp.apps.example.org"
	// selfServerName is the name on TierPlan's own certificate, the host of its public URL, which the
	// transport verifies the backend against (D-14).
	selfServerName = "tierplan.example.org"
)

// selfStubPort is the stub's listen port as a number, the SelfInput's BackendPort.
func selfStubPort(t *testing.T) int {
	t.Helper()
	p, err := strconv.Atoi(traefiktest.StubPort)
	if err != nil {
		t.Fatalf("the stub's port %q is not a number: %v", traefiktest.StubPort, err)
	}
	return p
}

// selfRenderInput is the input the reconciler builds for the environment that carries TierPlan: the
// system claim selfFront with its one route row and a front certificate from ca, and a SelfInput that
// reaches the stub by its alias on the test network, verifies it as selfServerName against trust, and
// says TierPlan is attached.
func selfRenderInput(t *testing.T, ca *traefiktest.CA, trust []byte) Input {
	t.Helper()
	return Input{
		Snapshot: ingress.Snapshot{
			Settings: ingress.Settings{EnvID: "env-1", Exists: true, Enabled: true, ConfigVersion: 1},
			Claims:   []ingress.SnapshotClaim{systemClaim(selfFront)},
			Routes:   []ingress.SnapshotRoute{systemRoute(selfFront)},
		},
		Certs:        servable(t, ca, selfFront),
		CertsVersion: 1,
		OwnVersion:   1,
		Self: &SelfInput{
			Host:        selfFront,
			BackendHost: traefiktest.StubAlias,
			BackendPort: selfStubPort(t),
			ServerName:  selfServerName,
			TrustPEM:    trust,
			Attached:    true,
		},
	}
}

// selfSite is the pinned Traefik over a rendered bundle, in front of a stub TierPlan.
type selfSite struct {
	renderedSite
	stub *traefiktest.Stub
}

// startSelfSite starts the stub with certPEM and keyPEM, renders in (with edit applied to it first,
// when given), applies the bundle and starts Traefik over it. The route must not be skipped: a skip
// would leave nothing to test.
func startSelfSite(t *testing.T, certPEM, keyPEM []byte, trust []byte, edit func(*Input)) *selfSite {
	t.Helper()
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	stub := traefiktest.StartSelfStub(t, env, traefiktest.StubOptions{CertPEM: certPEM, KeyPEM: keyPEM})
	in := selfRenderInput(t, ca, trust)
	if edit != nil {
		edit(&in)
	}
	res, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("the self route was skipped: %+v", res.Skipped)
	}
	return &selfSite{renderedSite: startRendered(t, env, ca, res.Bundle), stub: stub}
}

// awaitFailure polls until a request for TierPlan's host answers a server error, which is how Traefik
// answers when it cannot verify the backend (F2a: a 500; never pinned to one code, Pitfall 1), and then
// until Traefik's log holds the x509 line that says why.
func (s *selfSite) awaitFailure(t *testing.T, pathAndQuery string) int {
	t.Helper()
	var status int
	testsupport.Poll(t, testsupport.Deadline, "GET "+selfFront+pathAndQuery+" to answer a server error", func() (bool, string) { // testsupport: external, Traefik reloads on its own schedule
		st, err := s.statusOf(selfFront, pathAndQuery)
		if err != nil {
			return false, err.Error()
		}
		status = st
		return st >= http.StatusInternalServerError, "status " + strconv.Itoa(st)
	})
	testsupport.Poll(t, testsupport.Deadline, "Traefik's log to hold an x509 line", func() (bool, string) { // testsupport: external, a real container's log
		return strings.Contains(s.tr.Logs(t), "x509"), "no x509 line yet"
	})
	return status
}

// TestRenderedSelfRouteVerifiesTheBackend is the tracer (D-23): the bundle Render makes for the system
// claim, applied to the pinned Traefik with the shipped static file, serves GET / for TierPlan's host
// from the stub over t-self, with the backend verified against the stub's own self-signed leaf as the
// trust file. The stub answers / with the protocol the request arrived on, so the body also shows the
// route reached it over HTTP/2.
func TestRenderedSelfRouteVerifiesTheBackend(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	cert, key := traefiktest.SelfSignedLeaf(t, selfServerName)
	s := startSelfSite(t, cert, key, cert, nil)

	body := s.awaitGet(t, selfFront, "/")
	t.Logf("GET https://%s/ answered 200 from the stub: %q", selfFront, strings.TrimSpace(body))
	if got := strings.TrimSpace(body); got != "HTTP/2.0" {
		t.Errorf("the stub saw the protocol %q, want HTTP/2.0 (the answer of the stub's / over t-self)", got)
	}
}

// TestRenderedSelfRouteRefusesAnUntrustedBackend is the control of the tracer (T-28-56) and the
// connect-path exclusion (D-10). With a trust file holding another self-signed leaf for the same name,
// the rendered route answers a server error and Traefik logs x509: the transport verifies, so a
// passing tracer is not one with verification off. Through the good render, /agent/connect and
// /ingress/connect are Traefik's 404, though the stub answers both: the route never carries them.
func TestRenderedSelfRouteRefusesAnUntrustedBackend(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	cert, key := traefiktest.SelfSignedLeaf(t, selfServerName)

	t.Run("untrusted leaf", func(t *testing.T) {
		other, _ := traefiktest.SelfSignedLeaf(t, selfServerName)
		s := startSelfSite(t, cert, key, other, nil)
		status := s.awaitFailure(t, "/")
		t.Logf("with a trust file that does not hold the stub's leaf, GET / answered %d and Traefik logged x509", status)
		if status, _ := s.stub.Get(t, cert, "/"); status != http.StatusOK {
			t.Fatalf("the stub itself answered %d, want 200: the failure is not the proxy's verification", status)
		}
	})

	t.Run("connect paths", func(t *testing.T) {
		s := startSelfSite(t, cert, key, cert, nil)
		s.awaitGet(t, selfFront, "/")
		for _, p := range []string{"/agent/connect", "/ingress/connect"} {
			if status, body := s.stub.Get(t, cert, p); status != http.StatusOK || body != "reached" {
				t.Fatalf("the stub answered %s with %d %q, want 200 \"reached\": a 404 through the proxy would prove nothing", p, status, body)
			}
		}
		for _, p := range []string{"/agent/connect", "/ingress/connect", "/agent/connect/x", "/ingress/connect?x=1"} {
			status, err := s.statusOf(selfFront, p)
			if err != nil {
				t.Fatalf("GET %s%s: %v", selfFront, p, err)
			}
			t.Logf("GET %s%s answered %d", selfFront, p, status)
			if status != http.StatusNotFound {
				t.Errorf("GET %s%s answered %d, want 404: the self route carried a connect path", selfFront, p, status)
			}
		}
	})
}
