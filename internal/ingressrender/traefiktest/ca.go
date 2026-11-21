// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package traefiktest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"testing"
	"time"
)

// CA is a throwaway certificate authority for one test. It issues the leaves a Traefik under test
// serves, and its pool is what a probe verifies them against. Its keys live in memory only.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// NewCA makes a CA valid from an hour ago for a week, so a run never depends on a fixed expiry date.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("traefiktest: generate the CA key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "TierPlan Traefik test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("traefiktest: create the CA: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("traefiktest: parse the CA: %v", err)
	}
	return &CA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Leaf issues a server certificate for hosts (the first is its common name, all are DNS names) and
// returns its chain (the leaf alone) and its key, both as PEM.
func (c *CA) Leaf(t testing.TB, hosts ...string) (chainPEM, keyPEM []byte) {
	t.Helper()
	return c.LeafValidFor(t, 24*time.Hour, hosts...)
}

// LeafValidFor is Leaf for a certificate that is valid for the given time from now (and from an hour ago). TierPlan serves
// a certificate only while it has more than a day left, so a test that uploads one through the certificate store asks for
// more than that, and no more than the CA's own week.
func (c *CA) LeafValidFor(t testing.TB, valid time.Duration, hosts ...string) (chainPEM, keyPEM []byte) {
	t.Helper()
	if len(hosts) == 0 {
		t.Fatal("traefiktest: Leaf needs a host name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("traefiktest: generate a leaf key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(valid),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("traefiktest: issue a leaf for %v: %v", hosts, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("traefiktest: marshal a leaf key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// Pool is a pool holding only this CA, to verify a leaf it issued.
func (c *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	return pool
}

// PEM is the CA certificate, as PEM.
func (c *CA) PEM() []byte { return append([]byte(nil), c.pem...) }

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(fmt.Errorf("traefiktest: make a serial number: %w", err))
	}
	return n
}
