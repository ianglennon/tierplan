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

	// s1Unknown is a hostname nothing serves.
	s1Unknown = "b.test.local"
	// s1Loopback is the IP literal the tests send as an SNI.
	s1Loopback = "127.0.0.1"

	s1CertPath = traefiktest.NodeRoot + "/certs/a.crt"
	s1KeyPath  = traefiktest.NodeRoot + "/certs/a.key"

	// unrecognizedName is the text of the TLS alert strict SNI answers with.
	unrecognizedName = "unrecognized name"
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
	return startS1Files(t, env, ca, b.files())
}

// startS1Files is startS1 for an arbitrary file set, such as none at all (an empty dynamic directory).
func startS1Files(t *testing.T, env *traefiktest.Env, ca *traefiktest.CA, files []traefiktest.FileSpec) *s1Site {
	t.Helper()
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_web", Hostname: "shop-web"})
	env.Apply(t, files)
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

// awaitRefusal polls until Traefik refuses name with the unrecognized-name alert, which tells the test
// the configuration with strict SNI has been loaded (before that it presents its default certificate).
func (s *s1Site) awaitRefusal(t *testing.T, name string) {
	t.Helper()
	testsupport.Poll(t, testsupport.Deadline, "Traefik to refuse "+name+" as an unrecognized name", func() (bool, string) {
		_, err := s.tr.Handshake(name, s.ca.Pool())
		if err == nil {
			return false, "the handshake succeeded"
		}
		return strings.Contains(err.Error(), unrecognizedName), err.Error()
	})
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

// requireRefused fails unless err is the unrecognized-name alert with no certificate presented: a
// default certificate would surface as a verification error that names its subject instead.
func requireRefused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: the handshake succeeded, want the %q alert", what, unrecognizedName)
		return
	}
	if !strings.Contains(err.Error(), unrecognizedName) {
		t.Errorf("%s: handshake error %q does not contain %q", what, err, unrecognizedName)
	}
	if got := traefiktest.PresentedSubject(err); got != "" {
		t.Errorf("%s: Traefik presented a certificate (subject %q), want none", what, got)
	}
}

// requireRefusesUnservedNames asserts the three ways a client can ask for a name nothing serves: an
// unknown name, no SNI at all, and an IP address as SNI.
func (s *s1Site) requireRefusesUnservedNames(t *testing.T) {
	t.Helper()
	_, err := s.tr.Handshake(s1Unknown, s.ca.Pool())
	requireRefused(t, "unknown SNI "+s1Unknown, err)
	_, err = s.tr.Handshake("", s.ca.Pool())
	requireRefused(t, "no SNI", err)
	requireRefused(t, "IP address as SNI", s.tr.HandshakeIPSNI(s1Loopback))
}

// TestS1StrictSNI proves RTE-01 on the pinned Traefik (D-04, D-23): with tls.options.default holding
// sniStrict, a known name is served and an unknown name, no SNI and an IP address each fail the
// handshake with the unrecognized-name alert and no certificate presented.
func TestS1StrictSNI(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS1(t, fullBundle(), true)
	// The known name succeeds only once the bundle is loaded, so the refusals below are the strict
	// configuration speaking and not a proxy that has not read its files yet.
	s.awaitHandshake(t, s1Host)
	s.requireRefusesUnservedNames(t)
}

// TestS1EmptyBundleFailsClosed proves the empty bundle is safe (D-23, Pitfall 1): a dynamic file that
// holds only tls.options.default, with no route and no certificate, still refuses every name. This is
// the bundle a node must hold before Traefik starts.
func TestS1EmptyBundleFailsClosed(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	s := startS1(t, s1Bundle{Options: true}, false)
	// Before the file is read Traefik presents its default certificate, so wait for the alert.
	s.awaitRefusal(t, s1Unknown)
	s.requireRefusesUnservedNames(t)
	requireRefused(t, "the known name with nothing to serve it", errOf(s.tr.Handshake(s1Host, s.ca.Pool())))
}

func errOf(_ tls.ConnectionState, err error) error { return err }

// TestS1DefaultCertWithoutTLSOptions is the negative control (D-23, O3 and O4): without
// tls.options.default Traefik presents its own generated certificate for a name it does not serve.
// That is what the strict-SNI assertions above would see if the options were ever lost from a bundle,
// so a pass there means something: the harness can tell a broken bundle from a good one.
func TestS1DefaultCertWithoutTLSOptions(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)

	requireDefaultCert := func(t *testing.T, s *s1Site) {
		t.Helper()
		_, err := s.tr.Handshake(s1Unknown, s.ca.Pool())
		requireDefault(t, "unknown SNI "+s1Unknown, err)
		_, err = s.tr.Handshake("", s.ca.Pool())
		requireDefault(t, "no SNI", err)
		// The rewritten hello of HandshakeIPSNI cannot show the certificate of a handshake that goes
		// on (the client's transcript no longer matches the server's, so it fails with a bad record
		// MAC before any certificate is read; D-30). What it can show is that Traefik did not
		// refuse the name: without strict SNI an IP address is not met with the alert.
		if err := s.tr.HandshakeIPSNI(s1Loopback); err == nil || strings.Contains(err.Error(), unrecognizedName) {
			t.Errorf("IP address as SNI: handshake error %v, want a failure that is not the %q alert", err, unrecognizedName)
		}
	}

	t.Run("empty dynamic directory", func(t *testing.T) {
		env := traefiktest.NewEnv(t)
		s := startS1Files(t, env, traefiktest.NewCA(t), nil)
		// Nothing is loaded, so the default certificate is there as soon as Traefik listens.
		testsupport.Poll(t, testsupport.Deadline, "Traefik to present its default certificate", func() (bool, string) {
			_, err := s.tr.Handshake(s1Unknown, s.ca.Pool())
			return traefiktest.PresentedSubject(err) != "", errString(err)
		})
		requireDefaultCert(t, s)
	})

	t.Run("routes and a certificate but no tls options", func(t *testing.T) {
		b := fullBundle()
		b.Options = false
		s := startS1(t, b, true)
		// The known name works once the file is read; the others then reach the default store.
		s.awaitHandshake(t, s1Host)
		requireDefaultCert(t, s)
	})
}

func requireDefault(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: the handshake succeeded", what)
		return
	}
	if got := traefiktest.PresentedSubject(err); got != traefiktest.DefaultCertSubject {
		t.Errorf("%s: presented subject %q (error %v), want %q", what, got, err, traefiktest.DefaultCertSubject)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestS1AbsoluteCertPaths proves D-24 (O14): certFile and keyFile are read as absolute paths under the
// node root. A path relative to the YAML's own directory, and one relative to Traefik's working
// directory, load nothing, so the hostname is refused. The renderer therefore writes absolute paths
// and the proxy image mounts the bundle root at that same absolute path.
func TestS1AbsoluteCertPaths(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)

	t.Run("absolute path loads", func(t *testing.T) {
		s := startS1(t, fullBundle(), true)
		s.awaitHandshake(t, s1Host)
	})

	for _, tc := range []struct{ name, cert, key string }{
		{"relative to the YAML's directory", "../certs/a.crt", "../certs/a.key"},
		{"relative to the bundle root", "certs/a.crt", "certs/a.key"},
	} {
		t.Run(tc.name+" loads nothing", func(t *testing.T) {
			b := fullBundle()
			b.CertFile, b.KeyFile = tc.cert, tc.key
			s := startS1(t, b, true)
			// With the certificate not loaded the options still are, so the alert tells the file was read.
			s.awaitRefusal(t, s1Host)
			_, err := s.tr.Handshake(s1Host, s.ca.Pool())
			requireRefused(t, "the known name with the certificate path "+tc.cert, err)
			t.Logf("certFile %q was not loaded: a relative path is not read against the YAML's directory or the bundle root (O14: Traefik reads it against its own working directory, which is / in this image)", tc.cert)
		})
	}
}
