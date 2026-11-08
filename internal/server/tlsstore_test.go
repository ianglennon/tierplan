// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// newSealedStore is a sealedCertStore over a migrated temporary database and a vault with a
// generated key.
func newSealedStore(t *testing.T) sealedCertStore {
	t.Helper()
	v, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return sealedCertStore{st: storetest.New(t), v: v}
}

func TestSealedCertStoreRoundTripsASealedKey(t *testing.T) {
	ctx := context.Background()
	s := newSealedStore(t)
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := []byte("-----BEGIN CERTIFICATE-----\nfirst\n-----END CERTIFICATE-----\n")
	t0 := time.Unix(1_700_000_000, 0)

	if err := s.Insert(ctx, "t", certPEM, keyPEM, t0); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	gotCert, gotKey, found, err := s.Load(ctx, "t")
	if err != nil || !found {
		t.Fatalf("Load = found %v, %v; want found", found, err)
	}
	if !bytes.Equal(gotCert, certPEM) || !bytes.Equal(gotKey, keyPEM) {
		t.Errorf("Load returned a different certificate or key than was inserted")
	}

	var raw string
	if err := s.st.Read.QueryRowContext(ctx, `SELECT key_sealed FROM tls_certs WHERE id = 't'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "PRIVATE KEY") || raw == string(keyPEM) {
		t.Errorf("the key_sealed column holds the key in the clear")
	}

	next := []byte("-----BEGIN CERTIFICATE-----\nsecond\n-----END CERTIFICATE-----\n")
	t1 := t0.Add(time.Hour)
	if err := s.UpdateCert(ctx, "t", next, t1); err != nil {
		t.Fatalf("UpdateCert: %v", err)
	}
	gotCert, gotKey, found, err = s.Load(ctx, "t")
	if err != nil || !found || !bytes.Equal(gotCert, next) || !bytes.Equal(gotKey, keyPEM) {
		t.Errorf("after UpdateCert: found %v, err %v, certificate changed %v, key unchanged %v",
			found, err, bytes.Equal(gotCert, next), bytes.Equal(gotKey, keyPEM))
	}
	var created, updated int64
	if err := s.st.Read.QueryRowContext(ctx, `SELECT created_at, updated_at FROM tls_certs WHERE id = 't'`).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if created != t0.UnixNano() || updated != t1.UnixNano() {
		t.Errorf("created_at, updated_at = %d, %d; want %d, %d", created, updated, t0.UnixNano(), t1.UnixNano())
	}

	if _, _, found, err := s.Load(ctx, "missing"); found || err != nil {
		t.Errorf("Load of an unknown id = found %v, %v; want not found and no error", found, err)
	}
}

// memCertStore is a tlsCertStore in memory, for tests that need a store without a database. It
// counts the writes so a test can say how many a code path made.
type memCertStore struct {
	mu      sync.Mutex
	rows    map[string]memCert
	inserts int
	updates int
}

type memCert struct {
	cert, key []byte
	updated   time.Time
}

func newMemCertStore() *memCertStore { return &memCertStore{rows: map[string]memCert{}} }

func (m *memCertStore) Load(_ context.Context, id string) ([]byte, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return nil, nil, false, nil
	}
	return bytes.Clone(r.cert), bytes.Clone(r.key), true, nil
}

func (m *memCertStore) Insert(_ context.Context, id string, certPEM, keyPEM []byte, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[id]; ok {
		return errors.New("duplicate id " + id)
	}
	m.rows[id] = memCert{cert: bytes.Clone(certPEM), key: bytes.Clone(keyPEM), updated: now}
	m.inserts++
	return nil
}

func (m *memCertStore) UpdateCert(_ context.Context, id string, certPEM []byte, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id]
	if !ok {
		return errors.New("no row " + id)
	}
	m.rows[id] = memCert{cert: bytes.Clone(certPEM), key: r.key, updated: now}
	m.updates++
	return nil
}

// TestCertStoresShareOneContract runs the same assertions over the real store and the fake, so the
// fake cannot drift from what production does.
func TestCertStoresShareOneContract(t *testing.T) {
	stores := map[string]func(t *testing.T) tlsCertStore{
		"sealed": func(t *testing.T) tlsCertStore { return newSealedStore(t) },
		"memory": func(*testing.T) tlsCertStore { return newMemCertStore() },
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := mk(t)
			if _, _, found, err := s.Load(ctx, "a"); found || err != nil {
				t.Fatalf("Load on an empty store = found %v, %v; want not found and no error", found, err)
			}
			cert, key := []byte("cert-1"), []byte("-----BEGIN PRIVATE KEY-----\nk\n-----END PRIVATE KEY-----\n")
			now := time.Unix(1_700_000_000, 0)
			if err := s.Insert(ctx, "a", cert, key, now); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if err := s.Insert(ctx, "a", cert, key, now); err == nil {
				t.Error("a second Insert under the same id succeeded")
			}
			gotCert, gotKey, found, err := s.Load(ctx, "a")
			if err != nil || !found || !bytes.Equal(gotCert, cert) || !bytes.Equal(gotKey, key) {
				t.Fatalf("Load = %q, found %v, %v; want the inserted pair", gotCert, found, err)
			}
			if err := s.UpdateCert(ctx, "a", []byte("cert-2"), now.Add(time.Hour)); err != nil {
				t.Fatalf("UpdateCert: %v", err)
			}
			gotCert, gotKey, found, err = s.Load(ctx, "a")
			if err != nil || !found || string(gotCert) != "cert-2" || !bytes.Equal(gotKey, key) {
				t.Errorf("after UpdateCert: certificate %q, key kept %v, found %v, %v", gotCert, bytes.Equal(gotKey, key), found, err)
			}
			if _, _, found, err := s.Load(ctx, "b"); found || err != nil {
				t.Errorf("Load of an unknown id = found %v, %v; want not found and no error", found, err)
			}
		})
	}
}

// A row whose envelope does not open must stop the caller. If it read as "not found" the caller
// would take it for a first start, make a new key and change the pin every agent holds (D-11).
func TestAnUnreadableEnvelopeIsAnErrorNotAFirstStart(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	sealer, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	other, err := vault.LoadOrCreate("", t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := (sealedCertStore{st: st, v: sealer}).Insert(ctx, "t", []byte("cert"), []byte("key"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}

	_, key, found, err := (sealedCertStore{st: st, v: other}).Load(ctx, "t")
	if err == nil {
		t.Fatal("Load with another vault succeeded")
	}
	if found || key != nil {
		t.Errorf("Load reported found=%v, key=%q alongside the error", found, key)
	}
	if !errors.Is(err, vault.ErrUnknownKey) {
		t.Errorf("the error does not wrap vault.ErrUnknownKey: %v", err)
	}

	// An envelope copied into another row opens under the right vault but not under that row's context.
	same := sealedCertStore{st: st, v: sealer}
	if err := same.Insert(ctx, "u", []byte("cert"), []byte("other key"), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write.ExecContext(ctx, `UPDATE tls_certs SET key_sealed = (SELECT key_sealed FROM tls_certs WHERE id = 't') WHERE id = 'u'`); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := same.Load(ctx, "u"); found || !errors.Is(err, vault.ErrDecrypt) {
		t.Errorf("Load of a copied envelope = found %v, %v; want an error wrapping vault.ErrDecrypt", found, err)
	}
}

func TestWriteMirrorWritesOnlyThePublicCertificateAtomically(t *testing.T) {
	dir := t.TempDir()
	first := []byte("-----BEGIN CERTIFICATE-----\nfirst\n-----END CERTIFICATE-----\n")
	if err := writeMirror(dir, first); err != nil {
		t.Fatalf("writeMirror: %v", err)
	}
	tlsDir := filepath.Join(dir, "tls")
	if fi, err := os.Stat(tlsDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("tls/ = %v, %v; want a directory with mode 0700", fi, err)
	}
	file := filepath.Join(tlsDir, mirrorCertFile)
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("%s = %v, %v; want a file with mode 0644", file, fi, err)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, first) { //nolint:gosec // a path under the test's own temporary directory
		t.Errorf("%s = %q; want exactly the certificate", file, got)
	}

	second := []byte("-----BEGIN CERTIFICATE-----\nsecond\n-----END CERTIFICATE-----\n")
	if err := writeMirror(dir, second); err != nil {
		t.Fatalf("second writeMirror: %v", err)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, second) { //nolint:gosec // a path under the test's own temporary directory
		t.Errorf("%s after a second write = %q; want the new certificate", file, got)
	}
	entries, err := os.ReadDir(tlsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != mirrorCertFile {
			t.Errorf("tls/ holds %q besides %s", e.Name(), mirrorCertFile)
		}
		if data, _ := os.ReadFile(filepath.Join(tlsDir, e.Name())); strings.Contains(string(data), "PRIVATE KEY") { //nolint:gosec // a path under the test's own temporary directory
			t.Errorf("%s holds key material", e.Name())
		}
	}
}

// buildSelfSigned builds a server the way production does, with TIERPLAN_TLS_SELF_SIGNED set and a fresh
// data directory, and returns it with the configuration and everything Build logged.
func buildSelfSigned(t *testing.T) (*Server, config.Config, string) {
	t.Helper()
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":        t.TempDir() + "/data",
		"TIERPLAN_TLS_SELF_SIGNED": "true",
	})
	var logs bytes.Buffer
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, cfg, logs.String()
}

func TestASelfSignedInstallServesHTTPSFromItsSealedCertificate(t *testing.T) {
	srv, cfg, logs := buildSelfSigned(t)
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

	// The healthcheck verifies the served certificate against the mirror, from the first start.
	if err := Healthcheck(context.Background(), addr, HealthcheckCertFile(cfg)); err != nil {
		t.Fatalf("Healthcheck over TLS against the mirror: %v", err)
	}
	mirror, err := os.ReadFile(HealthcheckCertFile(cfg)) //nolint:gosec // the mirror under the test's own data directory
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(mirror) {
		t.Fatal("the mirror holds no certificate")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
	t.Cleanup(client.CloseIdleConnections)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz over TLS trusting only the mirror: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	// The certificate is good for ten years.
	leaf := resp.TLS.PeerCertificates[0]
	if want := time.Now().Add(3650 * 24 * time.Hour); leaf.NotAfter.Sub(want).Abs() > 24*time.Hour {
		t.Errorf("NotAfter = %v, want about %v (ten years)", leaf.NotAfter, want)
	}

	// The key is in the database as a vault envelope, and nowhere else.
	var sealed string
	if err := srv.store.Read.QueryRowContext(ctx, `SELECT key_sealed FROM tls_certs WHERE id = ?`, selfSignedID).Scan(&sealed); err != nil {
		t.Fatalf("the %s row: %v", selfSignedID, err)
	}
	if strings.Contains(sealed, "PRIVATE KEY") {
		t.Error("key_sealed holds the key in the clear")
	}
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != mirrorCertFile {
		t.Errorf("tls directory = %v, want exactly %s", entries, mirrorCertFile)
	}

	// Startup says the browser will warn, with the fingerprint to compare, and never logs key material.
	if !strings.Contains(logs, "Your browser will warn about it") || !strings.Contains(logs, fingerprint(srv.https.Config())) {
		t.Errorf("startup log lacks the browser warning and the fingerprint:\n%s", logs)
	}
	if strings.Contains(logs, "PRIVATE KEY") || strings.Contains(logs, sealed) {
		t.Error("the startup log holds key material")
	}
}

func TestTwoSelfSignedInstallsHaveDifferentPins(t *testing.T) {
	a, _, _ := buildSelfSigned(t)
	b, _, _ := buildSelfSigned(t)
	if a.TLSPin() == "" || a.TLSPin() == b.TLSPin() {
		t.Errorf("pins = %q and %q; want two different, non-empty pins", a.TLSPin(), b.TLSPin())
	}
}
