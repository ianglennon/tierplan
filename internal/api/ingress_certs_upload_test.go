// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
)

// The upload and switch-back routes (Phase 20, CRT-08, D-22, D-23).

// uploadPath is the upload route of one environment.
func uploadPath(env string) string { return certsPath(env, "/upload") }

// uploadCA signs the leaves of the upload tests. Its validity is counted from the fixture clock.
type uploadCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newUploadCA(t *testing.T, now time.Time) *uploadCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Upload Test Root"},
		NotBefore: now.Add(-365 * 24 * time.Hour), NotAfter: now.Add(10 * 365 * 24 * time.Hour),
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
	return &uploadCA{cert: cert, key: key}
}

// uploadLeaf shapes a leaf: its names and validity relative to now, and the key type.
type uploadLeaf struct {
	sans                []string
	notBefore, notAfter time.Time
}

// issue makes a P-256 key and signs a leaf for it. It returns the chain and the key as PKCS#8 PEM.
func (ca *uploadCA) issue(t *testing.T, now time.Time, o uploadLeaf) (chainPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	if o.notBefore.IsZero() {
		o.notBefore = now.Add(-time.Hour)
	}
	if o.notAfter.IsZero() {
		o.notAfter = now.Add(90 * 24 * time.Hour)
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign a leaf: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), pkcs8PEM(t, key)
}

func pkcs8PEM(t *testing.T, key crypto.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal a key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func uploadBody(host, chainPEM, keyPEM string) string {
	b, _ := json.Marshal(map[string]string{"hostname": host, "certificatePem": chainPEM, "privateKeyPem": keyPEM})
	return string(b)
}

// uploadResp is an upload's answer.
type uploadResp struct {
	certResp
	Warnings []string `json:"warnings"`
}

func decodeUpload(t *testing.T, rec *httptest.ResponseRecorder) uploadResp {
	t.Helper()
	var u uploadResp
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return u
}

// keyLines are the base64 lines of a PEM key: none of them may appear in anything TierPlan says or writes.
func keyLines(keyPEM string) []string {
	var out []string
	for _, l := range strings.Split(keyPEM, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "-----") {
			out = append(out, l)
		}
	}
	return out
}

// requireNoKey fails for every place text holds the key: its header, or any of its base64 lines.
func requireNoKey(t *testing.T, where, text, keyPEM string) {
	t.Helper()
	if strings.Contains(text, "PRIVATE KEY") {
		t.Errorf("%s hold a private key header", where)
	}
	for _, l := range keyLines(keyPEM) {
		if strings.Contains(text, l) {
			t.Errorf("%s hold a line of the private key", where)
			return
		}
	}
}

// CRT-08 tracer: an Admin uploads a certificate and key for a hostname and gets 201 with the certificate (source upload,
// state uploaded, warning chain_untrusted because the test CA is not in the system roots); the list shows it; the upload
// is audited with its hostname, what it replaced and the warning; and the key is in no answer, audit row or log line.
func TestIngressCertUploadThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	ca := newUploadCA(t, f.clk.now())
	chainPEM, keyPEM := ca.issue(t, f.clk.now(), uploadLeaf{sans: []string{"app.apps.example.com"}})

	rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody("App.apps.example.com.", chainPEM, keyPEM))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s, want 201", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	u := decodeUpload(t, rec)
	if u.ID == "" || u.Hostname != "app.apps.example.com" || u.Source != "upload" || u.State != "uploaded" || !u.Ready || u.Issuer != "" || u.Staging {
		t.Errorf("certificate = %+v", u)
	}
	if len(u.Warnings) != 1 || u.Warnings[0] != "chain_untrusted" {
		t.Errorf("warnings = %v, want [chain_untrusted]", u.Warnings)
	}
	bodies := rec.Body.String()

	rec = f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	items := decodeCerts(t, rec)
	if len(items) != 1 || items[0].ID != u.ID || items[0].Source != "upload" || items[0].State != "uploaded" {
		t.Errorf("list = %+v, want the uploaded certificate", items)
	}
	bodies += rec.Body.String()

	rows := f.ingressAudit(t, audit.ActionIngressCertUploaded)
	if len(rows) != 1 {
		t.Fatalf("ingress.cert_uploaded rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_certificate" || r.TargetID != u.ID || r.EnvID != f.envA ||
		r.Details["hostname"] != "app.apps.example.com" || r.Details["replaced"] != "none" || r.Details["warning"] != "chain_untrusted" {
		t.Errorf("audit row = %+v", r)
	}

	requireNoKey(t, "the answers", bodies, keyPEM)
	requireNoKey(t, "the audit log", f.everyAuditText(t), keyPEM)
	requireNoKey(t, "the server log", f.logs.String(), keyPEM)
}
