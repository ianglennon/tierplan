// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Spike S1, second half, backend and header cases (D-13, D-14, D-08, RTE-03): how the pinned Traefik
// reaches an HTTPS backend, what it does with forwarded headers from a peer it does not trust, whether
// it executes dynamic files as templates, and how it matches paths. The fixtures are built from
// string constants, so the Go identifier of the verification-off setting is never written; the YAML
// key that spells it with a lower-case first letter is not that identifier.

const (
	// s1BackendName is the name the HTTPS backend's certificate carries. The hostname the proxy
	// connects to (wt) is not on it, so a client that checks names must be told this one.
	s1BackendName = "backend.internal"
	// s1CACert is where the test CA is written under certs/, and the absolute path rootCAs names.
	s1CACert = traefiktest.NodeRoot + "/certs/ca.crt"
)

// s1BackendDoc is a dynamic file with four routers to the one HTTPS backend wt:443, each with its own
// service, and the three transports the cases need. The routers are told apart by path.
func s1BackendDoc(front string) string {
	svc := func(name, transport string) string {
		out := "    " + name + ":\n      loadBalancer:\n"
		if transport != "" {
			out += "        serversTransport: " + transport + "\n"
		}
		return out + "        servers:\n          - url: https://wt:443\n"
	}
	return s1ReloadOptions +
		"  certificates:\n    - certFile: " + traefiktest.NodeRoot + "/certs/front.crt\n      keyFile: " + traefiktest.NodeRoot + "/certs/front.key\n" +
		"http:\n  routers:\n" +
		s1Router("r-own", s1Rule(front, "/own"), "s-own") +
		s1Router("r-none", s1Rule(front, "/none"), "s-none") +
		s1Router("r-user", s1Rule(front, "/user"), "s-user") +
		s1Router("r-noname", s1Rule(front, "/noname"), "s-noname") +
		"  services:\n" +
		svc("s-own", "t-own") + svc("s-none", "") + svc("s-user", "t-user-https") + svc("s-noname", "t-noname") +
		"  serversTransports:\n" +
		"    t-own:\n      rootCAs:\n        - " + s1CACert + "\n      serverName: " + s1BackendName + "\n" +
		"    t-noname:\n      rootCAs:\n        - " + s1CACert + "\n" +
		"    t-user-https:\n      insecureSkipVerify: true\n      disableHTTP2: true\n"
}

// TestS1BackendTLSRootCAsAndServerName proves what Phase 28 needs for TierPlan's own route (D-13,
// correction 15, O9) and the user-route transport D-13 uses. Against a self-signed HTTPS whoami whose
// certificate is for backend.internal, reached as wt:443:
//   - a transport with rootCAs (the test CA) and serverName backend.internal verifies and answers 200;
//   - no transport fails the verification: 500 and an x509 line in Traefik's log (a 500, not a 502);
//   - a transport with rootCAs but no serverName fails the same way: the CA is trusted but the name
//     wt is not on the certificate, so serverName is not optional;
//   - a transport that turns verification off and disables HTTP/2 answers 200: the user-route case.
func TestS1BackendTLSRootCAsAndServerName(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	const front = "be.test"
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	frontChain, frontKey := ca.Leaf(t, front)
	beChain, beKey := ca.Leaf(t, s1BackendName)
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "wt", Hostname: "wt-tls", CertPEM: beChain, KeyPEM: beKey})
	env.Apply(t, append(s1CertFiles("front", frontChain, frontKey),
		traefiktest.FileSpec{Path: "certs/ca.crt", Data: ca.PEM()},
		s1Dynamic(s1BackendDoc(front))))
	s := &s1Site{env: env, ca: ca, tr: env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})}

	// (a) rootCAs plus serverName: the verified backend.
	body := s.awaitGet(t, front, "/own")
	if !strings.Contains(body, "Hostname: wt-tls") {
		t.Errorf("/own was not answered by the HTTPS backend:\n%s", body)
	}

	// (b) no transport: verification fails and Traefik answers 500, not 502.
	s.awaitStatus(t, front, "/none", http.StatusInternalServerError)
	testsupport.Poll(t, testsupport.Deadline, "Traefik's log to hold an x509 line", func() (bool, string) { // testsupport: external, a real container's log
		return strings.Contains(s.tr.Logs(t), "x509"), "no x509 line yet"
	})

	// (c) a transport with verification off and HTTP/2 off: the user-route case.
	s.awaitStatus(t, front, "/user", http.StatusOK)

	// (d) rootCAs without serverName: the CA is trusted, the name wt is not on the certificate.
	status, err := s.statusOf(front, "/noname")
	if err != nil {
		t.Fatalf("GET /noname: %v", err)
	}
	t.Logf("rootCAs without serverName answers %d (rootCAs plus serverName: 200, no transport: 500)", status)
	if status == http.StatusOK {
		t.Errorf("rootCAs without serverName answered 200: the name check did not happen, so serverName would not be needed")
	}
}

// echoHeaders reads whoami's echo: every line of the form "Name: value" after the first, by lower-cased
// name. The request line and the Hostname, IP and RemoteAddr lines are there too and are harmless.
func echoHeaders(body string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(body, "\n") {
		name, value, ok := strings.Cut(strings.TrimRight(line, "\r"), ": ")
		if !ok || strings.ContainsAny(name, " \t") {
			continue
		}
		out[strings.ToLower(name)] = append(out[strings.ToLower(name)], value)
	}
	return out
}

// TestS1ForwardedHeadersFromAnUntrustedPeer proves D-14 (T-21-10, O10 and O17): with no
// forwardedHeaders configured Traefik trusts no peer, so a client's own forwarding headers are
// overwritten with the values Traefik measured and never reach the backend, and a header spelled with
// underscores is deleted by aliasHeadersStrategy.
func TestS1ForwardedHeadersFromAnUntrustedPeer(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS1(t, fullBundle(), true)
	s.awaitGet(t, s1Host, "/shop")

	const spoof = "203.0.113.9"
	header := http.Header{}
	for k, v := range map[string]string{
		"X-Forwarded-For":    spoof,
		"X-Forwarded-Proto":  "http",
		"X-Forwarded-Host":   "evil.test",
		"X-Real-Ip":          spoof,
		"X-Forwarded-Prefix": "/evil",
		"X_Auth_User":        "admin",
	} {
		header.Set(k, v)
	}
	status, body, _, clientIP, err := s.tr.TryGetFromIP(s.ca.Pool(), s1Host, "/shop", header)
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET /shop answered %d (error %v), want 200", status, err)
	}
	if clientIP == "" {
		t.Fatal("the probe recorded no client address")
	}
	got := echoHeaders(body)
	t.Logf("the client connected from %s; the backend saw X-Forwarded-For %v, X-Forwarded-Proto %v, X-Forwarded-Host %v, X-Real-Ip %v",
		clientIP, got["x-forwarded-for"], got["x-forwarded-proto"], got["x-forwarded-host"], got["x-real-ip"])

	want := map[string]string{
		"x-forwarded-for":   clientIP,
		"x-forwarded-proto": "https",
		"x-forwarded-host":  s1Host,
	}
	for name, v := range want {
		if len(got[name]) != 1 || got[name][0] != v {
			t.Errorf("%s reached the backend as %q, want exactly %q", name, got[name], v)
		}
	}
	if strings.Contains(strings.Join(got["x-real-ip"], ","), spoof) {
		t.Errorf("x-real-ip reached the backend as %q: the spoofed value survived", got["x-real-ip"])
	}
	for _, v := range got["x-forwarded-prefix"] {
		if v == "/evil" {
			t.Errorf("x-forwarded-prefix /evil reached the backend")
		}
	}
	for name := range got {
		if strings.ReplaceAll(name, "_", "-") == "x-auth-user" {
			t.Errorf("the underscore header %q reached the backend (aliasHeadersStrategy delete did not drop it): %q", name, got[name])
		}
	}
	if strings.Contains(body, spoof) {
		t.Errorf("the spoofed address %s appears in the backend's echo:\n%s", spoof, body)
	}
}
