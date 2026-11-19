// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Spike S1, first half (D-01): the strict-SNI, certificate-path, redirect and no-call-home behaviour of
// the pinned Traefik, on hand-written fixture bundles. Phase 23 reads the layout these tests fix, and
// 21-09 repeats the cases on rendered bundles. Every test runs a real Traefik on an --internal network
// with the same static file the proxy image will ship, and polls its asynchronous file reload with a
// bounded deadline, never a sleep.

const (
	// s1Host is the hostname the fixture serves.
	s1Host = "a.test.local"

	s1CertPath = traefiktest.NodeRoot + "/certs/a.crt"
	s1KeyPath  = traefiktest.NodeRoot + "/certs/a.key"
)

// s1Bundle describes a fixture dynamic configuration. The YAML is built from constants and the paths
// below; nothing user-supplied reaches it.
type s1Bundle struct {
	// Options adds tls.options.default with sniStrict and a TLS 1.2 floor.
	Options bool
	// CertFile and KeyFile are the paths as written in the YAML; empty writes no certificate entry.
	CertFile, KeyFile string
	// Route adds a router for s1Host on websecure to the backend shop_web:80.
	Route bool
	// Chain and Key, when set, are written to certs/a.crt and certs/a.key.
	Chain, Key []byte
}

// files is the bundle as the supervisor applies it: certificates first, the one YAML file last.
func (b s1Bundle) files() []traefiktest.FileSpec {
	var out []traefiktest.FileSpec
	if b.Chain != nil {
		out = append(out, traefiktest.FileSpec{Path: "certs/a.crt", Data: b.Chain})
	}
	if b.Key != nil {
		out = append(out, traefiktest.FileSpec{Path: "certs/a.key", Data: b.Key, Mode: 0o600})
	}
	return append(out, traefiktest.FileSpec{Path: "dynamic/tierplan.yml", Data: []byte(b.yaml())})
}

func (b s1Bundle) yaml() string {
	var sb strings.Builder
	if b.Options || b.CertFile != "" {
		sb.WriteString("tls:\n")
	}
	if b.Options {
		sb.WriteString("  options:\n    default:\n      sniStrict: true\n      minVersion: VersionTLS12\n")
	}
	if b.CertFile != "" {
		sb.WriteString("  certificates:\n    - certFile: " + b.CertFile + "\n      keyFile: " + b.KeyFile + "\n")
	}
	if b.Route {
		sb.WriteString("http:\n  routers:\n    r-a:\n      rule: Host(`" + s1Host + "`)\n      entryPoints:\n        - websecure\n" +
			"      service: s-a\n      tls: {}\n  services:\n    s-a:\n      loadBalancer:\n        servers:\n          - url: http://shop_web:80\n")
	}
	return sb.String()
}

// s1Site is one Traefik with the fixture bundle and the CA that signed its leaf.
type s1Site struct {
	env *traefiktest.Env
	ca  *traefiktest.CA
	tr  *traefiktest.Traefik
}

// startS1 starts a whoami backend and a Traefik on a new internal network with the bundle already in
// place, as a node supervisor writes it before it starts the proxy. With withLeaf the bundle's
// certificate files hold a leaf for s1Host from a new CA.
func startS1(t *testing.T, b s1Bundle, withLeaf bool) *s1Site {
	t.Helper()
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	if withLeaf {
		b.Chain, b.Key = ca.Leaf(t, s1Host)
	}
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_web", Hostname: "shop-web"})
	env.Apply(t, b.files())
	return &s1Site{env: env, ca: ca, tr: env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})}
}

// awaitHandshake polls until a handshake for name succeeds and returns its state. Traefik reloads its
// file provider asynchronously, so a handshake fails until the certificate is loaded.
func (s *s1Site) awaitHandshake(t *testing.T, name string) tls.ConnectionState {
	t.Helper()
	var state tls.ConnectionState
	testsupport.Poll(t, testsupport.Deadline, "a handshake for "+name+" to succeed", func() (bool, string) {
		st, err := s.tr.Handshake(name, s.ca.Pool())
		if err != nil {
			return false, err.Error()
		}
		state = st
		return true, ""
	})
	return state
}

// awaitGet polls until a request answers 200, which also waits for the backend to listen.
func (s *s1Site) awaitGet(t *testing.T, host, pathAndQuery string) string {
	t.Helper()
	var body string
	testsupport.Poll(t, testsupport.Deadline, "GET "+host+pathAndQuery+" to answer 200", func() (bool, string) {
		status, b, _, err := s.tr.TryGet(s.ca.Pool(), host, pathAndQuery, nil)
		if err != nil {
			return false, err.Error()
		}
		body = b
		return status == http.StatusOK, "status " + strconv.Itoa(status)
	})
	return body
}

// fullBundle is the good bundle: strict SNI, the leaf with absolute paths, and one route.
func fullBundle() s1Bundle {
	return s1Bundle{Options: true, CertFile: s1CertPath, KeyFile: s1KeyPath, Route: true}
}

// TestS1KnownSNIServesTheRouteLeaf is the tracer (D-01): a pinned Traefik on an internal network,
// started with the embedded static file and a bind-mounted bundle root, completes a handshake for a
// known SNI, presents the leaf the test CA issued for that hostname, and routes a request to the
// backend.
func TestS1KnownSNIServesTheRouteLeaf(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS1(t, fullBundle(), true)

	state := s.awaitHandshake(t, s1Host)
	if len(state.PeerCertificates) == 0 {
		t.Fatal("the handshake presented no certificate")
	}
	leaf := state.PeerCertificates[0]
	if got := leaf.DNSNames; len(got) != 1 || got[0] != s1Host {
		t.Fatalf("presented leaf names %v, want only %s", got, s1Host)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: s1Host, Roots: s.ca.Pool()}); err != nil {
		t.Fatalf("the presented leaf does not verify against the test CA: %v", err)
	}

	body := s.awaitGet(t, s1Host, "/shop")
	for _, want := range []string{"Hostname: shop-web", "GET /shop"} {
		if !strings.Contains(body, want) {
			t.Errorf("the backend's echo lacks %q:\n%s", want, body)
		}
	}
}
