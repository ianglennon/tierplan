// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// importLine is the log line the one-time import writes.
const importLine = "imported the self-signed TLS key into the database"

// captureDefaultLog routes the default logger, which the TLS code writes to, into a buffer for the
// rest of the test.
func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	old := slog.Default()
	buf := &lockedBuffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// legacyFiles are the two files an earlier TierPlan kept under <dataDir>/tls.
type legacyFiles struct {
	dir, key, cert string
}

func legacyPaths(dataDir string) legacyFiles {
	dir := filepath.Join(dataDir, selfSignedDir)
	return legacyFiles{
		dir:  dir,
		key:  filepath.Join(dir, selfSignedKeyFile),
		cert: filepath.Join(dir, selfSignedCertFile),
	}
}

// writeLegacyPair leaves the pair a Phase 13 install had: a PKCS#8 ECDSA P-256 key and a
// certificate that is valid for one year. It returns the key.
func writeLegacyPair(t *testing.T, dataDir string, now time.Time) *ecdsa.PrivateKey {
	t.Helper()
	p := legacyPaths(dataDir)
	if err := os.MkdirAll(p.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.key, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	writeOneYearCert(t, p.cert, key, now)
	return key
}

// writeOneYearCert writes the certificate a Phase 13 install made: one year, signed by its own key.
func writeOneYearCert(t *testing.T, path string, key *ecdsa.PrivateKey, now time.Time) {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "TierPlan self-signed"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // a public certificate
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// An install from before the database held the key starts on this version with the pin it had, a
// ten-year certificate and no key file; the second start imports nothing.
func TestImportKeepsThePinAndResignsForTenYears(t *testing.T) {
	logs := captureDefaultLog(t)
	dataDir := t.TempDir() + "/data"
	key := writeLegacyPair(t, dataDir, time.Now())
	wantPin := keyPin(&key.PublicKey)
	p := legacyPaths(dataDir)

	cfg := testConfig(t, map[string]string{"TIERPLAN_DATA_DIR": dataDir})
	srv, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	if got := srv.TLSPin(); got != wantPin {
		t.Errorf("served pin = %q, want the legacy key's pin %q", got, wantPin)
	}
	leaf := leafOf(t, srv.https.Config())
	if want := time.Now().Add(selfSignedValidity); leaf.NotAfter.Sub(want).Abs() > 24*time.Hour {
		t.Errorf("NotAfter = %v, want about %v (ten years)", leaf.NotAfter, want)
	}
	if exists(t, p.key) || exists(t, p.cert) {
		t.Errorf("legacy files remain: key %v, cert %v", exists(t, p.key), exists(t, p.cert))
	}
	if !exists(t, filepath.Join(p.dir, mirrorCertFile)) {
		t.Errorf("%s was not written", mirrorCertFile)
	}
	var updated int64
	var rows int
	row := func() {
		t.Helper()
		if err := srv.store.Read.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at), 0) FROM tls_certs WHERE id = ?`, selfSignedID).Scan(&rows, &updated); err != nil {
			t.Fatal(err)
		}
	}
	row()
	if rows != 1 {
		t.Fatalf("tls_certs rows = %d, want 1", rows)
	}
	firstUpdated := updated
	if n := strings.Count(logs.String(), importLine); n != 1 {
		t.Errorf("the import line was logged %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), wantPin) {
		t.Errorf("the import line does not carry tls_pin %q:\n%s", wantPin, logs)
	}
	if strings.Contains(logs.String(), "PRIVATE KEY") {
		t.Error("the log holds key material")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// The second start finds the row: no import, no write, the same pin.
	logs2 := captureDefaultLog(t)
	srv2, err := Build(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv2.Close() })
	srv = srv2
	row()
	if rows != 1 || updated != firstUpdated {
		t.Errorf("after the second start: %d rows, updated_at %d; want 1 row and %d untouched", rows, updated, firstUpdated)
	}
	if strings.Contains(logs2.String(), importLine) {
		t.Errorf("the second start imported again:\n%s", logs2)
	}
	if got := srv2.TLSPin(); got != wantPin {
		t.Errorf("pin after the second start = %q, want %q", got, wantPin)
	}
}
