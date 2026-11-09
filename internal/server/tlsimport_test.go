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
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/vault"
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

// prepare is one start of the self-signed source over store: what the server does at Build.
func prepare(t *testing.T, store tlsCertStore, dataDir string) (*ecdsa.PrivateKey, error) {
	t.Helper()
	_, key, err := prepareSelfSigned(context.Background(), store, dataDir, wantedNames("", ""), fixedNow)
	return key, err
}

// failRemoval makes the removal of a legacy file fail until the test ends or restore is called.
func failRemoval(t *testing.T) (restore func()) {
	t.Helper()
	old := removeFile
	removeFile = func(string) error { return errors.New("the file system is read-only") }
	restore = func() { removeFile = old }
	t.Cleanup(restore)
	return restore
}

// A failure at any step of the import leaves the key file in place, and the next start finishes the
// import with the pin the install had (D-11, T-13.1-08).
func TestImportSurvivesAFailureAtEachStep(t *testing.T) {
	steps := []struct {
		name string
		// inject breaks one step and returns what undoes it for the next start.
		inject  func(t *testing.T, m *memCertStore) (undo func())
		wantErr bool
		// keyKept is whether selfsigned.key is still there after the failed start.
		keyKept bool
		wantLog string
	}{
		{name: "no failure", inject: func(*testing.T, *memCertStore) func() { return func() {} }, keyKept: false},
		{
			name: "insert",
			inject: func(_ *testing.T, m *memCertStore) func() {
				m.failInsert = errors.New("the database is full")
				return func() { m.failInsert = nil }
			},
			wantErr: true, keyKept: true,
		},
		{
			name: "checkpoint",
			inject: func(_ *testing.T, m *memCertStore) func() {
				m.failCheckpoint = errors.New("the checkpoint was blocked by another connection")
				return func() { m.failCheckpoint = nil }
			},
			wantErr: true, keyKept: true,
		},
		{
			name: "compare",
			inject: func(_ *testing.T, m *memCertStore) func() {
				m.otherKey = true
				return func() { m.otherKey = false }
			},
			wantErr: true, keyKept: true,
		},
		{
			name: "remove",
			inject: func(t *testing.T, _ *memCertStore) func() {
				return failRemoval(t)
			},
			keyKept: true,
			wantLog: "could not remove a legacy self-signed TLS file",
		},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			dataDir := t.TempDir()
			key := writeLegacyPair(t, dataDir, fixedNow)
			wantPin := keyPin(&key.PublicKey)
			p := legacyPaths(dataDir)
			m := newMemCertStore()

			undo := step.inject(t, m)
			got, err := prepare(t, m, dataDir)
			if (err != nil) != step.wantErr {
				t.Fatalf("start with the %s step broken: err = %v, want an error: %v", step.name, err, step.wantErr)
			}
			if exists(t, p.key) != step.keyKept {
				t.Errorf("selfsigned.key present = %v after the %s step failed, want %v", exists(t, p.key), step.name, step.keyKept)
			}
			if step.wantErr && !exists(t, p.cert) {
				t.Errorf("selfsigned.crt was removed although the import failed")
			}
			if err == nil && keyPin(&got.PublicKey) != wantPin {
				t.Errorf("served pin = %s, want the legacy pin %s", keyPin(&got.PublicKey), wantPin)
			}
			if step.wantLog != "" && !strings.Contains(logs.String(), step.wantLog) {
				t.Errorf("log lacks %q:\n%s", step.wantLog, logs)
			}

			// The next start, with the fault gone, ends with the same pin and no legacy files.
			undo()
			got, err = prepare(t, m, dataDir)
			if err != nil {
				t.Fatalf("the start after the %s failure: %v", step.name, err)
			}
			if keyPin(&got.PublicKey) != wantPin {
				t.Errorf("pin after the %s failure = %s, want %s", step.name, keyPin(&got.PublicKey), wantPin)
			}
			if exists(t, p.key) || exists(t, p.cert) {
				t.Errorf("legacy files remain after the next start: key %v, cert %v", exists(t, p.key), exists(t, p.cert))
			}
			if m.updates != 0 {
				t.Errorf("the certificate was rewritten %d times", m.updates)
			}
		})
	}
}

// A row in the database always wins over a legacy file, which is only a leftover (T-13.1-09).
func TestStoredKeyWinsOverAStrayKeyFile(t *testing.T) {
	strays := []struct {
		name    string
		write   func(t *testing.T, dataDir string, stored *ecdsa.PrivateKey)
		wantLog string
	}{
		{
			name: "another key",
			write: func(t *testing.T, dataDir string, _ *ecdsa.PrivateKey) {
				writeLegacyPair(t, dataDir, fixedNow)
			},
			wantLog: "whose key differs",
		},
		{
			name: "the stored key",
			write: func(t *testing.T, dataDir string, stored *ecdsa.PrivateKey) {
				p := legacyPaths(dataDir)
				if err := os.MkdirAll(p.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				keyPEM, err := encodeKey(stored)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.key, keyPEM, 0o600); err != nil {
					t.Fatal(err)
				}
				writeOneYearCert(t, p.cert, stored, fixedNow)
			},
		},
		{
			name: "a file that is not a key",
			write: func(t *testing.T, dataDir string, _ *ecdsa.PrivateKey) {
				p := legacyPaths(dataDir)
				if err := os.MkdirAll(p.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.key, []byte("not a key"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "cannot be read",
		},
	}
	for _, stray := range strays {
		t.Run(stray.name, func(t *testing.T) {
			dataDir := t.TempDir()
			m := newMemCertStore()
			stored, err := prepare(t, m, dataDir)
			if err != nil {
				t.Fatal(err)
			}
			storedPin := keyPin(&stored.PublicKey)

			logs := captureDefaultLog(t)
			stray.write(t, dataDir, stored)
			got, err := prepare(t, m, dataDir)
			if err != nil {
				t.Fatalf("start with a stray key file: %v", err)
			}
			if keyPin(&got.PublicKey) != storedPin {
				t.Errorf("served pin = %s, want the stored key's pin %s", keyPin(&got.PublicKey), storedPin)
			}
			p := legacyPaths(dataDir)
			if exists(t, p.key) || exists(t, p.cert) {
				t.Errorf("the stray files remain: key %v, cert %v", exists(t, p.key), exists(t, p.cert))
			}
			if stray.wantLog == "" {
				if strings.Contains(logs.String(), "level=WARN") {
					t.Errorf("a stray copy of the stored key caused a warning:\n%s", logs)
				}
			} else if !strings.Contains(logs.String(), stray.wantLog) {
				t.Errorf("log lacks %q:\n%s", stray.wantLog, logs)
			}
			if stray.name == "another key" {
				if !strings.Contains(logs.String(), storedPin) {
					t.Errorf("the warning does not name the stored pin %s:\n%s", storedPin, logs)
				}
			}
			if m.inserts != 1 || m.updates != 0 {
				t.Errorf("writes = %d inserts, %d updates; want the original insert only", m.inserts, m.updates)
			}
		})
	}
}

// A row that does not open stops startup with the vault's error. No key is made, the row is left as
// it is, and a legacy key file is not touched, because it may be the only good copy.
func TestAnUnreadableStoredKeyStopsStartup(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	good := newSealedStore(t)
	if _, err := prepare(t, good, dataDir); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := good.st.Read.QueryRowContext(ctx, `SELECT key_sealed FROM tls_certs WHERE id = ?`, selfSignedID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	writeLegacyPair(t, dataDir, fixedNow)

	other, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepare(t, sealedCertStore{st: good.st, v: other}, dataDir)
	if err == nil {
		t.Fatal("startup with a vault that cannot open the TLS key succeeded")
	}
	if !errors.Is(err, vault.ErrUnknownKey) {
		t.Errorf("err = %v, want one wrapping the vault's error", err)
	}
	var after string
	var rows int
	if err := good.st.Read.QueryRowContext(ctx, `SELECT COUNT(*), MAX(key_sealed) FROM tls_certs`).Scan(&rows, &after); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || after != before {
		t.Errorf("tls_certs = %d rows, key_sealed unchanged %v; want the one row exactly as it was", rows, after == before)
	}
	if !exists(t, legacyPaths(dataDir).key) {
		t.Error("the legacy key file was removed although the stored key could not be opened")
	}
}

// With no row and no key to import, a key is made. When a certificate or a broken key file shows
// that agents may hold a pin, the change of pin is warned about.
func TestANewKeyIsMadeAndWarnedAboutWhenNoLegacyKeyCanBeLoaded(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(t *testing.T, dataDir string)
		wantWarn bool
	}{
		{name: "nothing there", setup: func(*testing.T, string) {}},
		{
			name: "a certificate and no key",
			setup: func(t *testing.T, dataDir string) {
				p := legacyPaths(dataDir)
				if err := os.MkdirAll(p.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				old, err := newKey()
				if err != nil {
					t.Fatal(err)
				}
				writeOneYearCert(t, p.cert, old, fixedNow)
			},
			wantWarn: true,
		},
		{
			name: "a key file that is not a key",
			setup: func(t *testing.T, dataDir string) {
				p := legacyPaths(dataDir)
				if err := os.MkdirAll(p.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.key, []byte("not a key"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantWarn: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := captureDefaultLog(t)
			dataDir := t.TempDir()
			c.setup(t, dataDir)
			m := newMemCertStore()
			if _, err := prepare(t, m, dataDir); err != nil {
				t.Fatal(err)
			}
			if m.inserts != 1 {
				t.Errorf("inserts = %d, want a new key stored", m.inserts)
			}
			warned := strings.Contains(logs.String(), "could not be loaded and was replaced")
			if warned != c.wantWarn {
				t.Errorf("pin-change warning logged = %v, want %v:\n%s", warned, c.wantWarn, logs)
			}
		})
	}
}

// A legacy key file that exists but cannot be read may well be a good key, so startup stops rather
// than minting a replacement that would change the pin.
func TestAnUnreadableLegacyKeyFileStopsStartup(t *testing.T) {
	dataDir := t.TempDir()
	p := legacyPaths(dataDir)
	// A directory where the file should be: reading it fails, and it is not a missing file.
	if err := os.MkdirAll(p.key, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newMemCertStore()
	_, err := prepare(t, m, dataDir)
	if err == nil || !strings.Contains(err.Error(), "was not replaced") {
		t.Fatalf("err = %v, want one saying the legacy key was not replaced", err)
	}
	if m.inserts != 0 {
		t.Errorf("inserts = %d, want no key made", m.inserts)
	}
}
