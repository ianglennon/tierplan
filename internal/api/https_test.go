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
	"strings"
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
	Source    string             `json:"source"`
	Issuer    string             `json:"issuer"`
	Names     []string           `json:"names"`
	NotBefore string             `json:"notBefore"`
	NotAfter  string             `json:"notAfter"`
	Trust     api.HTTPSTrustBody `json:"trust"`
}

type httpsStatusJSON struct {
	Certificate   httpsCertJSON           `json:"certificate"`
	CertFiles     bool                    `json:"certFiles"`
	CertFilesInfo *api.HTTPSCertFilesBody `json:"certFilesInfo"`
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
	if got.CertFilesInfo != nil {
		t.Errorf("certFilesInfo = %+v without certificate files, want none", got.CertFilesInfo)
	}
	if strings.Contains(rec.Body.String(), "certFilesInfo") {
		t.Errorf("the body names certFilesInfo without certificate files: %s", rec.Body)
	}
}

// The self-signed certificate is shown with its verdict and no cert-files block, even when the deps
// carry paths.
func TestHTTPSStatusSelfSignedHasNoCertFilesInfo(t *testing.T) {
	t.Parallel()
	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"localhost"}, nil)
	rec, got := httpsStatus(t, api.HTTPSDeps{
		CertFile: "/ignored.crt", KeyFile: "/ignored.key",
		Served: func() api.ServedCertificate {
			return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf, Trust: api.CertTrust{Checked: true, Reason: "self_signed"}}
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.CertFilesInfo != nil {
		t.Errorf("certFilesInfo = %+v for the self-signed certificate, want none", got.CertFilesInfo)
	}
	if got.Certificate.Trust.Verified || got.Certificate.Trust.Reason != "self_signed" {
		t.Errorf("trust = %+v, want self_signed and not verified", got.Certificate.Trust)
	}
}

func TestHTTPSStatusForCertificateFiles(t *testing.T) {
	t.Parallel()
	leaf := httpsLeaf(t, pkix.Name{CommonName: "Example CA"}, []string{"xn--bcher-kva.example", "b.example.com"}, nil)
	trust := api.CertTrust{Checked: true, Reason: "unknown_authority", Host: "b.example.com"}
	rec, got := httpsStatus(t, api.HTTPSDeps{
		CertFiles: true, CertFile: "/certs/tls.crt", KeyFile: "/certs/tls.key",
		Served: func() api.ServedCertificate {
			return api.ServedCertificate{Source: api.SourceCertFiles, Leaf: leaf, Trust: trust}
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.Certificate.Source != "cert_files" || !got.CertFiles {
		t.Errorf("source %q certFiles %v, want cert_files and true", got.Certificate.Source, got.CertFiles)
	}
	wantTrust := api.HTTPSTrustBody{Checked: true, Reason: "unknown_authority", Host: "b.example.com"}
	if got.Certificate.Trust != wantTrust {
		t.Errorf("certificate.trust = %+v, want %+v", got.Certificate.Trust, wantTrust)
	}
	if info := got.CertFilesInfo; info == nil || info.CertPath != "/certs/tls.crt" || info.KeyPath != "/certs/tls.key" || info.Trust != wantTrust {
		t.Errorf("certFilesInfo = %+v, want both paths and the verdict", info)
	}
	// Names are returned exactly as the certificate carries them: A-labels stay A-labels.
	if want := []string{"xn--bcher-kva.example", "b.example.com"}; !slices.Equal(got.Certificate.Names, want) {
		t.Errorf("names = %v, want %v", got.Certificate.Names, want)
	}
}

func TestHTTPSStatusIssuerUnknownAndIPOnlyNames(t *testing.T) {
	t.Parallel()
	leaf := httpsLeaf(t, pkix.Name{}, nil, []net.IP{net.ParseIP("192.0.2.7"), net.ParseIP("2001:db8::1")})
	rec, got := httpsStatus(t, api.HTTPSDeps{Served: func() api.ServedCertificate {
		return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf}
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got.Certificate.Issuer != "" {
		t.Errorf("issuer = %q, want empty", got.Certificate.Issuer)
	}
	if want := []string{"192.0.2.7", "2001:db8::1"}; !slices.Equal(got.Certificate.Names, want) {
		t.Errorf("names = %v, want %v", got.Certificate.Names, want)
	}
	if !strings.Contains(rec.Body.String(), `"issuer":""`) {
		t.Errorf("issuer is not in the body as an empty string: %s", rec.Body)
	}

	// The organisation names the issuer when there is no common name.
	org := httpsLeaf(t, pkix.Name{Organization: []string{"Example Trust"}}, []string{"a.example.com"}, nil)
	_, got = httpsStatus(t, api.HTTPSDeps{Served: func() api.ServedCertificate {
		return api.ServedCertificate{Source: api.SourceCertFiles, Leaf: org}
	}})
	if got.Certificate.Issuer != "Example Trust" {
		t.Errorf("issuer = %q, want the organisation", got.Certificate.Issuer)
	}
}

func TestHTTPSStatusNeedsSettingsRead(t *testing.T) {
	t.Parallel()
	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"localhost"}, nil)
	f := newAuthFixture(t, config.Config{})
	f.https = api.HTTPSDeps{Served: func() api.ServedCertificate {
		return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf}
	}}
	f.h = f.build(t)
	f.addUser(t, "viewer", false) // no role at all

	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET: %d, want 401", rec.Code)
	}

	viewer, _ := f.signIn(t, "viewer", false)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status", cookie: viewer}); rec.Code != http.StatusForbidden {
		t.Errorf("GET without settings.read: %d, want 403", rec.Code)
	}

	admin, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status", cookie: admin})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET: %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Error("the status body holds key material")
	}
}

func TestHTTPSStatusWithoutACertificateIsUnavailable(t *testing.T) {
	t.Parallel()
	rec, _ := httpsStatus(t, api.HTTPSDeps{Served: func() api.ServedCertificate { return api.ServedCertificate{} }})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "https_unavailable") {
		t.Errorf("status = %d: %s, want 503 https_unavailable", rec.Code, rec.Body)
	}
}
