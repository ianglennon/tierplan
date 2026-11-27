// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testLeaf is a self-signed ECDSA leaf for name with the given serial, signed by key.
func testLeaf(t *testing.T, key *ecdsa.PrivateKey, serial int64, name string) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{name},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func keyPEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// TestSelfStubReloadServesTheNewPair proves the reload the swap test relies on (A-02): after the reload
// signal the stub serves the pair now in its files to a new handshake and /serial reports its serial,
// and a pair that does not load leaves the one served so far in place.
func TestSelfStubReloadServesTheNewPair(t *testing.T) {
	const name = "tierplan.example.org"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldCert, newCert := testLeaf(t, key, 0xa1, name), testLeaf(t, key, 0xb2, name)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	write := func(f string, b []byte) {
		t.Helper()
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(certFile, oldCert)
	write(keyFile, keyPEM(t, key))

	store := &certStore{certFile: certFile, keyFile: keyFile}
	if err := store.load(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	side := newChanListener(ln.Addr())
	t.Cleanup(func() { _ = ln.Close(); _ = side.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				select {
				case side.ch <- c:
				case <-side.done:
					_ = c.Close()
				}
			}()
		}
	}()
	// Unbuffered: a send returns once serveTLS has taken the signal, and the next send returns only
	// after that reload is done, because the loop takes no signal while it reloads.
	reload := make(chan os.Signal)
	go func() { _ = serveTLS(side, stubConfig{serial: store.serial}, store, reload) }()
	settle := func() {
		reload <- nil
		reload <- nil
	}

	pool := x509.NewCertPool()
	for _, c := range [][]byte{oldCert, newCert} {
		pool.AppendCertsFromPEM(c)
	}
	// get is one request on a new connection, and the serial of the leaf that connection was served.
	get := func(path string) (body, served string) {
		t.Helper()
		tp := &http.Transport{
			TLSClientConfig:   &tls.Config{ServerName: name, RootCAs: pool, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
			ForceAttemptHTTP2: true,
		}
		defer tp.CloseIdleConnections()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://"+ln.Addr().String()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: tp}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), resp.TLS.PeerCertificates[0].SerialNumber.Text(16)
	}

	if body, served := get("/serial"); body != "a1" || served != "a1" {
		t.Fatalf("before the reload /serial said %q on a connection served %q, want a1 for both", body, served)
	}

	write(certFile, newCert)
	settle()
	if body, served := get("/serial"); body != "b2" || served != "b2" {
		t.Fatalf("after the reload /serial said %q on a connection served %q, want b2 for both", body, served)
	}

	write(certFile, []byte("not a certificate"))
	settle()
	if body, served := get("/serial"); body != "b2" || served != "b2" {
		t.Fatalf("after a reload of a broken pair /serial said %q on a connection served %q, want b2 kept", body, served)
	}
}
