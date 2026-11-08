// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
)

var fixedNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func nowFn(t time.Time) func() time.Time { return func() time.Time { return t } }

func selfSignedConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{DataDir: t.TempDir(), TLSSelfSigned: true}
}

// leafOf parses the certificate a TLS config serves.
func leafOf(t *testing.T, c *tls.Config) *x509.Certificate {
	t.Helper()
	if c == nil || c.GetCertificate == nil {
		t.Fatalf("config = %+v, want one that serves its certificate through GetCertificate", c)
	}
	// A hello with no server name is the case a static Certificates list would
	// have answered without ever calling GetCertificate.
	cert, err := c.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// setHostname makes the machine host name the one given for the test.
func setHostname(t *testing.T, name string, err error) {
	t.Helper()
	old := osHostname
	osHostname = func() (string, error) { return name, err }
	t.Cleanup(func() { osHostname = old })
}

func ipStrings(ips []net.IP) []string {
	var out []string
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

func str(v string) func() string { return func() string { return v } }

// ssFixture is a self-signed install in memory: a configuration and the store its certificate lives in.
// Each call to source or tlsConfig is one start of the server over the same store.
type ssFixture struct {
	cfg   config.Config
	store *memCertStore
}

func newSSFixture(t *testing.T) ssFixture {
	t.Helper()
	return ssFixture{cfg: selfSignedConfig(t), store: newMemCertStore()}
}

func (f ssFixture) source(t *testing.T, public, agent string, now time.Time) *certSource {
	t.Helper()
	src, kind, err := newCertSource(context.Background(), f.cfg, f.store, str(public), str(agent), nowFn(now))
	if err != nil {
		t.Fatal(err)
	}
	if kind != tlsKindSelfSigned {
		t.Fatalf("kind = %v, want the self-signed kind", kind)
	}
	return src
}

func (f ssFixture) tlsConfig(t *testing.T, public, agent string, now time.Time) *tls.Config {
	t.Helper()
	return &tls.Config{GetCertificate: f.source(t, public, agent, now).GetCertificate, MinVersion: tls.VersionTLS12}
}

func TestTLSConfigIsNilWithoutTLSSettings(t *testing.T) {
	src, kind, err := newCertSource(context.Background(), config.Config{DataDir: t.TempDir()}, newMemCertStore(), nil, nil, nowFn(fixedNow))
	if err != nil || src != nil || kind != tlsKindNone {
		t.Fatalf("newCertSource = %v, %v, %v; want no source, none and no error (plain HTTP)", src, kind, err)
	}
	if TLSEnabled(config.Config{}) {
		t.Fatal("TLSEnabled is true with no TLS settings")
	}
}

func TestTLSConfigRefusesFilesTogetherWithSelfSigned(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), TLSSelfSigned: true, TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem"}
	_, _, err := newCertSource(context.Background(), cfg, newMemCertStore(), nil, nil, nowFn(fixedNow))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"TIERPLAN_TLS_SELF_SIGNED", "TIERPLAN_TLS_CERT_FILE", "TIERPLAN_TLS_KEY_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestTLSSelfSignedSANSetIsCorrectAndNoKeyFileIsWritten(t *testing.T) {
	f := newSSFixture(t)
	got := f.tlsConfig(t, "https://tierplan.example.com:7480", "", fixedNow)
	if got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}

	leaf := leafOf(t, got)
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key = %T, want an ECDSA P-256 key", leaf.PublicKey)
	}
	if want := fixedNow.Add(3650 * 24 * time.Hour); leaf.NotAfter.Sub(want).Abs() > 2*time.Minute {
		t.Errorf("NotAfter = %v, want about %v (ten years)", leaf.NotAfter, want)
	}
	for _, name := range []string{"localhost", "tierplan.example.com"} {
		if !slices.Contains(leaf.DNSNames, name) {
			t.Errorf("DNS SANs %v lack %q", leaf.DNSNames, name)
		}
	}
	if host, _ := os.Hostname(); host != "" && !looksLikeContainerID(strings.ToLower(host)) && !slices.Contains(leaf.DNSNames, strings.ToLower(host)) {
		t.Errorf("DNS SANs %v lack the hostname %q", leaf.DNSNames, host)
	}
	if ips := ipStrings(leaf.IPAddresses); !slices.Equal(ips, []string{"127.0.0.1", "::1"}) {
		t.Errorf("IP SANs = %v, want 127.0.0.1 and ::1", ips)
	}

	// Only the public certificate is on disk: the key lives in the store, and no temporary file is left.
	dir := filepath.Join(f.cfg.DataDir, "tls")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "server.crt" {
		t.Errorf("tls directory = %v, want exactly server.crt", entries)
	}
	if _, _, found, _ := f.store.Load(context.Background(), selfSignedID); !found {
		t.Error("the certificate was not stored")
	}
}

func TestTLSSelfSignedPutsAnIPPublicHostInTheIPSANs(t *testing.T) {
	got := newSSFixture(t).tlsConfig(t, "http://192.0.2.10:7480", "", fixedNow)
	if ips := ipStrings(leafOf(t, got).IPAddresses); !slices.Equal(ips, []string{"127.0.0.1", "192.0.2.10", "::1"}) {
		t.Errorf("IP SANs = %v, want the public host as an IP SAN", ips)
	}
	if dns := leafOf(t, got).DNSNames; slices.Contains(dns, "192.0.2.10") {
		t.Errorf("DNS SANs %v hold an IP literal", dns)
	}
}

func TestTLSSelfSignedIsReusedOnTheSecondCall(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "https://a.example.com", "", fixedNow)
	second := f.tlsConfig(t, "https://a.example.com", "", fixedNow.Add(24*time.Hour))
	if leafOf(t, first).SerialNumber.Cmp(leafOf(t, second).SerialNumber) != 0 {
		t.Fatal("the certificate was regenerated although nothing changed")
	}
	// The SAN list is built in a stable order, so a second start writes no row.
	if f.store.inserts != 1 || f.store.updates != 0 {
		t.Errorf("writes = %d inserts, %d updates; want 1 and 0", f.store.inserts, f.store.updates)
	}
}

func TestTLSSelfSignedIsResignedWithTheSameKeyWhenThePublicHostChanges(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "https://a.example.com", "", fixedNow)
	second := f.tlsConfig(t, "https://b.example.com", "", fixedNow)
	l := leafOf(t, second)
	if l.SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("the certificate was reused after the public host changed")
	}
	if !slices.Contains(l.DNSNames, "b.example.com") || slices.Contains(l.DNSNames, "a.example.com") {
		t.Errorf("DNS SANs = %v, want b.example.com and not a.example.com", l.DNSNames)
	}
	if a, b := pinOf(t, first), pinOf(t, second); a != b {
		t.Errorf("pin changed on a re-sign: %s then %s", a, b)
	}
	if f.store.inserts != 1 || f.store.updates != 1 {
		t.Errorf("writes = %d inserts, %d updates; want 1 and 1", f.store.inserts, f.store.updates)
	}
}

func TestTLSSelfSignedIsResignedAtStartOnlyOutsideItsValidity(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "", "", fixedNow)
	// Nine years on, with a year to go: never renewed because it is getting old.
	kept := f.tlsConfig(t, "", "", fixedNow.AddDate(9, 0, 0))
	if leafOf(t, kept).SerialNumber.Cmp(leafOf(t, first).SerialNumber) != 0 {
		t.Fatal("a certificate inside its validity was replaced")
	}
	// A start after it ran out re-signs it, with the same key.
	late := f.tlsConfig(t, "", "", fixedNow.AddDate(11, 0, 0))
	if leafOf(t, late).SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("an expired certificate was served")
	}
	if pinOf(t, late) != pinOf(t, first) {
		t.Error("the pin changed when an expired certificate was signed again")
	}
}

func TestTLSSelfSignedIsResignedWhenTheStoredCertificateIsBroken(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "", "", fixedNow)
	row := f.store.rows[selfSignedID]
	row.cert = []byte("not a certificate")
	f.store.rows[selfSignedID] = row

	got := f.tlsConfig(t, "", "", fixedNow)
	leafOf(t, got)
	if pinOf(t, got) != pinOf(t, first) {
		t.Fatal("a corrupt stored certificate was replaced with a new key")
	}
}

func TestTLSConfigLoadsASuppliedCertificatePair(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	writePair(t, crt, key, "files.example.com")
	cfg := config.Config{DataDir: t.TempDir(), TLSCertFile: crt, TLSKeyFile: key}
	src, kind, err := newCertSource(context.Background(), cfg, newMemCertStore(), nil, nil, nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if kind != tlsKindSupplied {
		t.Errorf("kind = %v, want supplied", kind)
	}
	got := &tls.Config{GetCertificate: src.GetCertificate}
	if !slices.Contains(leafOf(t, got).DNSNames, "files.example.com") {
		t.Error("the supplied pair was not the one loaded")
	}
	// Supplied files are never written to, and nothing is generated for them.
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "tls")); err == nil {
		t.Error("a tls directory was created although the certificate came from files")
	}
}

func TestTLSConfigNamesTheSettingWhenTheFilesCannotBeLoaded(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), TLSCertFile: "/nonexistent/c.pem", TLSKeyFile: "/nonexistent/k.pem"}
	_, _, err := newCertSource(context.Background(), cfg, newMemCertStore(), nil, nil, nowFn(fixedNow))
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TLS_CERT_FILE") {
		t.Fatalf("err = %v, want one naming TIERPLAN_TLS_CERT_FILE", err)
	}
}

func TestTLSFingerprintIsTheSHA256OfTheLeaf(t *testing.T) {
	got := newSSFixture(t).tlsConfig(t, "", "", fixedNow)
	fp := fingerprint(got)
	// Colon-separated upper-case hex, 32 bytes: 64 digits and 31 colons.
	if len(fp) != 95 || strings.Count(fp, ":") != 31 || fp != strings.ToUpper(fp) {
		t.Fatalf("fingerprint = %q", fp)
	}
	if fingerprint(nil) != "" {
		t.Error("fingerprint of no config must be empty")
	}
}

func TestTLSSelfSignedEncodesNamesCaseInsensitivelyAndCoversTheEmptyCase(t *testing.T) {
	setHostname(t, "3f2a9c1b7d4e", nil) // a container ID: left out
	f := newSSFixture(t)
	// Nothing configured: the certificate still covers the loopback names.
	empty := leafOf(t, f.tlsConfig(t, "", "", fixedNow))
	if !slices.Contains(empty.DNSNames, "localhost") || !slices.Equal(ipStrings(empty.IPAddresses), []string{"127.0.0.1", "::1"}) {
		t.Fatalf("empty case: DNS %v, IPs %v; want localhost, 127.0.0.1 and ::1", empty.DNSNames, empty.IPAddresses)
	}
	// The same host in another letter case is already covered: no re-sign.
	f.tlsConfig(t, "https://Tierplan.Example.COM", "", fixedNow)
	updates := f.store.updates
	again := leafOf(t, f.tlsConfig(t, "https://tierplan.example.com", "", fixedNow))
	if f.store.updates != updates {
		t.Error("a host that differs only in letter case caused a re-sign")
	}
	if !slices.Contains(again.DNSNames, "tierplan.example.com") {
		t.Errorf("DNS SANs %v lack the lower-cased host", again.DNSNames)
	}
}

func TestHealthcheckWorksAgainstTheServersOwnSelfSignedTLS(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":        t.TempDir() + "/data",
		"TIERPLAN_TLS_SELF_SIGNED": "true",
		"TIERPLAN_PUBLIC_URL":      "https://tierplan.example.com",
	})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if err := Healthcheck(context.Background(), ":"+port, HealthcheckCertFile(cfg)); err != nil {
		t.Fatalf("Healthcheck over TLS: %v", err)
	}
	// The certificate is verified: another one in the file is refused, and a
	// file that cannot be read is an error that names it.
	other := newSSFixture(t)
	other.source(t, "", "", fixedNow)
	if err := Healthcheck(context.Background(), ":"+port, HealthcheckCertFile(other.cfg)); err == nil {
		t.Fatal("a healthcheck trusting a different certificate passed")
	}
	missing := filepath.Join(t.TempDir(), "none.crt")
	if err := Healthcheck(context.Background(), ":"+port, missing); err == nil || !strings.Contains(err.Error(), "none.crt") {
		t.Fatalf("a missing certificate file gave %v, want an error naming it", err)
	}
	// The port answers plain HTTP as well (D-01), and /healthz is exempt from the redirect gate, so a
	// plain probe passes against a TLS install too.
	if err := Healthcheck(context.Background(), ":"+port, ""); err != nil {
		t.Fatalf("a plain-HTTP healthcheck against the shared listener: %v", err)
	}

	// A real client sees the certificate, and the server logs its fingerprint.
	pool := x509.NewCertPool()
	pool.AddCert(leafOf(t, srv.https.Config()))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
	resp, err := client.Get("https://127.0.0.1:" + port + "/healthz") //nolint:noctx // test against a loopback listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	// The generated certificate is never pinned with HSTS (WR-09).
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q with the self-signed certificate, want none", got)
	}
}

func TestTLSSelfSignedLeavesAContainerIDHostnameOut(t *testing.T) {
	setHostname(t, "3f2a9c1b7d4e", nil)
	got := newSSFixture(t).tlsConfig(t, "", "", fixedNow)
	if names := leafOf(t, got).DNSNames; slices.Contains(names, "3f2a9c1b7d4e") {
		t.Errorf("DNS SANs %v hold the container ID, which changes on every recreation", names)
	}

	full := strings.Repeat("ab12", 16)
	setHostname(t, full, nil)
	got = newSSFixture(t).tlsConfig(t, "", "", fixedNow)
	if names := leafOf(t, got).DNSNames; slices.Contains(names, full) {
		t.Errorf("DNS SANs %v hold the 64-hex container ID", names)
	}

	setHostname(t, "nas01", nil)
	got = newSSFixture(t).tlsConfig(t, "", "", fixedNow)
	if names := leafOf(t, got).DNSNames; !slices.Contains(names, "nas01") {
		t.Errorf("DNS SANs %v lack the machine host name nas01", names)
	}
}

func TestLooksLikeContainerID(t *testing.T) {
	for host, want := range map[string]bool{
		"3f2a9c1b7d4e":                  true,
		strings.Repeat("0f", 32):        true,
		"nas01":                         false,
		"3f2a9c1b7d4":                   false, // 11 digits
		"3f2a9c1b7d4e0":                 false, // 13 digits
		"3f2a9c1b7d4g":                  false, // not hex
		"3F2A9C1B7D4E":                  false, // Docker writes lower case
		"build-3f2a9c1b7d4e.example.io": false,
		"":                              false,
	} {
		if got := looksLikeContainerID(host); got != want {
			t.Errorf("looksLikeContainerID(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestTLSSelfSignedIsKeptWhenTheContainerIsRecreated(t *testing.T) {
	f := newSSFixture(t)
	setHostname(t, "aaaaaaaaaaaa", nil)
	first := f.tlsConfig(t, "https://tierplan.example.com", "", fixedNow)
	setHostname(t, "bbbbbbbbbbbb", nil)
	second := f.tlsConfig(t, "https://tierplan.example.com", "", fixedNow.Add(time.Hour))
	if fingerprint(first) != fingerprint(second) || fingerprint(first) == "" {
		t.Fatal("recreating the container changed the certificate, which would lock a browser out")
	}
}

func TestTLSSelfSignedIsKeptWhenItCoversMoreThanIsWanted(t *testing.T) {
	f := newSSFixture(t)
	setHostname(t, "nas01", nil)
	first := f.tlsConfig(t, "", "", fixedNow)
	for name, hostname := range map[string]func(){
		"a failing host name lookup": func() { setHostname(t, "", errors.New("no hostname")) },
		"a container ID host name":   func() { setHostname(t, "cccccccccccc", nil) },
	} {
		hostname()
		again := f.tlsConfig(t, "", "", fixedNow.Add(time.Hour))
		if fingerprint(again) != fingerprint(first) {
			t.Errorf("%s: a certificate that covers every wanted name was replaced", name)
		}
	}
}

// writePair stores a fresh self-signed pair at the given paths and returns its
// leaf. The two files are what an operator's certificate renewal replaces.
func writePair(t *testing.T, crt, key string, host string) *x509.Certificate {
	t.Helper()
	k, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := signCertificate(k, wantedNames("https://"+host, ""), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil { //nolint:gosec // a path inside the test's temporary directory
		t.Fatal(err)
	}
	if err := os.WriteFile(key, keyPEM, 0o600); err != nil { //nolint:gosec // a path inside the test's temporary directory
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestSelfSignedIsNeverRenewedOnATimer(t *testing.T) {
	f := newSSFixture(t)
	src := f.source(t, "https://a.example.com", "", fixedNow)
	got := &tls.Config{GetCertificate: src.GetCertificate}
	first := leafOf(t, got)

	// Past 30 days before expiry, and past a year: the same certificate is served and nothing is written.
	for _, at := range []time.Time{
		fixedNow.AddDate(0, 0, 3650-29),
		fixedNow.AddDate(1, 0, 0),
		fixedNow.AddDate(5, 0, 0),
	} {
		src.Refresh(at)
		if leafOf(t, got).SerialNumber.Cmp(first.SerialNumber) != 0 {
			t.Fatalf("the certificate was replaced when the clock reached %v", at)
		}
	}
	if f.store.updates != 0 {
		t.Errorf("%d certificate writes with the names unchanged, want none", f.store.updates)
	}
}

func TestSelfSignedIsResignedLiveWhenTheURLChanges(t *testing.T) {
	f := newSSFixture(t)
	clk := newStepClock(fixedNow)
	var public atomic.Pointer[string]
	setPublic := func(v string) { public.Store(&v) }
	setPublic("")
	src, _, err := newCertSource(context.Background(), f.cfg, f.store, func() string { return *public.Load() }, str(""), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	got := &tls.Config{GetCertificate: src.GetCertificate}
	before := leafOf(t, got)
	pin := pinOf(t, got)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runCertRefresh(ctx, clk, src)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// A URL saved by the wizard or Settings after start is covered within one name-check interval.
	setPublic("https://new.example.test")
	clk.Tick(t, selfSignedNameCheck)
	clk.WaitArmed(t) // the loop arms again once Refresh has run

	after := leafOf(t, got)
	if !slices.Contains(after.DNSNames, "new.example.test") {
		t.Fatalf("DNS SANs = %v, want the saved URL's host", after.DNSNames)
	}
	if after.SerialNumber.Cmp(before.SerialNumber) == 0 {
		t.Error("the serial did not change, so nothing was signed")
	}
	if got := pinOf(t, got); got != pin {
		t.Errorf("pin = %s after the re-sign, want %s", got, pin)
	}
	// The row and the mirror both hold the new certificate.
	stored, _, _, _ := f.store.Load(context.Background(), selfSignedID)
	mirror, err := os.ReadFile(filepath.Join(f.cfg.DataDir, "tls", mirrorCertFile)) //nolint:gosec // a path inside the test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"row": stored, "mirror": mirror} {
		block, _ := pem.Decode(data)
		if block == nil {
			t.Fatalf("the %s holds no certificate", name)
		}
		if block.Type != "CERTIFICATE" || !bytes.Equal(block.Bytes, after.Raw) {
			t.Errorf("the %s does not hold the certificate being served", name)
		}
	}
	if f.store.updates != 1 {
		t.Errorf("%d certificate writes, want 1", f.store.updates)
	}

	// A host already covered, in another letter case, is not signed again.
	setPublic("https://NEW.example.test")
	clk.Advance(selfSignedNameCheck) // the loop is already waiting on its timer
	clk.WaitArmed(t)
	if f.store.updates != 1 || leafOf(t, got).SerialNumber.Cmp(after.SerialNumber) != 0 {
		t.Error("a covered host in another letter case caused a re-sign")
	}
}

func TestTLSSuppliedCertificateIsReloadedWhenTheFilesChange(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	a := writePair(t, crt, key, "a.example.com")

	src, _, err := newCertSource(context.Background(), config.Config{DataDir: t.TempDir(), TLSCertFile: crt, TLSKeyFile: key}, newMemCertStore(), nil, nil, nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	got := &tls.Config{GetCertificate: src.GetCertificate}
	if leafOf(t, got).SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatal("pair A is not the one served")
	}

	b := writePair(t, crt, key, "b.example.com")
	// Within the reload interval nothing is re-read.
	src.Refresh(fixedNow.Add(10 * time.Second))
	if leafOf(t, got).SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Error("the files were re-read inside the reload interval")
	}
	src.Refresh(fixedNow.Add(2 * time.Minute))
	if leafOf(t, got).SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("pair B was not picked up after the files were replaced")
	}

	// A half-written or broken replacement must not take the site down.
	for _, f := range []string{crt, key} {
		if err := os.WriteFile(f, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src.Refresh(fixedNow.Add(5 * time.Minute))
	if leafOf(t, got).SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("a broken replacement displaced the working certificate")
	}
}

// servedLeaf opens a fresh TLS connection to addr and returns the certificate the server presents.
func servedLeaf(t *testing.T, addr string, roots *x509.CertPool) *x509.Certificate {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("TLS handshake with %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0]
}

func TestTLSSuppliedPairIsServedWithinOneRecheckWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	a := writePair(t, crt, key, "a.example.com")

	clk := newStepClock(fixedNow)
	srv, err := Build(context.Background(), testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":      t.TempDir() + "/data",
		"TIERPLAN_TLS_CERT_FILE": crt,
		"TIERPLAN_TLS_KEY_FILE":  key,
	}), slog.New(slog.DiscardHandler), WithTLSRefreshClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	addr := ln.Addr().String()

	// The client trusts both pairs, so what it sees is decided by the server alone.
	b := writePair(t, filepath.Join(dir, "b.pem"), filepath.Join(dir, "b.key"), "b.example.com")
	roots := x509.NewCertPool()
	roots.AddCert(a)
	roots.AddCert(b)

	if got := servedLeaf(t, addr, roots); got.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatal("pair A is not the one served")
	}

	// A connection opened under A, kept alive across the swap.
	held := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
	t.Cleanup(held.CloseIdleConnections)
	get := func() (reused bool, peer *x509.Certificate) {
		t.Helper()
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, "https://"+addr+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := held.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return reused, resp.TLS.PeerCertificates[0]
	}
	if _, peer := get(); peer.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Fatal("the held connection was not opened under pair A")
	}

	// The operator replaces the pair. One recheck interval later, with no restart, B is served.
	if err := os.Rename(filepath.Join(dir, "b.pem"), crt); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "b.key"), key); err != nil {
		t.Fatal(err)
	}
	clk.Tick(t, suppliedRecheck)
	clk.WaitArmed(t) // the loop arms again once Refresh has run

	if got := servedLeaf(t, addr, roots); got.SerialNumber.Cmp(b.SerialNumber) != 0 {
		t.Fatal("pair B is not served within one recheck interval of the files being replaced")
	}
	reused, peer := get()
	if !reused {
		t.Error("the connection opened before the swap was not reused, so it did not survive it")
	}
	if peer.SerialNumber.Cmp(a.SerialNumber) != 0 {
		t.Error("the connection opened before the swap does not still show pair A")
	}
}

func TestTLSCertificateIsServedWhenTheClientSendsNoServerName(t *testing.T) {
	// Go uses a static Certificates list in preference to GetCertificate when
	// the client sends no SNI (a connection to an IP address), which would
	// silently pin that client to the first certificate for ever.
	got := newSSFixture(t).tlsConfig(t, "", "", fixedNow)
	if len(got.Certificates) != 0 {
		t.Errorf("Certificates holds %d entries; it must stay empty so GetCertificate always answers", len(got.Certificates))
	}
}

// pinOf is the SPKI pin of what a TLS config serves.
func pinOf(t *testing.T, c *tls.Config) string {
	t.Helper()
	pin := spkiPin(c)
	if !strings.HasPrefix(pin, "sha256/") {
		t.Fatalf("spkiPin = %q, want a sha256/ pin", pin)
	}
	return pin
}

func TestSPKIPinIsStableWhenTheCertificateIsSignedAgainAfterItRanOut(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "", "", fixedNow)
	second := f.tlsConfig(t, "", "", fixedNow.AddDate(11, 0, 0))
	if leafOf(t, first).SerialNumber.Cmp(leafOf(t, second).SerialNumber) == 0 {
		t.Fatal("the certificate was not signed again, so the test proves nothing")
	}
	if a, b := pinOf(t, first), pinOf(t, second); a != b {
		t.Fatalf("pin changed on a re-sign: %s then %s", a, b)
	}
}

func TestSPKIPinIsStableWhenTheWantedNamesChange(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "https://a.example.com", "", fixedNow)
	second := f.tlsConfig(t, "https://b.example.com", "", fixedNow)
	l := leafOf(t, second)
	if !slices.Contains(l.DNSNames, "b.example.com") {
		t.Fatalf("DNS SANs = %v, want the new host", l.DNSNames)
	}
	if a, b := pinOf(t, first), pinOf(t, second); a != b {
		t.Fatalf("pin changed with the host name: %s then %s", a, b)
	}
}

// A key that cannot be read stops startup. It is never replaced, because a new key would change the
// pin that every agent holds (T-13.1-03). Plan 04 adds the legacy-file and unreadable-envelope cases.
func TestAStoredKeyThatCannotBeReadStopsStartupAndIsNotReplaced(t *testing.T) {
	f := newSSFixture(t)
	f.tlsConfig(t, "", "", fixedNow)
	row := f.store.rows[selfSignedID]
	row.key = []byte("not a key")
	f.store.rows[selfSignedID] = row

	_, _, err := newCertSource(context.Background(), f.cfg, f.store, str(""), str(""), nowFn(fixedNow))
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TLS_SELF_SIGNED") {
		t.Fatalf("err = %v, want one that names the setting", err)
	}
	if string(f.store.rows[selfSignedID].key) != "not a key" || f.store.inserts != 1 || f.store.updates != 0 {
		t.Error("the unreadable row was replaced or rewritten")
	}
}

func TestLoadKeyReadsAnEncodedKeyFile(t *testing.T) {
	k, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(k)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadKey(path)
	if err != nil || !got.Equal(k) {
		t.Fatalf("loadKey = %v, %v; want the key that was written", got, err)
	}
	if _, err := loadKey(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("loadKey of a missing file succeeded")
	}
}

func TestSPKIPinIsEmptyWithoutACertificate(t *testing.T) {
	if got := spkiPin(nil); got != "" {
		t.Errorf("spkiPin(nil) = %q, want empty", got)
	}
}

func TestServerTLSPinIsPublishedOnlyForTheSelfSignedCertificate(t *testing.T) {
	build := func(env map[string]string) *Server {
		t.Helper()
		env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}

	self := build(map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true"})
	if got, want := self.TLSPin(), spkiPin(self.https.Config()); got == "" || got != want {
		t.Errorf("self-signed TLSPin() = %q, want %q", got, want)
	}

	src := t.TempDir()
	writePair(t, filepath.Join(src, "c.pem"), filepath.Join(src, "k.pem"), "supplied.example.com")
	supplied := build(map[string]string{
		"TIERPLAN_TLS_CERT_FILE": filepath.Join(src, "c.pem"),
		"TIERPLAN_TLS_KEY_FILE":  filepath.Join(src, "k.pem"),
	})
	if got := supplied.TLSPin(); got != "" {
		t.Errorf("supplied certificate TLSPin() = %q, want empty", got)
	}

	if got := build(map[string]string{}).TLSPin(); got != "" {
		t.Errorf("plain HTTP TLSPin() = %q, want empty", got)
	}
}

func TestServerWelcomePinsComeFromTheSelfSignedPin(t *testing.T) {
	self := func(env map[string]string) *Server {
		env["TIERPLAN_DATA_DIR"] = t.TempDir() + "/data"
		srv, err := Build(context.Background(), testConfig(t, env), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		return srv
	}
	srv := self(map[string]string{"TIERPLAN_TLS_SELF_SIGNED": "true"})
	if got := srv.Tunnel().Pins(); len(got) != 1 || got[0] != srv.TLSPin() || !strings.HasPrefix(got[0], "sha256/") {
		t.Errorf("self-signed welcome pins = %v, want exactly [%s]", got, srv.TLSPin())
	}
	if got := self(map[string]string{}).Tunnel().Pins(); len(got) != 0 {
		t.Errorf("plain HTTP welcome pins = %v, want none", got)
	}
}

func TestTLSSelfSignedCoversTheAgentURLHostAndKeepsThePin(t *testing.T) {
	f := newSSFixture(t)
	first := f.tlsConfig(t, "https://tierplan.example.com", "", fixedNow)
	pin := spkiPin(first)
	if pin == "" {
		t.Fatal("no pin for the first certificate")
	}

	// Naming an agent URL on another host re-signs the certificate for both
	// hosts. The key is the same, so the pin installed agents hold still matches.
	second := f.tlsConfig(t, "https://tierplan.example.com", "https://tierplan.lan:7480", fixedNow)
	l := leafOf(t, second)
	if l.SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("the certificate was reused although the agent URL host is new")
	}
	for _, name := range []string{"tierplan.example.com", "tierplan.lan"} {
		if !slices.Contains(l.DNSNames, name) {
			t.Errorf("DNS SANs %v lack %q", l.DNSNames, name)
		}
	}
	if got := spkiPin(second); got != pin {
		t.Errorf("pin = %q after the agent URL changed, want %q: a pinned agent would stop connecting", got, pin)
	}

	// An agent URL given as an address is an IP SAN.
	third := f.tlsConfig(t, "https://tierplan.example.com", "http://192.0.2.44:7480", fixedNow)
	if ips := ipStrings(leafOf(t, third).IPAddresses); !slices.Contains(ips, "192.0.2.44") {
		t.Errorf("IP SANs = %v, want the agent URL address", ips)
	}
	if got := spkiPin(third); got != pin {
		t.Errorf("pin = %q, want %q", got, pin)
	}
}

func TestWantedNamesTakesBothURLs(t *testing.T) {
	setHostname(t, "", errors.New("no hostname"))
	got := wantedNames("https://a.example.com/tierplan", "https://b.example.com:7480")
	if !slices.Equal(got.dns, []string{"a.example.com", "b.example.com", "localhost"}) {
		t.Errorf("dns = %v, want both hosts and localhost", got.dns)
	}
	// An empty or unparsable agent URL adds nothing.
	for _, agent := range []string{"", "::not a url::"} {
		if got := wantedNames("https://a.example.com", agent); !slices.Equal(got.dns, []string{"a.example.com", "localhost"}) {
			t.Errorf("agent URL %q: dns = %v", agent, got.dns)
		}
	}
}
