// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/config"
)

// GET /api/https/status (phase 14, SET-03, D-08, D-10).

var (
	httpsNotBefore = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	httpsNotAfter  = time.Date(2026, 4, 1, 12, 30, 0, 0, time.UTC)
)

// httpsLeaf signs a leaf with the given issuer name, DNS names and IPs. The issuer is the leaf's own
// name, so it is a self-signed certificate; an empty issuer has no CN and no O.
func httpsLeaf(t *testing.T, issuer pkix.Name, dns []string, ips []net.IP) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: issuer,
		NotBefore: httpsNotBefore, NotAfter: httpsNotAfter,
		DNSNames: dns, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

type httpsCertJSON struct {
	Source    string   `json:"source"`
	Issuer    string   `json:"issuer"`
	Names     []string `json:"names"`
	NotBefore string   `json:"notBefore"`
	NotAfter  string   `json:"notAfter"`
}

type httpsStatusJSON struct {
	Certificate httpsCertJSON `json:"certificate"`
	CertFiles   bool          `json:"certFiles"`
}

// httpsStatus signs in as admin, serves served and reads the status.
func httpsStatus(t *testing.T, deps api.HTTPSDeps) (*httptest.ResponseRecorder, httpsStatusJSON) {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	f.https = deps
	f.h = f.build(t)
	cookie, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status", cookie: cookie})
	var got httpsStatusJSON
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
	}
	return rec, got
}

func TestHTTPSStatusShowsTheServedCertificate(t *testing.T) {
	t.Parallel()
	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed", Organization: []string{"TierPlan"}},
		[]string{"tierplan.example.com", "localhost"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")})
	rec, got := httpsStatus(t, api.HTTPSDeps{Served: func() api.ServedCertificate {
		return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf}
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := got.Certificate
	if c.Source != "self_signed" || c.Issuer != "TierPlan self-signed" {
		t.Errorf("source %q issuer %q", c.Source, c.Issuer)
	}
	if want := []string{"tierplan.example.com", "localhost", "127.0.0.1", "::1"}; !slices.Equal(c.Names, want) {
		t.Errorf("names = %v, want %v", c.Names, want)
	}
	if c.NotAfter != "2026-04-01T12:30:00Z" || c.NotBefore != "2026-01-01T00:00:00Z" {
		t.Errorf("validity %s .. %s", c.NotBefore, c.NotAfter)
	}
	if got.CertFiles {
		t.Error("certFiles is true without certificate files")
	}
}
