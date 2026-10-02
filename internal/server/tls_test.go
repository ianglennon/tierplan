// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
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
	if c == nil || len(c.Certificates) != 1 {
		t.Fatalf("config = %+v, want exactly one certificate", c)
	}
	leaf, err := x509.ParseCertificate(c.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
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
	got, err := TLSConfig(config.Config{DataDir: t.TempDir()}, "", nowFn(fixedNow))
	if err != nil || got != nil {
		t.Fatalf("TLSConfig = %v, %v; want nil and no error (plain HTTP)", got, err)
	}
	if TLSEnabled(config.Config{}) {
		t.Fatal("TLSEnabled is true with no TLS settings")
	}
}

func TestTLSConfigRefusesFilesTogetherWithSelfSigned(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir(), TLSSelfSigned: true, TLSCertFile: "/c.pem", TLSKeyFile: "/k.pem"}
	_, err := TLSConfig(cfg, "", nowFn(fixedNow))
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"BAYPLAN_TLS_SELF_SIGNED", "BAYPLAN_TLS_CERT_FILE", "BAYPLAN_TLS_KEY_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestTLSSelfSignedWritesTheSANSetAndAPrivateKey(t *testing.T) {
	cfg := selfSignedConfig(t)
	got, err := TLSConfig(cfg, "https://bayplan.example.com:7480", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}

	leaf := leafOf(t, got)
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || leaf.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Errorf("public key = %T, want an ECDSA P-256 key", leaf.PublicKey)
	}
	if want := fixedNow.AddDate(1, 0, 0); leaf.NotAfter.Sub(want).Abs() > 2*time.Minute {
		t.Errorf("NotAfter = %v, want about %v (one year)", leaf.NotAfter, want)
	}
	for _, name := range []string{"localhost", "bayplan.example.com"} {
		if !slices.Contains(leaf.DNSNames, name) {
			t.Errorf("DNS SANs %v lack %q", leaf.DNSNames, name)
		}
	}
	if host, _ := os.Hostname(); host != "" && !slices.Contains(leaf.DNSNames, strings.ToLower(host)) {
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
	got, err := TLSConfig(selfSignedConfig(t), "http://192.0.2.10:7480", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	if ips := ipStrings(leafOf(t, got).IPAddresses); !slices.Equal(ips, []string{"127.0.0.1", "192.0.2.10", "::1"}) {
		t.Errorf("IP SANs = %v, want the public host as an IP SAN", ips)
	}
}

func TestTLSSelfSignedIsReusedOnTheSecondCall(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://a.example.com", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	second, err := TLSConfig(cfg, "https://a.example.com", nowFn(fixedNow.Add(24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, first).SerialNumber.Cmp(leafOf(t, second).SerialNumber) != 0 {
		t.Fatal("the certificate was regenerated although nothing changed")
	}
}

func TestTLSSelfSignedIsRegeneratedWhenThePublicHostChanges(t *testing.T) {
	cfg := selfSignedConfig(t)
	first, err := TLSConfig(cfg, "https://a.example.com", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	second, err := TLSConfig(cfg, "https://b.example.com", nowFn(fixedNow))
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
	first, err := TLSConfig(cfg, "", nowFn(fixedNow))
	if err != nil {
		t.Fatal(err)
	}
	// 29 days before the end of a one-year certificate.
	near := fixedNow.AddDate(1, 0, -29)
	second, err := TLSConfig(cfg, "", nowFn(near))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, second).SerialNumber.Cmp(leafOf(t, first).SerialNumber) == 0 {
		t.Fatal("the certificate was reused 29 days before it expires")
	}
	// 31 days before expiry is still fine.
	third, err := TLSConfig(cfg, "", nowFn(near.Add(-48*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if leafOf(t, third).SerialNumber.Cmp(leafOf(t, second).SerialNumber) != 0 {
		t.Fatal("a certificate with more than 30 days left was regenerated")
	}
}

func TestTLSSelfSignedIsRegeneratedWhenTheStoredPairIsBroken(t *testing.T) {
	cfg := selfSignedConfig(t)
	if _, err := TLSConfig(cfg, "", nowFn(fixedNow)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "tls", "selfsigned.crt"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TLSConfig(cfg, "", nowFn(fixedNow))
	if err != nil {
		t.Fatalf("a corrupt stored certificate must be replaced, got %v", err)
	}
	leafOf(t, got)
}

func TestTLSConfigLoadsASuppliedCertificatePair(t *testing.T) {
	src := selfSignedConfig(t)
	if _, err := TLSConfig(src, "https://files.example.com", nowFn(fixedNow)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(src.DataDir, "tls")
	cfg := config.Config{
		DataDir:     t.TempDir(),
		TLSCertFile: filepath.Join(dir, "selfsigned.crt"),
		TLSKeyFile:  filepath.Join(dir, "selfsigned.key"),
	}
	got, err := TLSConfig(cfg, "", nowFn(fixedNow))
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
	_, err := TLSConfig(cfg, "", nowFn(fixedNow))
	if err == nil || !strings.Contains(err.Error(), "BAYPLAN_TLS_CERT_FILE") {
		t.Fatalf("err = %v, want one naming BAYPLAN_TLS_CERT_FILE", err)
	}
}

func TestTLSFingerprintIsTheSHA256OfTheLeaf(t *testing.T) {
	got, err := TLSConfig(selfSignedConfig(t), "", nowFn(fixedNow))
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
		"BAYPLAN_DATA_DIR":        t.TempDir() + "/data",
		"BAYPLAN_TLS_SELF_SIGNED": "true",
		"BAYPLAN_PUBLIC_URL":      "https://bayplan.example.com",
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

	if err := Healthcheck(context.Background(), ":"+port, true); err != nil {
		t.Fatalf("Healthcheck over TLS: %v", err)
	}
	// Plain HTTP against the TLS listener must not pass as healthy.
	if err := Healthcheck(context.Background(), ":"+port, false); err == nil {
		t.Fatal("a plain-HTTP healthcheck against a TLS listener should fail")
	}

	// A real client sees HSTS and the certificate, and the server logs its fingerprint.
	pool := x509.NewCertPool()
	pool.AddCert(leafOf(t, srv.tlsConfig))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
	resp, err := client.Get("https://127.0.0.1:" + port + "/healthz") //nolint:noctx // test against a loopback listener
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Errorf("Strict-Transport-Security = %q, want max-age=31536000", got)
	}
}
