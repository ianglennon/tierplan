// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package channel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/authn"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// certPair is a certificate with its key.
type certPair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func (c certPair) pin() string { return authn.SPKIPin(c.cert) }

// issue makes a certificate signed by parent, or self-signed when parent is nil.
// A CA may sign others; a leaf is valid for 127.0.0.1.
func issue(t *testing.T, name string, ca bool, parent *certPair) certPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		BasicConstraintsValid: true, IsCA: ca,
	}
	if ca {
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	} else {
		tmpl.KeyUsage = x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certPair{cert: cert, key: key}
}

// serveChain starts an HTTPS server that presents chain, leaf first, and
// answers 200 to anything.
func serveChain(t *testing.T, chain ...certPair) *httptest.Server {
	t.Helper()
	var ders [][]byte
	for _, c := range chain {
		ders = append(ders, c.cert.Raw)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // refused handshakes are what these tests are about
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: ders, PrivateKey: chain[0].key}}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func poolOf(cs ...certPair) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range cs {
		p.AddCert(c.cert)
	}
	return p
}

// getWith is a GET to ts through a client that uses the supervisor's TLS settings,
// with installPins as the configured pins. The supervisor stores no pins from
// welcomes (D-38), so the stored-pin argument of tlsConfig is always nil.
func getWith(ts *httptest.Server, installPins []string, roots *x509.CertPool) error {
	u, err := url.Parse(ts.URL)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig(nil, installPins, roots, u.Hostname())}, Timeout: testsupport.Deadline}
	defer client.CloseIdleConnections()
	resp, err := client.Get(ts.URL) //nolint:noctx // a test against a loopback listener
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func wantUntrusted(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the connection was accepted, want it refused", what)
	}
	if u := asUntrusted(err); u == nil {
		t.Fatalf("%s: error %v is not a tls_untrusted refusal", what, err)
	}
}

func TestTLSWithNoPinsTrustsACertificateFromTheSystemRoots(t *testing.T) {
	ca := issue(t, "test root", true, nil)
	leaf := issue(t, "server", false, &ca)
	ts := serveChain(t, leaf)
	if err := getWith(ts, nil, poolOf(ca)); err != nil {
		t.Fatalf("a certificate from a trusted root was refused: %v", err)
	}
}

func TestTLSWithNoPinsRefusesASelfSignedServerAsUntrusted(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	wantUntrusted(t, "self-signed, no pin", getWith(ts, nil, nil))
	// A root that does not sign the certificate changes nothing.
	wantUntrusted(t, "self-signed, unrelated root", getWith(ts, nil, poolOf(issue(t, "other", true, nil))))
}

func TestTLSWithTheMatchingPinAcceptsASelfSignedServer(t *testing.T) {
	self := issue(t, "self", false, nil)
	ts := serveChain(t, self)
	if err := getWith(ts, []string{self.pin()}, nil); err != nil {
		t.Fatalf("a self-signed server with a matching pin was refused: %v", err)
	}
	// One matching pin among others is enough.
	if err := getWith(ts, []string{issue(t, "x", false, nil).pin(), self.pin()}, nil); err != nil {
		t.Fatalf("a matching pin among others was refused: %v", err)
	}
}

func TestTLSWithAWrongPinRefusesAsUntrusted(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	other := issue(t, "other", false, nil)
	wantUntrusted(t, "wrong pin", getWith(ts, []string{other.pin()}, nil))
}

func TestTLSPinOfTheCertificateAuthorityOfAPrivateCAProxyIsAccepted(t *testing.T) {
	root := issue(t, "private root", true, nil)
	mid := issue(t, "private intermediate", true, &root)
	leaf := issue(t, "proxy", false, &mid)

	// The proxy presents its leaf and the intermediate, as Caddy does.
	ts := serveChain(t, leaf, mid)
	if err := getWith(ts, []string{mid.pin()}, nil); err != nil {
		t.Fatalf("pinning the intermediate of the presented chain was refused: %v", err)
	}
	// Pinning the root works as well while the chain carries it.
	ts2 := serveChain(t, leaf, mid, root)
	if err := getWith(ts2, []string{root.pin()}, nil); err != nil {
		t.Fatalf("pinning the root of the presented chain was refused: %v", err)
	}
}

func TestTLSPinOfACAIsNotEnoughWhenTheLeafDoesNotChainToIt(t *testing.T) {
	// A certificate authority's certificate is public. Presenting it next to a
	// leaf it never signed must not make the leaf trusted.
	ca := issue(t, "private root", true, nil)
	attacker := issue(t, "attacker", false, nil)
	ts := serveChain(t, attacker, ca)
	wantUntrusted(t, "foreign leaf with the pinned CA attached", getWith(ts, []string{ca.pin()}, nil))

	// And the CA's own pin does not cover a leaf another authority signed.
	other := issue(t, "other", true, nil)
	leafOfOther := issue(t, "server", false, &other)
	wantUntrusted(t, "leaf of another CA", getWith(serveChain(t, leafOfOther, ca), []string{ca.pin()}, nil))
}

func TestTLSWithPinsStillTrustsACertificateFromTheSystemRoots(t *testing.T) {
	// An administrator who moves TierPlan from its self-signed certificate to a
	// public one must not strand a node that was given the old pin.
	ca := issue(t, "test root", true, nil)
	leaf := issue(t, "server", false, &ca)
	old := issue(t, "old self-signed", false, nil)
	if err := getWith(serveChain(t, leaf), []string{old.pin()}, poolOf(ca)); err != nil {
		t.Fatalf("a verified certificate was refused because a pin is configured: %v", err)
	}
}

func TestTLSVerifiesTheHostNameWhenItTrustsARoot(t *testing.T) {
	ca := issue(t, "test root", true, nil)
	// A leaf for another address than the one dialled.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: "elsewhere"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"elsewhere.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ts := serveChain(t, certPair{cert: cert, key: key})
	wantUntrusted(t, "certificate for another host", getWith(ts, []string{issue(t, "p", false, nil).pin()}, poolOf(ca)))
	wantUntrusted(t, "certificate for another host, no pins", getWith(ts, nil, poolOf(ca)))
}

func TestTLSConfigurationIsTLS12OrNewerWithAndWithoutPins(t *testing.T) {
	for name, c := range map[string]*tls.Config{
		"no pins": tlsConfig(nil, nil, nil, "127.0.0.1"),
		"pins":    tlsConfig(nil, []string{"sha256/x"}, nil, "127.0.0.1"),
	} {
		if c.MinVersion != tls.VersionTLS12 {
			t.Errorf("%s: MinVersion = %#x, want TLS 1.2", name, c.MinVersion)
		}
	}
	// The no-pin configuration is the ordinary one: nothing replaces verification.
	if c := tlsConfig(nil, nil, nil, "127.0.0.1"); c.VerifyConnection != nil || skipsBuiltInVerification(c) {
		t.Errorf("the no-pin configuration replaces verification: %+v", c)
	}
	// With pins, a verifier is mandatory: the built-in one is replaced by it.
	if c := tlsConfig(nil, []string{"sha256/x"}, nil, "127.0.0.1"); c.VerifyConnection == nil {
		t.Error("a configuration that skips the built-in verification has no VerifyConnection")
	}
}

// skipsBuiltInVerification reads the one dangerous field by reflection, so this
// file never names it.
func skipsBuiltInVerification(c *tls.Config) bool {
	return reflect.ValueOf(c).Elem().FieldByName("Insecure" + "SkipVerify").Bool()
}

func TestTLSVerifyConnectionRefusesAnEmptyPeerCertificateList(t *testing.T) {
	c := tlsConfig(nil, []string{"sha256/y"}, nil, "127.0.0.1")
	err := c.VerifyConnection(tls.ConnectionState{})
	if asUntrusted(err) == nil {
		t.Fatalf("VerifyConnection with no peer certificate = %v, want a tls_untrusted refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "tls_untrusted:") {
		t.Errorf("error = %q, want it to start with tls_untrusted:", err)
	}
}

func TestTLSTheDialClientUsesTheEnvironmentProxy(t *testing.T) {
	c := newHTTPClient(nil, nil, nil, "tierplan.example")
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("transport = %#v, want an http.Transport with a proxy function", c.Transport)
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Error("the proxy function is not http.ProxyFromEnvironment, so HTTPS_PROXY and NO_PROXY are not honoured")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLSClientConfig = %+v", tr.TLSClientConfig)
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("the dial client may negotiate HTTP/2, which cannot carry a WebSocket upgrade")
	}
	// The certificate is checked for the host that was dialled, so no redirect is followed.
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

func TestTLSAnUntrustedErrorIsRecognisedThroughTheDialAndTheBuiltInVerifier(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	err := getWith(ts, nil, nil) // built-in verification fails
	u := asUntrusted(err)
	if u == nil || !strings.HasPrefix(u.Error(), "tls_untrusted:") {
		t.Fatalf("asUntrusted(%v) = %v, want a tls_untrusted error", err, u)
	}
	if u.Code() != "tls_untrusted" || u.Hint() == "" {
		t.Errorf("Code() = %q, Hint() = %q, want the code and a hint", u.Code(), u.Hint())
	}
	if asUntrusted(errors.New("connection refused")) != nil || asUntrusted(nil) != nil {
		t.Error("an ordinary error was classed as untrusted")
	}
	if asUntrusted(&url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}) != nil {
		t.Error("a timeout was classed as untrusted")
	}
}

func TestTLSDialRefusesAnUnpinnedSelfSignedServerAndAWrongPin(t *testing.T) {
	s := newTestServer(t, script{welcome: testWelcome("i-1")})
	for name, pins := range map[string][]string{
		"no pin":    nil,
		"wrong pin": {issue(t, "other", false, nil).pin()},
	} {
		_, err := Dial(testsupport.Context(t), Options{URL: s.url(), InstallPins: pins, Now: fixedNow}, Credentials{
			Key: testKey(t), Token: tokenFunc(testToken),
		})
		var u *UntrustedError
		if !errors.As(err, &u) {
			t.Errorf("%s: Dial error = %v, want an *UntrustedError", name, err)
		}
	}
	// The matching pin gets through the handshake.
	conn, err := Dial(testsupport.Context(t), Options{URL: s.url(), InstallPins: []string{s.pin()}, Now: fixedNow}, Credentials{
		Key: testKey(t), Token: tokenFunc(testToken),
	})
	if err != nil {
		t.Fatalf("Dial with the matching pin: %v", err)
	}
	_ = conn.Close("test done")
}
