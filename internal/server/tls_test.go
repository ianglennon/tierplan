// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
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

func TestTLSConfigIsNilWithoutTLSSettings(t *testing.T) {
	got, err := TLSConfig(config.Config{DataDir: t.TempDir()}, "", "", nowFn(fixedNow))
	if err != nil || got != nil {
		t.Fatalf("TLSConfig = %v, %v; want nil and no error (plain HTTP)", got, err)
	}
	if TLSEnabled(config.Config{}) {
		t.Fatal("TLSEnabled is true with no TLS settings")
	}
}

func TestTLSConfigRefusesFilesTogetherWithSelfSigned(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), TLSSelfSigned: true, TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem"}
	_, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"TIERPLAN_TLS_SELF_SIGNED", "TIERPLAN_TLS_CERT_FILE", "TIERPLAN_TLS_KEY_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestTLSSelfSignedWritesTheSANSetAndAPrivateKey(t *testing.T) {
	cfg := selfSignedConfig(t)
	got, err := TLSConfig(cfg, "https://tierplan.example.com:7480", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}

	leaf := leafOf(t, got)
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("public key = %T, want an ECDSA P-256 key", leaf.PublicKey)
	}
	if want := fixedNow.AddDate(1, 0, 0); leaf.NotAfter.Sub(want).Abs() > 2*time.Minute {
		t.Errorf("NotAfter = %v, want about %v (one year)", leaf.NotAfter, want)
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

	dir := filepath.Join(cfg.DataDir, "tls")
	if fi, err := os.Stat(filepath.Join(dir, "selfsigned.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("selfsigned.key: %v, %v; want mode 0600", fi, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "selfsigned.crt")); err != nil {
		t.Errorf("selfsigned.crt: %v", err)
	}
	// The files are written atomically: no temporary file is left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("tls directory holds %d entries, want exactly the key and the certificate", len(entries))
	}
}

func TestTLSSelfSignedPutsAnIPPublicHostInTheIPSANs(t *testing.T) {
	got, err := TLSConfig(selfSignedConfig(t), "http://192.0.2.10:7480", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if ips := ipStrings(leafOf(t, got).IPAddresses); !slices.Equal(ips, []string{"127.0.0.1", "192.0.2.10", "::1"}) {
		t.Errorf("IP SANs = %v, want the public host as an IP SAN", ips)
	}
}

func TestTLSSelfSignedIsReusedOnTheSecondCall(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://a.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	second, err := TLSConfig(cfg, "https://a.example.com", "", nowFn(fixedNow.Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, first).SerialNumber.Cmp(leafOf(t, second).SerialNumber) != 0 {
		t.Fatal("the certificate was regenerated although nothing changed")
	}
}

func TestTLSSelfSignedIsRegeneratedWhenThePublicHostChanges(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://a.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	second, err := TLSConfig(cfg, "https://b.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	l := leafOf(t, second)
	if l.SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("the certificate was reused after the public host changed")
	}
	if !slices.Contains(l.DNSNames, "b.example.com") || slices.Contains(l.DNSNames, "a.example.com") {
		t.Errorf("DNS SANs = %v, want b.example.com and not a.example.com", l.DNSNames)
	}
}

func TestTLSSelfSignedIsRegeneratedWithin30DaysOfExpiry(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	// 29 days before the end of a one-year certificate.
	near := fixedNow.AddDate(1, 0, -29)
	second, err := TLSConfig(cfg, "", "", nowFn(near))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, second).SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("the certificate was reused 29 days before it expires")
	}
	// The replacement is good for a year, so a day later it is kept.
	third, err := TLSConfig(cfg, "", "", nowFn(near.Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, third).SerialNumber.Cmp(leafOf(t, second).SerialNumber) != 0 {
		t.Fatal("a certificate with more than 30 days left was regenerated")
	}
	// And one with 31 days left is kept as well.
	fresh := selfSignedConfig(t)
	a, err := TLSConfig(fresh, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	b, err := TLSConfig(fresh, "", "", nowFn(fixedNow.AddDate(1, 0, -31)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, a).SerialNumber.Cmp(leafOf(t, b).SerialNumber) != 0 {
		t.Fatal("a certificate with 31 days left was regenerated")
	}
}

func TestTLSSelfSignedIsRegeneratedWhenTheStoredPairIsBroken(t *testing.T) {
	cfg := selfSignedConfig(t)
	if _, err := TLSConfig(cfg, "", "", nowFn(fixedNow)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "tls", "selfsigned.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatalf("a corrupt stored certificate must be replaced, got %v", err)
	}
	leafOf(t, got)
}

func TestTLSConfigLoadsASuppliedCertificatePair(t *testing.T) {
	src := selfSignedConfig(t)
	if _, err := TLSConfig(src, "https://files.example.com", "", nowFn(fixedNow)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(src.DataDir, "tls")
	cfg := config.Config{
		DataDir:     t.TempDir(),
		TLSCertFile: filepath.Join(dir, "selfsigned.crt"),
		TLSKeyFile:  filepath.Join(dir, "selfsigned.key"),
	}
	got, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}
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
	_, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err == nil || !strings.Contains(err.Error(), "TIERPLAN_TLS_CERT_FILE") {
		t.Fatalf("err = %v, want one naming TIERPLAN_TLS_CERT_FILE", err)
	}
}

func TestTLSFingerprintIsTheSHA256OfTheLeaf(t *testing.T) {
	got, err := TLSConfig(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	fp := fingerprint(got)
	// Colon-separated upper-case hex, 32 bytes: 64 digits and 31 colons.
	if len(fp) != 95 || strings.Count(fp, ":") != 31 || fp != strings.ToUpper(fp) {
		t.Fatalf("fingerprint = %q", fp)
	}
	if fingerprint(nil) != "" {
		t.Error("fingerprint of no config must be empty")
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
	other := selfSignedConfig(t)
	if _, err := TLSConfig(other, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := Healthcheck(context.Background(), ":"+port, HealthcheckCertFile(other)); err == nil {
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
	got, err := TLSConfig(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if names := leafOf(t, got).DNSNames; slices.Contains(names, "3f2a9c1b7d4e") {
		t.Errorf("DNS SANs %v hold the container ID, which changes on every recreation", names)
	}

	full := strings.Repeat("ab12", 16)
	setHostname(t, full, nil)
	got, err = TLSConfig(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if names := leafOf(t, got).DNSNames; slices.Contains(names, full) {
		t.Errorf("DNS SANs %v hold the 64-hex container ID", names)
	}

	setHostname(t, "nas01", nil)
	got, err = TLSConfig(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
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
	cfg := selfSignedConfig(t)
	setHostname(t, "aaaaaaaaaaaa", nil)
	first, err := TLSConfig(cfg, "https://tierplan.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	setHostname(t, "bbbbbbbbbbbb", nil)
	second, err := TLSConfig(cfg, "https://tierplan.example.com", "", nowFn(fixedNow.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint(first) != fingerprint(second) || fingerprint(first) == "" {
		t.Fatal("recreating the container changed the certificate, which would lock a browser out")
	}
}

func TestTLSSelfSignedIsKeptWhenItCoversMoreThanIsWanted(t *testing.T) {
	cfg := selfSignedConfig(t)
	setHostname(t, "nas01", nil)
	first, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	for name, hostname := range map[string]func(){
		"a failing host name lookup": func() { setHostname(t, "", errors.New("no hostname")) },
		"a container ID host name":   func() { setHostname(t, "cccccccccccc", nil) },
	} {
		hostname()
		again, err := TLSConfig(cfg, "", "", nowFn(fixedNow.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		if fingerprint(again) != fingerprint(first) {
			t.Errorf("%s: a certificate that covers every wanted name was replaced", name)
		}
	}
}

func TestTLSSelfSignedIsRenewedWhileRunning(t *testing.T) {
	src, _, err := newCertSource(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	got := &tls.Config{GetCertificate: src.GetCertificate}
	first := leafOf(t, got)

	// Well inside the year: the same certificate is served.
	src.Refresh(fixedNow.Add(24 * time.Hour))
	if leafOf(t, got).SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("the certificate was replaced a day after it was made")
	}

	// 25 days before it expires, with the process still running.
	src.Refresh(fixedNow.Add(340 * 24 * time.Hour))
	renewed := leafOf(t, got)
	if !renewed.NotAfter.After(first.NotAfter) {
		t.Fatalf("NotAfter = %v after renewal, want later than %v", renewed.NotAfter, first.NotAfter)
	}
	if renewed.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Error("the serial number did not change, so nothing was regenerated")
	}
}

// writePair stores a fresh self-signed pair at the given paths and returns its
// leaf. The two files are what an operator's certificate renewal replaces.
func writePair(t *testing.T, crt, key string, host string) *x509.Certificate {
	t.Helper()
	src := config.Config{DataDir: t.TempDir(), TLSSelfSigned: true}
	got, err := TLSConfig(src, "https://"+host, "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	for from, to := range map[string]string{
		filepath.Join(src.DataDir, "tls", "selfsigned.crt"): crt,
		filepath.Join(src.DataDir, "tls", "selfsigned.key"): key,
	} {
		b, err := os.ReadFile(from) //nolint:gosec // a path inside the test's temporary directory
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o600); err != nil { //nolint:gosec // a path inside the test's temporary directory
			t.Fatal(err)
		}
	}
	return leafOf(t, got)
}

func TestTLSSuppliedCertificateIsReloadedWhenTheFilesChange(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	a := writePair(t, crt, key, "a.example.com")

	src, _, err := newCertSource(config.Config{DataDir: t.TempDir(), TLSCertFile: crt, TLSKeyFile: key}, "", "", nowFn(fixedNow))
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
	got, err := TLSConfig(selfSignedConfig(t), "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
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

func TestSPKIPinIsStableWhenTheCertificateIsRenewed(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	// 29 days before the end of a one-year certificate: a forced renewal.
	second, err := TLSConfig(cfg, "", "", nowFn(fixedNow.AddDate(1, 0, -29)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, first).SerialNumber.Cmp(leafOf(t, second).SerialNumber) == 0 {
		t.Fatal("the certificate was not renewed, so the test proves nothing")
	}
	if a, b := pinOf(t, first), pinOf(t, second); a != b {
		t.Fatalf("pin changed on renewal: %s then %s", a, b)
	}
}

func TestSPKIPinIsStableWhileRunningAcrossARenewal(t *testing.T) {
	cfg := selfSignedConfig(t)
	src, _, err := newCertSource(cfg, "", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	got := &tls.Config{GetCertificate: src.GetCertificate}
	before := pinOf(t, got)
	src.Refresh(fixedNow.AddDate(1, 0, -29).Add(selfSignedRecheck))
	if after := pinOf(t, got); after != before {
		t.Fatalf("pin changed in a running renewal: %s then %s", before, after)
	}
}

func TestSPKIPinIsStableWhenTheWantedNamesChange(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://a.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	second, err := TLSConfig(cfg, "https://b.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	l := leafOf(t, second)
	if !slices.Contains(l.DNSNames, "b.example.com") {
		t.Fatalf("DNS SANs = %v, want the new host", l.DNSNames)
	}
	if a, b := pinOf(t, first), pinOf(t, second); a != b {
		t.Fatalf("pin changed with the host name: %s then %s", a, b)
	}
}

func TestSPKIPinChangesAndIsLoggedWhenTheKeyIsMissingOrCorrupt(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T, keyPath string){
		"missing": func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt": func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte("not a key"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := selfSignedConfig(t)
			first, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
			if err != nil {
				t.Fatal(err)
			}
			damage(t, filepath.Join(cfg.DataDir, selfSignedDir, selfSignedKeyFile))

			var buf strings.Builder
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(old) })

			second, err := TLSConfig(cfg, "", "", nowFn(fixedNow))
			if err != nil {
				t.Fatalf("a lost key must be replaced, got %v", err)
			}
			if pinOf(t, first) == pinOf(t, second) {
				t.Fatal("pin is unchanged although the key was replaced")
			}
			if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "tls_pin") {
				t.Fatalf("log = %q, want a warning that names tls_pin", out)
			}
		})
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
	if _, err := TLSConfig(config.Config{DataDir: src, TLSSelfSigned: true}, "", "", nil); err != nil {
		t.Fatal(err)
	}
	supplied := build(map[string]string{
		"TIERPLAN_TLS_CERT_FILE": filepath.Join(src, selfSignedDir, selfSignedCertFile),
		"TIERPLAN_TLS_KEY_FILE":  filepath.Join(src, selfSignedDir, selfSignedKeyFile),
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
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://tierplan.example.com", "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	pin := spkiPin(first)
	if pin == "" {
		t.Fatal("no pin for the first certificate")
	}

	// Naming an agent URL on another host re-signs the certificate for both
	// hosts. The key is the same, so the pin installed agents hold still matches.
	second, err := TLSConfig(cfg, "https://tierplan.example.com", "https://tierplan.lan:7480", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
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
	third, err := TLSConfig(cfg, "https://tierplan.example.com", "http://192.0.2.44:7480", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
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
