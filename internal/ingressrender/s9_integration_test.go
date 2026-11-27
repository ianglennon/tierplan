// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
)

// Spike S9, short form (Phase 28, SELF-02, SELF-05, D-10, D-14, D-15, A-03): TierPlan's own route goes
// through a servers transport that verifies the backend (serverName and rootCAs naming TierPlan's
// self-signed certificate, which has no CA bit), over HTTP/2, and the event feed and the shell
// WebSocket must stay up through it. The backend here is the stub TierPlan of traefiktest, started with
// the real keepalive periods shortened. The 20-minute idle hold through the real virtual IP is 28-15.
const (
	// s9Front is the name TierPlan's route answers to.
	s9Front = "app.example.org"
	// s9SelfName is the name on the backend's certificate, which the transport's serverName must carry.
	s9SelfName = "tierplan.example.org"
	// s9RouterRule is the rule of the self route: the whole host except the two connect paths, which
	// the proxy must never carry (D-10).
	s9RouterRule = "Host(`" + s9Front + "`) && !PathPrefix(`/agent/connect`) && !PathPrefix(`/ingress/connect`)"
	// s9SelfCert is where the stub's certificate is written under certs/, and the path rootCAs names.
	s9SelfCert = traefiktest.NodeRoot + "/certs/self.crt"
)

// s9Doc is a dynamic file with one router, s9, on websecure with TLS, to the service s9-self, which is
// the stub over the transport t-self with the given serverName. rootCAs is omitted when withRoots is
// false.
func s9Doc(serverName string, withRoots bool) string {
	transport := "    t-self:\n      serverName: " + serverName + "\n"
	if withRoots {
		transport += "      rootCAs:\n        - " + s9SelfCert + "\n"
	}
	return s1ReloadOptions +
		"  certificates:\n    - certFile: " + traefiktest.NodeRoot + "/certs/front.crt\n      keyFile: " + traefiktest.NodeRoot + "/certs/front.key\n" +
		"http:\n  routers:\n" +
		s1Router("s9", s9RouterRule, "s9-self") +
		"  services:\n    s9-self:\n      loadBalancer:\n        serversTransport: t-self\n        servers:\n          - url: https://" +
		traefiktest.StubAlias + ":" + traefiktest.StubPort + "\n" +
		"  serversTransports:\n" + transport
}

// s9Site is one Traefik in front of a stub TierPlan.
type s9Site struct {
	*s1Site
	stub *traefiktest.Stub
	// selfCert is the stub's certificate, which is also its own root.
	selfCert []byte
}

// startS9 starts the stub with the given keepalives, a Traefik with the s9 route and the CA that
// signed the front certificate. serverName and withRoots shape the transport.
func startS9(t *testing.T, stub traefiktest.StubOptions, serverName string, withRoots bool) *s9Site {
	t.Helper()
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	frontChain, frontKey := ca.Leaf(t, s9Front)
	selfCert, selfKey := traefiktest.SelfSignedLeaf(t, s9SelfName)
	stub.CertPEM, stub.KeyPEM = selfCert, selfKey
	st := traefiktest.StartSelfStub(t, env, stub)
	env.Apply(t, append(s1CertFiles("front", frontChain, frontKey),
		traefiktest.FileSpec{Path: "certs/self.crt", Data: selfCert},
		s1Dynamic(s9Doc(serverName, withRoots))))
	site := &s1Site{env: env, ca: ca, tr: env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})}
	return &s9Site{s1Site: site, stub: st, selfCert: selfCert}
}

// TestS9VerifiedBackendOverTSelf is the tracer: a request for TierPlan's host reaches the stub through
// Traefik over a transport that verifies it, and the stub says the request arrived on HTTP/2, so h2 was
// negotiated to the backend.
func TestS9VerifiedBackendOverTSelf(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS9(t, traefiktest.StubOptions{}, s9SelfName, true)
	s.awaitStatus(t, s9Front, "/", http.StatusOK)

	status, body, _ := s.tr.Get(t, s.ca.Pool(), s9Front, "/proto", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /proto answered %d, want 200", status)
	}
	if got := strings.TrimSpace(body); got != "HTTP/2.0" {
		t.Errorf("the stub saw the protocol %q, want HTTP/2.0: HTTP/2 was not negotiated to the backend", got)
	}
}
