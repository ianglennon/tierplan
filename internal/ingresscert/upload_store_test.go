// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingress"
)

// testCA is a certificate authority that signs the leaves of the upload tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

// newTestCA makes a root valid around fixedNow.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"},
		NotBefore: fixedNow.Add(-365 * 24 * time.Hour), NotAfter: fixedNow.Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the root: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// leafOpts shapes a leaf: its names and validity (the zero values mean a valid window around fixedNow).
type leafOpts struct {
	sans                []string
	notBefore, notAfter time.Time
}

// issue signs a leaf for pub with the CA and returns it as a PEM certificate.
func (ca *testCA) issue(t *testing.T, pub crypto.PublicKey, o leafOpts) []byte {
	t.Helper()
	if o.notBefore.IsZero() {
		o.notBefore = fixedNow.Add(-time.Hour)
	}
	if o.notAfter.IsZero() {
		o.notAfter = fixedNow.Add(90 * 24 * time.Hour)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: o.sans[0]}, DNSNames: o.sans,
		NotBefore: o.notBefore, NotAfter: o.notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatalf("sign a leaf: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// ecLeaf makes a P-256 key and a leaf for sans, with the key as PKCS#8 PEM.
func (ca *testCA) ecLeaf(t *testing.T, sans ...string) (chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	keyPEM, err = encodeECKey(key)
	if err != nil {
		t.Fatalf("encode the key: %v", err)
	}
	return ca.issue(t, &key.PublicKey, leafOpts{sans: sans}), keyPEM
}

// uploadWorld is a world in which env-1 holds the registered domain example.com and nothing is claimed.
func uploadWorld(envID, domainID string) ingress.World {
	return ingress.World{
		Domains:  []ingress.Domain{{ID: domainID, EnvID: envID, Suffix: "example.com", Issuer: ingress.IssuerStaging}},
		Reserved: map[string]bool{},
		Claims:   map[string]ingress.ClaimInfo{},
	}
}

// TestTracerAnUploadBecomesTheLiveCertificate is the tracer for CRT-08: a valid chain and key for a hostname
// become its live upload row, the key sealed under the new row's id, certs_version raised and the hook run in
// the same write.
func TestTracerAnUploadBecomesTheLiveCertificate(t *testing.T) {
	s, st := newTestStore(t)
	ctx := context.Background()
	seedEnv(t, st, "env-1", "one")
	ca := newTestCA(t)
	chainPEM, keyPEM := ca.ecLeaf(t, "app.example.com")

	var hookReplaced string
	var hookCert Cert
	got, warnings, err := s.Upload(ctx, UploadInput{
		EnvID: "env-1", Hostname: "App.Example.com.", Actor: "user:1",
		ChainPEM: chainPEM, KeyPEM: keyPEM, World: uploadWorld("env-1", "dom-1"),
	}, ca.pool, func(_ context.Context, _ *sql.Tx, c Cert, replaced string, _ []string) error {
		hookCert, hookReplaced = c, replaced
		return nil
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("a chain that verifies against the roots has no warning; got %v", warnings)
	}
	if got.Source != SourceUpload || got.Issuer != "" || got.Phase != PhaseIdle || !got.Ready || got.RetiredAt != (time.Time{}) {
		t.Fatalf("the upload is a live idle ready upload row with no issuer; got %+v", got)
	}
	if got.Hostname != "app.example.com" || got.EnvID != "env-1" || got.DomainID != "dom-1" || got.CreatedBy != "user:1" {
		t.Fatalf("the row names the normalized hostname, the environment, the domain and the actor; got %+v", got)
	}
	live, found, err := s.LiveCert(ctx, "app.example.com")
	if err != nil || !found || live.ID != got.ID {
		t.Fatalf("LiveCert = %v, found %v, id %q; want the uploaded row %q", err, found, live.ID, got.ID)
	}
	signer, err := s.OpenKey(live)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	wantDER, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatalf("encode the opened key: %v", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || !bytes.Equal(block.Bytes, wantDER) {
		t.Fatalf("the opened key is not the uploaded key")
	}
	if bytes.Contains([]byte(live.KeySealed), block.Bytes) {
		t.Fatalf("the stored key is not sealed")
	}
	if certs, config := versions(t, st, "env-1"); certs != 1 || config != 0 {
		t.Fatalf("an upload raises certs_version once and never config_version; got %d and %d", certs, config)
	}
	if hookReplaced != ReplacedNone || hookCert.ID != got.ID {
		t.Fatalf("the hook saw replaced %q for %q; want none for %q", hookReplaced, hookCert.ID, got.ID)
	}
}
