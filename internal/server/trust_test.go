// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// pkiCert is a certificate and the key that signed it, for building the chains the trust check judges.
type pkiCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

var pkiSerial int64

// pkiIssue signs a certificate for template under parent, or under itself when parent is nil.
func pkiIssue(t *testing.T, parent *pkiCert, template *x509.Certificate) pkiCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkiSerial++
	template.SerialNumber = big.NewInt(pkiSerial)
	signer, signerCert := key, template
	if parent != nil {
		signer, signerCert = parent.key, parent.cert
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signerCert, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pkiCert{cert: cert, key: key}
}

// pkiCA is a certificate authority valid from notBefore to notAfter, signed by parent (a root when nil).
func pkiCA(t *testing.T, name string, parent *pkiCert, notBefore, notAfter time.Time) pkiCert {
	t.Helper()
	return pkiIssue(t, parent, &x509.Certificate{
		Subject:   pkix.Name{CommonName: name},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true, IsCA: true,
	})
}

// pkiLeaf is a server certificate for hosts, signed by parent, or its own issuer when parent is nil.
func pkiLeaf(t *testing.T, parent *pkiCert, notBefore, notAfter time.Time, hosts ...string) pkiCert {
	t.Helper()
	return pkiIssue(t, parent, &x509.Certificate{
		Subject:   pkix.Name{CommonName: "leaf"},
		DNSNames:  hosts,
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
}

func pkiPool(cas ...pkiCert) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range cas {
		pool.AddCert(c.cert)
	}
	return pool
}

// pkiWritePair writes the certificates as one PEM file, leaf first, and the leaf's key as another.
func pkiWritePair(t *testing.T, leaf pkiCert, chain ...pkiCert) (crt, key string) {
	t.Helper()
	dir := t.TempDir()
	crt, key = filepath.Join(dir, "chain.pem"), filepath.Join(dir, "key.pem")
	var certPEM []byte
	for _, c := range append([]pkiCert{leaf}, chain...) {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})...)
	}
	der, err := x509.MarshalPKCS8PrivateKey(leaf.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crt, certPEM, 0o600); err != nil { //nolint:gosec // a path inside the test's temporary directory
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil { //nolint:gosec // a path inside the test's temporary directory
		t.Fatal(err)
	}
	return crt, key
}

// trustStatus reads GET /api/https/status as the signed-in admin.
func (r *acmeRig) trustStatus(c *http.Client, cookie *http.Cookie) api.HTTPSStatusBody {
	r.t.Helper()
	req, err := http.NewRequestWithContext(testsupport.Context(r.t), http.MethodGet, r.base+"/api/https/status", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := c.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var got api.HTTPSStatusBody
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK {
		r.t.Fatalf("GET /api/https/status: %d (%v)", resp.StatusCode, err)
	}
	return got
}

// Validity wide enough that the rig's clock, which starts at the wall clock, is inside it.
var (
	pkiFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	pkiTo   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// A certificate pair whose chain reaches a root the test injected is verified for the public URL's
// host, in the status's certificate block and in the cert-files block that also shows the two paths
// (D-10, D-16).
func TestStatusTrustsCertFilesSignedByTheInjectedRoot(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	inter := pkiCA(t, "test intermediate", &root, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &inter, pkiFrom, pkiTo, acmeHost)
	crt, key := pkiWritePair(t, leaf, inter)

	r := newACMERig(t, acmetest.Options{}, map[string]string{"TIERPLAN_TLS_CERT_FILE": crt, "TIERPLAN_TLS_KEY_FILE": key},
		WithTrustRoots(pkiPool(root)))
	r.startNew()
	client := r.client(pkiPool(root))
	got := r.trustStatus(client, r.signIn(client))

	if got.CertFilesInfo == nil {
		t.Fatal("certFilesInfo is missing with certificate files")
	}
	if got.CertFilesInfo.CertPath != crt || got.CertFilesInfo.KeyPath != key {
		t.Errorf("paths = %q and %q, want %q and %q", got.CertFilesInfo.CertPath, got.CertFilesInfo.KeyPath, crt, key)
	}
	want := api.HTTPSTrustBody{Checked: true, Verified: true, Host: acmeHost}
	if got.CertFilesInfo.Trust != want {
		t.Errorf("certFilesInfo.trust = %+v, want %+v", got.CertFilesInfo.Trust, want)
	}
	if got.Certificate.Trust != want {
		t.Errorf("certificate.trust = %+v, want %+v", got.Certificate.Trust, want)
	}
	if got.Certificate.Source != api.SourceCertFiles {
		t.Errorf("source = %q, want cert_files", got.Certificate.Source)
	}
}

// The check is the one an agent makes: the root, the presented intermediates, the host, server
// authentication and the injected time. A chain from a root to a leaf passes.
func TestPublicTrustVerifiesAFullChain(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	inter := pkiCA(t, "test intermediate", &root, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &inter, pkiFrom, pkiTo, "tierplan.example.com")
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

	got := verifyPublicTrust([]*x509.Certificate{leaf.cert, inter.cert}, "tierplan.example.com", pkiPool(root), now)
	want := trustVerdict{Checked: true, Verified: true, Host: "tierplan.example.com"}
	if got != want {
		t.Errorf("verdict = %+v, want %+v", got, want)
	}
}
