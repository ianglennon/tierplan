// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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

// trustAt is a moment inside the validity of the certificates the unit tests build.
var trustAt = time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)

// A leaf sent without the intermediate that signed it does not reach the root, which is how an agent
// judges a cert file that holds only the leaf (D-10).
func TestPublicTrustLeafWithoutItsIntermediateIsUnknownAuthority(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	inter := pkiCA(t, "test intermediate", &root, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &inter, pkiFrom, pkiTo, "tierplan.example.com")

	got := verifyPublicTrust([]*x509.Certificate{leaf.cert}, "tierplan.example.com", pkiPool(root), trustAt)
	want := trustVerdict{Checked: true, Reason: trustUnknownAuthority, Host: "tierplan.example.com"}
	if got != want {
		t.Errorf("verdict = %+v, want %+v", got, want)
	}
	// With the intermediate presented, the same leaf verifies: the chain was the whole difference.
	got = verifyPublicTrust([]*x509.Certificate{leaf.cert, inter.cert}, "tierplan.example.com", pkiPool(root), trustAt)
	if !got.Verified {
		t.Errorf("verdict with the intermediate = %+v, want verified", got)
	}
}

// A certificate that is its own issuer is self_signed, and one signed by a CA the pool does not hold is
// unknown_authority.
func TestPublicTrustSelfSignedLeafAndAnUnknownIssuer(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	self := pkiLeaf(t, nil, pkiFrom, pkiTo, "tierplan.example.com")
	other := pkiCA(t, "another root", nil, pkiFrom, pkiTo)
	signed := pkiLeaf(t, &other, pkiFrom, pkiTo, "tierplan.example.com")

	if got := verifyPublicTrust([]*x509.Certificate{self.cert}, "tierplan.example.com", pkiPool(root), trustAt); got.Reason != trustSelfSigned || got.Verified {
		t.Errorf("a self-signed leaf = %+v, want self_signed", got)
	}
	if got := verifyPublicTrust([]*x509.Certificate{signed.cert}, "tierplan.example.com", pkiPool(root), trustAt); got.Reason != trustUnknownAuthority || got.Verified {
		t.Errorf("a leaf from an unknown CA = %+v, want unknown_authority", got)
	}
}

// Expiry is judged at the injected time: the same leaf verifies a minute before NotAfter and is expired
// after it.
func TestPublicTrustExpiryFollowsTheInjectedTime(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	notAfter := trustAt.Add(24 * time.Hour)
	leaf := pkiLeaf(t, &root, pkiFrom, notAfter, "tierplan.example.com")
	chain := []*x509.Certificate{leaf.cert}

	if got := verifyPublicTrust(chain, "tierplan.example.com", pkiPool(root), notAfter.Add(-time.Minute)); !got.Verified {
		t.Errorf("a minute before NotAfter = %+v, want verified", got)
	}
	if got := verifyPublicTrust(chain, "tierplan.example.com", pkiPool(root), notAfter.Add(time.Second)); got.Reason != trustExpired || got.Verified {
		t.Errorf("after NotAfter = %+v, want expired", got)
	}
}

// A host the leaf does not cover is name_mismatch, with the host in the verdict; with no public URL
// there is no name to check.
func TestPublicTrustNameMismatchAndNoPublicURL(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	leaf := pkiLeaf(t, &root, pkiFrom, pkiTo, "tierplan.example.com")
	chain := []*x509.Certificate{leaf.cert}

	got := verifyPublicTrust(chain, "other.example.com", pkiPool(root), trustAt)
	if want := (trustVerdict{Checked: true, Reason: trustNameMismatch, Host: "other.example.com"}); got != want {
		t.Errorf("a host the leaf does not cover = %+v, want %+v", got, want)
	}
	got = verifyPublicTrust(chain, "", pkiPool(root), trustAt)
	if want := (trustVerdict{Checked: true, Reason: trustNoPublicURL}); got != want {
		t.Errorf("no public URL = %+v, want %+v", got, want)
	}
}

// The same verdict comes back inside one minute of the injected clock, whatever asks, and any change of
// minute, leaf or host verifies again (T-16-10).
func TestPublicTrustCachesPerLeafHostAndMinute(t *testing.T) {
	root := pkiCA(t, "test root", nil, pkiFrom, pkiTo)
	first := pkiLeaf(t, &root, pkiFrom, pkiTo, "a.example.com", "b.example.com")
	second := pkiLeaf(t, &root, pkiFrom, pkiTo, "a.example.com", "b.example.com")
	now, host := trustAt, "a.example.com"
	c := newTrustChecker(pkiPool(root), func() time.Time { return now }, func() string { return host })
	verifications := 0
	c.verify = func(chain []*x509.Certificate, h string, roots *x509.CertPool, at time.Time) trustVerdict {
		verifications++
		return verifyPublicTrust(chain, h, roots, at)
	}
	chain := func(l pkiCert) []*x509.Certificate { return []*x509.Certificate{l.cert} }

	steps := []struct {
		name  string
		move  func()
		leaf  pkiCert
		count int
	}{
		{"the first request verifies", func() {}, first, 1},
		{"the same minute does not", func() { now = now.Add(20 * time.Second) }, first, 1},
		{"a new minute does", func() { now = now.Add(time.Minute) }, first, 2},
		{"a new leaf does", func() {}, second, 3},
		{"a new host does", func() { host = "b.example.com" }, second, 4},
		{"and then it is cached again", func() {}, second, 4},
	}
	for _, s := range steps {
		s.move()
		if v := c.verdictFor(tlsKindSupplied, chain(s.leaf)); !v.Verified {
			t.Fatalf("%s: verdict = %+v, want verified", s.name, v)
		}
		if verifications != s.count {
			t.Errorf("%s: %d verifications, want %d", s.name, verifications, s.count)
		}
	}
}

// The self-signed certificate is never verified (an agent without its pin would refuse it), and with
// nothing served there is nothing to judge.
func TestPublicTrustSelfSignedKindAndNothingServed(t *testing.T) {
	self := pkiLeaf(t, nil, pkiFrom, pkiTo, "tierplan.example.com")
	c := newTrustChecker(nil, func() time.Time { return trustAt }, func() string { return "tierplan.example.com" })
	c.verify = func([]*x509.Certificate, string, *x509.CertPool, time.Time) trustVerdict {
		t.Error("the self-signed kind was verified")
		return trustVerdict{}
	}

	got := c.verdictFor(tlsKindSelfSigned, []*x509.Certificate{self.cert})
	if want := (trustVerdict{Checked: true, Reason: trustSelfSigned, Host: "tierplan.example.com"}); got != want {
		t.Errorf("self-signed kind = %+v, want %+v", got, want)
	}
	if got := c.verdictFor(0, nil); got != (trustVerdict{}) {
		t.Errorf("nothing served = %+v, want the zero verdict", got)
	}
	if got := c.verdictFor(tlsKindSupplied, nil); got != (trustVerdict{}) {
		t.Errorf("a supplied kind with no chain = %+v, want the zero verdict", got)
	}
}

// A cert file that is its own issuer, read through the runtime the way the status reads it, carries the
// self_signed reason, and the leaf and verdict come from the one read.
func TestPublicTrustServedCertFilesThatAreSelfSigned(t *testing.T) {
	self := pkiLeaf(t, nil, pkiFrom, pkiTo, "tierplan.example.com")
	rt := newHTTPSRuntime()
	rt.Install(tlsKindSupplied, func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &tls.Certificate{Certificate: [][]byte{self.cert.Raw}, PrivateKey: self.key}, nil
	})
	c := newTrustChecker(nil, func() time.Time { return trustAt }, func() string { return "tierplan.example.com" })

	s := rt.servedWith(c)
	if s.Source != api.SourceCertFiles || s.Leaf == nil || !s.Leaf.Equal(self.cert) {
		t.Fatalf("served = %+v, want the cert-files leaf", s)
	}
	if want := (api.CertTrust{Checked: true, Reason: trustSelfSigned, Host: "tierplan.example.com"}); s.Trust != want {
		t.Errorf("trust = %+v, want %+v", s.Trust, want)
	}
	if got := rt.served(); got.Trust != (api.CertTrust{}) || got.Leaf == nil {
		t.Errorf("served without a checker = %+v, want the leaf and no verdict", got)
	}
}

// Let's Encrypt is judged like cert files (D-25): a certificate from the test directory is unverified
// unless the test injects its CA, and then it verifies for the public host.
func TestPublicTrustJudgesLetsEncryptLikeCertFiles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject bool
		want   api.HTTPSTrustBody
	}{
		{"without the CA", false, api.HTTPSTrustBody{Checked: true, Reason: trustUnknownAuthority, Host: acmeHost}},
		{"with the CA injected", true, api.HTTPSTrustBody{Checked: true, Verified: true, Host: acmeHost}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newACMERig(t, acmetest.Options{}, nil)
			if tc.inject {
				r.opts = append(r.opts, WithTrustRoots(r.fake.IssuerPool()))
			}
			r.startNew()
			r.issue()
			client := r.client(r.fake.IssuerPool())
			got := r.trustStatus(client, r.signIn(client))

			if got.Certificate.Source != api.SourceLetsEncrypt {
				t.Fatalf("source = %q, want lets_encrypt", got.Certificate.Source)
			}
			if got.Certificate.Trust != tc.want {
				t.Errorf("certificate.trust = %+v, want %+v", got.Certificate.Trust, tc.want)
			}
			if got.CertFilesInfo != nil {
				t.Errorf("certFilesInfo = %+v under Let's Encrypt, want none", got.CertFilesInfo)
			}
		})
	}
}
