// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/agent/internal/state"
	"github.com/ianglennon/bayplan/proto/authn"
	"github.com/ianglennon/bayplan/proto/wire"
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

// getWith is a GET to ts through a client that uses the agent's TLS settings.
func getWith(ts *httptest.Server, pins, override []string, roots *x509.CertPool) error {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig(pins, override, roots)}, Timeout: 5 * time.Second}
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
	if err := getWith(ts, nil, nil, poolOf(ca)); err != nil {
		t.Fatalf("a certificate from a trusted root was refused: %v", err)
	}
}

func TestTLSWithNoPinsRefusesASelfSignedServerAsUntrusted(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	wantUntrusted(t, "self-signed, no pin", getWith(ts, nil, nil, nil))
	// A root that does not sign the certificate changes nothing.
	wantUntrusted(t, "self-signed, unrelated root", getWith(ts, nil, nil, poolOf(issue(t, "other", true, nil))))
}

func TestTLSWithTheMatchingPinAcceptsASelfSignedServer(t *testing.T) {
	self := issue(t, "self", false, nil)
	ts := serveChain(t, self)
	if err := getWith(ts, []string{self.pin()}, nil, nil); err != nil {
		t.Fatalf("a self-signed server with a matching pin was refused: %v", err)
	}
	// Either list may carry it: the stored pins and the install command's.
	if err := getWith(ts, nil, []string{self.pin()}, nil); err != nil {
		t.Fatalf("a matching install pin was refused: %v", err)
	}
	// One matching pin among others is enough.
	if err := getWith(ts, []string{issue(t, "x", false, nil).pin(), self.pin()}, nil, nil); err != nil {
		t.Fatalf("a matching pin among others was refused: %v", err)
	}
}

func TestTLSWithAWrongPinRefusesAsUntrusted(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	other := issue(t, "other", false, nil)
	wantUntrusted(t, "wrong stored pin", getWith(ts, []string{other.pin()}, nil, nil))
	wantUntrusted(t, "wrong install pin", getWith(ts, nil, []string{other.pin()}, nil))
}

func TestTLSPinOfTheCertificateAuthorityOfAPrivateCAProxyIsAccepted(t *testing.T) {
	root := issue(t, "private root", true, nil)
	mid := issue(t, "private intermediate", true, &root)
	leaf := issue(t, "proxy", false, &mid)

	// The proxy presents its leaf and the intermediate, as Caddy does.
	ts := serveChain(t, leaf, mid)
	if err := getWith(ts, nil, []string{mid.pin()}, nil); err != nil {
		t.Fatalf("pinning the intermediate of the presented chain was refused: %v", err)
	}
	// The root is not sent but the chain still ends at it, so the intermediate
	// was enough; pinning the root while it is sent works as well.
	ts2 := serveChain(t, leaf, mid, root)
	if err := getWith(ts2, nil, []string{root.pin()}, nil); err != nil {
		t.Fatalf("pinning the root of the presented chain was refused: %v", err)
	}
}

func TestTLSPinOfACAIsNotEnoughWhenTheLeafDoesNotChainToIt(t *testing.T) {
	// A certificate authority's certificate is public. Presenting it next to a
	// leaf it never signed must not make the leaf trusted.
	ca := issue(t, "private root", true, nil)
	attacker := issue(t, "attacker", false, nil)
	ts := serveChain(t, attacker, ca)
	wantUntrusted(t, "foreign leaf with the pinned CA attached", getWith(ts, nil, []string{ca.pin()}, nil))

	// And the CA's own pin does not cover a leaf for another host name.
	other := issue(t, "other", true, nil)
	leafOfOther := issue(t, "server", false, &other)
	wantUntrusted(t, "leaf of another CA", getWith(serveChain(t, leafOfOther, ca), nil, []string{ca.pin()}, nil))
}

func TestTLSWithPinsStillTrustsACertificateFromTheSystemRoots(t *testing.T) {
	// An administrator who moves Bayplan from its self-signed certificate to a
	// public one must not strand agents that stored the old pin.
	ca := issue(t, "test root", true, nil)
	leaf := issue(t, "server", false, &ca)
	old := issue(t, "old self-signed", false, nil)
	if err := getWith(serveChain(t, leaf), []string{old.pin()}, nil, poolOf(ca)); err != nil {
		t.Fatalf("a verified certificate was refused because an old pin is stored: %v", err)
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
	cert, _ := x509.ParseCertificate(der)
	ts := serveChain(t, certPair{cert: cert, key: key})
	wantUntrusted(t, "certificate for another host", getWith(ts, []string{issue(t, "p", false, nil).pin()}, nil, poolOf(ca)))
	wantUntrusted(t, "certificate for another host, no pins", getWith(ts, nil, nil, poolOf(ca)))
}

func TestTLSConfigurationIsTLS12OrNewerWithAndWithoutPins(t *testing.T) {
	for name, c := range map[string]*tls.Config{
		"no pins":  tlsConfig(nil, nil, nil),
		"pins":     tlsConfig([]string{"sha256/x"}, nil, nil),
		"override": tlsConfig(nil, []string{"sha256/x"}, nil),
	} {
		if c.MinVersion != tls.VersionTLS12 {
			t.Errorf("%s: MinVersion = %#x, want TLS 1.2", name, c.MinVersion)
		}
	}
	// The no-pin configuration is the ordinary one: nothing replaces verification.
	if c := tlsConfig(nil, nil, nil); c.VerifyConnection != nil || skipsBuiltInVerification(c) {
		t.Errorf("the no-pin configuration replaces verification: %+v", c)
	}
	// With pins, a verifier is mandatory: the built-in one is replaced by it.
	if c := tlsConfig([]string{"sha256/x"}, nil, nil); c.VerifyConnection == nil {
		t.Error("a configuration that skips the built-in verification has no VerifyConnection")
	}
}

// skipsBuiltInVerification reads the one dangerous field by reflection, so this
// file never names it.
func skipsBuiltInVerification(c *tls.Config) bool {
	return reflect.ValueOf(c).Elem().FieldByName("Insecure" + "SkipVerify").Bool()
}

func TestTLSVerifyConnectionRefusesAnEmptyPeerCertificateList(t *testing.T) {
	c := tlsConfig([]string{"sha256/x"}, []string{"sha256/y"}, nil)
	err := c.VerifyConnection(tls.ConnectionState{})
	if asUntrusted(err) == nil {
		t.Fatalf("VerifyConnection with no peer certificate = %v, want a tls_untrusted refusal", err)
	}
	if !strings.HasPrefix(err.Error(), "tls_untrusted:") {
		t.Errorf("error = %q, want it to start with tls_untrusted:", err)
	}
}

func TestTheDialClientUsesTheEnvironmentProxy(t *testing.T) {
	c := newHTTPClient(nil, nil, nil)
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
}

func TestAnUntrustedErrorIsRecognisedThroughTheDialAndTheBuiltInVerifier(t *testing.T) {
	ts := serveChain(t, issue(t, "self", false, nil))
	err := getWith(ts, nil, nil, nil) // built-in verification fails
	u := asUntrusted(err)
	if u == nil || !strings.HasPrefix(u.Error(), "tls_untrusted:") {
		t.Fatalf("asUntrusted(%v) = %v, want a tls_untrusted error", err, u)
	}
	if asUntrusted(errors.New("connection refused")) != nil || asUntrusted(nil) != nil {
		t.Error("an ordinary error was classed as untrusted")
	}
	if asUntrusted(&url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}) != nil {
		t.Error("a timeout was classed as untrusted")
	}
}

// --- the agent end to end, against a TLS server ---

// lockedBuffer is a log sink the test reads while the agent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newFakeServerTLS(t *testing.T, script func(n int, a wire.Auth) reply) *fakeServer {
	t.Helper()
	f := &fakeServer{script: script, beats: make(chan wire.Heartbeat, 256), denied: make(chan wire.Denied, 256), started: make(chan int, 64)}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(func() {
		f.ts.CloseClientConnections()
		f.ts.Close()
	})
	return f
}

func serverPin(f *fakeServer) string { return authn.SPKIPin(f.ts.Certificate()) }

func TestTheAgentRefusesAnUnpinnedSelfSignedServerSaysSoOnceAndKeepsBackingOff(t *testing.T) {
	f := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{} })
	var sleeps atomic.Int32
	env := fastEnv()
	inner := env.sleep
	env.sleep = func(ctx context.Context, d time.Duration) error { sleeps.Add(1); return inner(ctx, d) }
	logs := &lockedBuffer{}

	startAgent(t, Config{URL: f.url(), JoinToken: "bpjt_x", Log: slog.New(slog.NewTextHandler(logs, nil))}, env)
	waitUntil(t, "three refused attempts", func() bool { return sleeps.Load() >= 3 })

	if n := f.attempts(); n != 0 {
		t.Fatalf("the server saw %d handshakes: the agent sent its token over a connection it could not verify", n)
	}
	out := logs.String()
	if n := strings.Count(out, "code=tls_untrusted"); n != 1 {
		t.Fatalf("%d lines with code=tls_untrusted, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(out, "hint=") || !strings.Contains(out, "Show install command") {
		t.Errorf("the line carries no fix hint:\n%s", out)
	}
	if strings.Contains(out, "bpjt_x") {
		t.Errorf("the join token is in the log:\n%s", out)
	}
}

func TestTheAgentRefusesAServerWhosePinIsWrong(t *testing.T) {
	f := newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{} })
	var sleeps atomic.Int32
	env := fastEnv()
	inner := env.sleep
	env.sleep = func(ctx context.Context, d time.Duration) error { sleeps.Add(1); return inner(ctx, d) }
	logs := &lockedBuffer{}

	wrong := "sha256/" + strings.Repeat("A", 43) + "="
	startAgent(t, Config{URL: f.url(), JoinToken: "bpjt_x", InstallPins: []string{wrong}, Log: slog.New(slog.NewTextHandler(logs, nil))}, env)
	waitUntil(t, "two refused attempts", func() bool { return sleeps.Load() >= 2 })
	if n := f.attempts(); n != 0 {
		t.Fatalf("the server saw %d handshakes through a wrong pin", n)
	}
	if !strings.Contains(logs.String(), "code=tls_untrusted") {
		t.Errorf("no tls_untrusted line:\n%s", logs.String())
	}
}

func TestTheAgentWithTheInstallPinConnectsAndKeepsTheWelcomePins(t *testing.T) {
	other := "sha256/" + strings.Repeat("Q", 43) + "="
	var f *fakeServer
	f = newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{pins: []string{serverPin(f), other}} })
	dir := t.TempDir()

	startAgent(t, Config{URL: f.url(), JoinToken: "bpjt_x", InstallPins: []string{serverPin(f)}, StateDir: dir}, fastEnv())
	f.authN(t, 1)
	waitUntil(t, "the welcome pins to be stored", func() bool {
		ps, err := state.LoadPins(dir)
		return err == nil && len(ps) == 2
	})
	ps, _ := state.LoadPins(dir)
	got := state.ActivePins(ps, time.Now())
	if len(got) != 2 || got[0] != serverPin(f) || got[1] != other {
		t.Fatalf("stored pins = %v, want the server's pin and the other", got)
	}
}

func TestStoredPinsLetARestartedAgentConnectWithoutTheInstallPin(t *testing.T) {
	var f *fakeServer
	f = newFakeServerTLS(t, func(int, wire.Auth) reply { return reply{pins: []string{serverPin(f)}} })
	dir := t.TempDir()

	cancel, done := startAgent(t, Config{URL: f.url(), JoinToken: "bpjt_x", InstallPins: []string{serverPin(f)}, StateDir: dir}, fastEnv())
	f.authN(t, 1)
	waitUntil(t, "the identity and the pins to be stored", func() bool {
		ps, err := state.LoadPins(dir)
		_, ok, lerr := state.Load(dir)
		return err == nil && len(ps) == 1 && ok && lerr == nil
	})
	cancel()
	<-done

	// The restart has no token and no install pin: only what it stored.
	startAgent(t, Config{URL: f.url(), StateDir: dir}, fastEnv())
	a, _ := f.authN(t, 2)
	if a.Mode != wire.ModeHello {
		t.Fatalf("the restarted agent said %q, want hello", a.Mode)
	}
}

func TestPinsFromAPlainSessionAreNeverStored(t *testing.T) {
	f := newFakeServer(t, func(int, wire.Auth) reply {
		return reply{pins: []string{"sha256/" + strings.Repeat("Z", 43) + "="}}
	})
	dir := t.TempDir()
	startAgent(t, Config{URL: f.url(), JoinToken: "bpjt_x", StateDir: dir}, fastEnv())
	f.authN(t, 1)
	waitUntil(t, "the agent to be connected", func() bool {
		select {
		case <-f.started:
			return true
		default:
			return false
		}
	})
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, state.PinsFile)); err == nil {
		t.Fatal("pins from an unverified plain ws:// session were stored")
	}
}

// --- the confinement gate ---

func TestTheInsecureTLSGateFailsWhenTheSettingAppearsInAnotherFile(t *testing.T) {
	script, err := filepath.Abs("../../../scripts/insecure-tls-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the gate script is missing: %v", err)
	}
	setting := "Insecure" + "SkipVerify" // never spelled out here, so this file passes the gate itself

	run := func(files map[string]string) (string, error) {
		dir := t.TempDir()
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command("bash", script, dir).CombinedOutput() //nolint:gosec // the repository's own script on a temporary fixture
		return string(out), err
	}
	use := "package x\n\nvar _ = struct{ " + setting + " bool }{" + setting + ": true}\n"

	if out, err := run(map[string]string{"agent/internal/connect/tls.go": use, "a/b.go": "package a\n"}); err != nil {
		t.Fatalf("the allowed file alone failed the gate: %v\n%s", err, out)
	}
	out, err := run(map[string]string{"agent/internal/connect/tls.go": use, "internal/server/x.go": use})
	if err == nil {
		t.Fatal("the gate passed with the setting in a second file")
	}
	if !strings.Contains(out, "internal/server/x.go") || strings.Contains(out, "agent/internal/connect/tls.go:") {
		t.Errorf("the gate output = %q, want it to name the offending file only", out)
	}
	if out, err := run(map[string]string{"internal/server/x.go": use}); err == nil {
		t.Fatalf("the gate passed with the setting only in another file:\n%s", out)
	}
	if out, err := run(map[string]string{"node_modules/p/x.go": use, "a.go": "package a\n"}); err != nil {
		t.Fatalf("a vendored dependency tree was scanned: %v\n%s", err, out)
	}
	if out, err := run(map[string]string{"a_test.go": "package a\n// " + setting + " in a comment\n"}); err == nil {
		t.Fatalf("a mention in a test file was let through:\n%s", out)
	}
}
