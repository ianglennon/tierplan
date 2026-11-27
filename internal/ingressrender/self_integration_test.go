// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// TestRenderedCarveOutReachesThePlainListener proves D-12 and SELF-04 through the rendered bundle: with
// selfServerName as an ACME host, the challenge path on that name reaches the stub's plain listener on
// port 80 (it answers "<token>.stub"), ahead of the redirect to https, while the same path on
// TierPlan's routed host and another path on its own name are still the redirect's 301. The carve-out
// opens nothing on port 80 but TierPlan's own challenge.
func TestRenderedCarveOutReachesThePlainListener(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	cert, key := traefiktest.SelfSignedLeaf(t, selfServerName)
	s := startSelfSite(t, cert, key, cert, func(in *Input) { in.Self.ACMEHosts = []string{selfServerName} })
	// The route answering 200 is the sign the whole file has been read.
	s.awaitGet(t, selfFront, "/")
	plain := &s12Site{tr: s.tr}
	web := func(host, target string) (status int, location, body string) {
		t.Helper()
		status, location, body, err := plain.get(s12WebPort, host, target)
		if err != nil {
			t.Fatalf("GET http://%s%s on the web entry point: %v", host, target, err)
		}
		t.Logf("http://%s%s answered %d (Location %q, body %q)", host, target, status, location, body)
		return status, location, body
	}

	status, _, body := web(selfServerName, s12ChallengePath)
	if status != http.StatusOK || body != "tok.stub" {
		t.Errorf("http://%s%s answered %d %q, want 200 \"tok.stub\" from the stub's plain listener", selfServerName, s12ChallengePath, status, body)
	}

	status, location, _ := web(selfFront, s12ChallengePath)
	if status != http.StatusMovedPermanently || location != "https://"+selfFront+s12ChallengePath {
		t.Errorf("http://%s%s answered %d with Location %q, want 301 to https: the carve-out is for the ACME hosts only", selfFront, s12ChallengePath, status, location)
	}

	status, location, _ = web(selfServerName, "/")
	if status != http.StatusMovedPermanently || location != "https://"+selfServerName+"/" {
		t.Errorf("http://%s/ answered %d with Location %q, want 301 to https: the carve-out is for the challenge path only", selfServerName, status, location)
	}
}

// swapPace is the gap between two requests of the swap tests: a client that keeps asking while
// TierPlan's certificate changes under the proxy.
const swapPace = 100 * time.Millisecond

// selfAnswer is one paced request: its status, or its transport error, and whether it was sent after
// the stub reported the new certificate.
type selfAnswer struct {
	status    int
	err       error
	afterSwap bool
}

// pacer sends GET / for TierPlan's host every swapPace until halted, and keeps every answer.
type pacer struct {
	mu      sync.Mutex
	answers []selfAnswer
	swapped atomic.Bool
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

// startPacer starts the requests. They run until halt, which the test's clean-up also calls.
func (s *selfSite) startPacer(t *testing.T) *pacer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &pacer{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		tick := time.NewTicker(swapPace)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			after := p.swapped.Load()
			status, err := s.statusOf(selfFront, "/")
			p.mu.Lock()
			p.answers = append(p.answers, selfAnswer{status: status, err: err, afterSwap: after})
			p.mu.Unlock()
		}
	}()
	t.Cleanup(p.halt)
	return p
}

// halt stops the requests and waits for the one in flight.
func (p *pacer) halt() {
	p.once.Do(func() {
		p.cancel()
		<-p.done
	})
}

// snapshot is a copy of the answers so far.
func (p *pacer) snapshot() []selfAnswer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]selfAnswer(nil), p.answers...)
}

// awaitAnswers polls until the pacer holds at least n answers.
func (p *pacer) awaitAnswers(t *testing.T, n int) {
	t.Helper()
	testsupport.Poll(t, testsupport.Deadline, strconv.Itoa(n)+" paced answers", func() (bool, string) { // testsupport: external, each answer is a real request through Traefik
		got := len(p.snapshot())
		return got >= n, strconv.Itoa(got) + " so far"
	})
}

// String is the answer for a log line.
func (a selfAnswer) String() string {
	if a.err != nil {
		return "error " + a.err.Error()
	}
	return strconv.Itoa(a.status)
}

// TestTrustBundleFollowsACertificateSwap proves A-02 and A-03 on the rendered route: TierPlan's
// certificate changes (a re-sign keeps the key and changes the names, and the old leaf does not verify
// the new one, F2b) while a client keeps asking for TierPlan's host. The stub reloads its pair on
// SIGHUP and drains its old connections, so the proxy must open new ones and verify the new leaf. With
// a trust file holding both leaves, the staged install's bundle, every answer before and after the swap
// is 200. The control holds the old leaf only: once the old connections are gone, the answers are
// server errors and Traefik logs x509, which shows the swap did make the proxy verify again.
func TestTrustBundleFollowsACertificateSwap(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const before, after = 10, 20
	oldCert, key := traefiktest.SelfSignedLeaf(t, selfServerName)
	newCert := traefiktest.ResignedLeaf(t, key, selfServerName, "tierplan.internal")

	t.Run("both leaves", func(t *testing.T) {
		trust := bytes.Join([][]byte{oldCert, newCert}, nil)
		s := startSelfSite(t, oldCert, key, trust, nil)
		s.awaitGet(t, selfFront, "/")
		p := s.startPacer(t)
		p.awaitAnswers(t, before)
		s.stub.SwapCert(t, newCert, key)
		p.swapped.Store(true)
		p.awaitAnswers(t, len(p.snapshot())+after)
		p.halt()

		answers := p.snapshot()
		n := 0
		for i, a := range answers {
			if a.afterSwap {
				n++
			}
			if a.err != nil || a.status != http.StatusOK {
				t.Errorf("answer %d (after the swap: %v) was %s, want 200: the route broke while the certificate changed", i, a.afterSwap, a)
			}
		}
		t.Logf("%d answers, %d of them after the stub served the new leaf, all checked for 200", len(answers), n)
		if n < after {
			t.Fatalf("only %d answers after the swap, want at least %d", n, after)
		}
	})

	t.Run("old leaf only", func(t *testing.T) {
		s := startSelfSite(t, oldCert, key, oldCert, nil)
		s.awaitGet(t, selfFront, "/")
		p := s.startPacer(t)
		p.awaitAnswers(t, before)
		s.stub.SwapCert(t, newCert, key)
		p.swapped.Store(true)
		status := s.awaitFailure(t, "/")
		t.Logf("with the old leaf only, GET / answered %d after the swap and Traefik logged x509", status)
		mark := len(p.snapshot())
		p.awaitAnswers(t, mark+after/4)
		p.halt()

		answers := p.snapshot()
		for i, a := range answers {
			switch {
			case !a.afterSwap && (a.err != nil || a.status != http.StatusOK):
				t.Errorf("answer %d, before the swap, was %s, want 200", i, a)
			case i >= mark && (a.err != nil || a.status < http.StatusInternalServerError):
				t.Errorf("answer %d, after the proxy refused the new leaf, was %s, want a server error: an unverified backend was served", i, a)
			}
		}
	})
}
