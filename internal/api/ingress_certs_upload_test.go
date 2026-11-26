// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
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

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
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

// certRows counts the certificate rows of an environment.
func (f *ingressFix) certRows(t *testing.T, env string) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_certs WHERE env_id = ?`, env).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// upload uploads a fresh valid certificate for host as u and requires 201.
func (f *ingressFix) upload(t *testing.T, u credentialUser, ca *uploadCA, env, host string) uploadResp {
	t.Helper()
	chainPEM, keyPEM := ca.issue(t, f.clk.now(), uploadLeaf{sans: []string{host}})
	rec := f.credCall(t, u, http.MethodPost, uploadPath(env), uploadBody(host, chainPEM, keyPEM))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload %s in %s: %d %s", host, env, rec.Code, rec.Body)
	}
	return decodeUpload(t, rec)
}

// D-23: each way an upload can be wrong is a 422 with its own stable code and a fixed sentence that holds no input byte; a
// hostname another environment holds is 409 ingress_host_taken; and no refusal writes a row (each is audited as a failure, A-01).
func TestIngressCertUploadRefusals(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminA, f.envA, "shared.example.com")
	f.register(t, f.adminB, f.envB, "shared.example.com")
	now := f.clk.now()
	ca := newUploadCA(t, now)
	const host = "app.apps.example.com"
	const marker = "SECRET-MARKER-9f2c41"

	chainPEM, keyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})
	_, otherKeyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})
	otherHostChain, otherHostKey := ca.issue(t, now, uploadLeaf{sans: []string{"other.apps.example.com"}})
	expiredChain, expiredKey := ca.issue(t, now, uploadLeaf{sans: []string{host}, notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-time.Hour)})
	futureChain, futureKey := ca.issue(t, now, uploadLeaf{sans: []string{host}, notBefore: now.Add(time.Hour), notAfter: now.Add(48 * time.Hour)})
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edPEM := pkcs8PEM(t, edKey)
	encryptedPEM := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte(marker)}))

	cases := []struct {
		name, code, chain, key string
		secrets                []string
	}{
		{"a key that is not the certificate's", "ingress_cert_key_mismatch", chainPEM, otherKeyPEM, []string{otherKeyPEM}},
		{"a certificate for another name", "ingress_cert_host_not_covered", otherHostChain, otherHostKey, []string{otherHostKey}},
		{"an expired certificate", "ingress_cert_expired", expiredChain, expiredKey, []string{expiredKey}},
		{"a certificate not valid yet", "ingress_cert_not_yet_valid", futureChain, futureKey, []string{futureKey}},
		{"a chain that is not PEM", "ingress_cert_chain_unparsable", marker, keyPEM, []string{marker, keyPEM}},
		{"an Ed25519 key", "ingress_cert_key_unsupported", chainPEM, edPEM, []string{edPEM}},
		{"an encrypted key", "ingress_cert_key_encrypted", chainPEM, encryptedPEM, []string{marker}},
		{"a 300 KiB chain", "ingress_cert_too_large", strings.Repeat("A", 300<<10), keyPEM, []string{keyPEM}},
		{"a 300 KiB key", "ingress_cert_too_large", chainPEM, marker + strings.Repeat("A", 300<<10), []string{marker}},
	}
	for _, tc := range cases {
		rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody(host, tc.chain, tc.key))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.code {
			t.Errorf("%s: %d %s, want 422 %s", tc.name, rec.Code, rec.Body, tc.code)
			continue
		}
		for _, secret := range tc.secrets {
			requireNoKey(t, tc.name+": the problem", rec.Body.String(), secret)
			if strings.Contains(rec.Body.String(), marker) {
				t.Errorf("%s: the problem holds input bytes", tc.name)
			}
		}
	}

	// The name rules are those of a request ahead: another environment's request holds the name, and the refusal names nothing of it.
	f.requestCert(t, f.adminB, f.envB, "web.shared.example.com")
	sharedChain, sharedKey := ca.issue(t, now, uploadLeaf{sans: []string{"web.shared.example.com"}})
	rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody("web.shared.example.com", sharedChain, sharedKey))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_host_taken" {
		t.Errorf("a hostname another environment holds: %d %s, want 409 ingress_host_taken", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), f.envB) || strings.Contains(rec.Body.String(), "ing-admin-b") {
		t.Errorf("the refusal names the other environment: %s", rec.Body)
	}
	rec = f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody("web.elsewhere.org", sharedChain, sharedKey))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_domain_not_allowed" {
		t.Errorf("a hostname under no domain: %d %s, want 422 ingress_domain_not_allowed", rec.Code, rec.Body)
	}

	if n := f.certRows(t, f.envA); n != 0 {
		t.Errorf("%d certificate rows of A, want none written by refusals", n)
	}
	// Every refusal above is one failure row (9 cases, the taken name and the unregistered domain), and none is a success.
	rows := f.ingressAudit(t, audit.ActionIngressCertUploaded)
	if len(rows) != len(cases)+2 {
		t.Errorf("audit rows = %d, want %d failure rows", len(rows), len(cases)+2)
	}
	for _, r := range rows {
		if r.Result != "failure" {
			t.Errorf("audit row = %+v, want a failure", r)
		}
	}
	requireNoKey(t, "the audit log", f.everyAuditText(t), keyPEM)
	requireNoKey(t, "the server log", f.logs.String(), keyPEM)
	requireNoKey(t, "the server log", f.logs.String(), otherKeyPEM)
	if strings.Contains(f.logs.String(), marker) {
		t.Errorf("the server log holds input bytes")
	}
}

// Pitfall 7: a schema violation is answered with the offending value, so the private key field must carry no maxLength; the
// hostname's bound is still there, which shows the check reads the right schema.
func TestIngressCertUploadSchemaHasNoKeyLength(t *testing.T) {
	t.Parallel()
	r := httpx.New(httpx.Options{})
	api.RegisterIngressCerts(r, api.IngressCertDeps{})
	raw, err := json.Marshal(r.API().OpenAPI())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	op, ok := doc.Paths["/api/env/{envId}/ingress/certificates/upload"]["post"]
	if !ok || op.OperationID != "uploadIngressCertificate" {
		t.Fatalf("the upload operation is not in the contract: %+v", op)
	}
	ref := op.RequestBody.Content["application/json"].Schema.Ref
	schema, ok := doc.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
	if !ok {
		t.Fatalf("the request body schema %q is not in the contract", ref)
	}
	key, ok := schema.Properties["privateKeyPem"]
	if !ok {
		t.Fatalf("the request body has no privateKeyPem: %v", schema.Properties)
	}
	for _, k := range []string{"maxLength", "minLength", "pattern", "enum", "format"} {
		if _, has := key[k]; has {
			t.Errorf("privateKeyPem has %s in the schema: a violation would echo the key", k)
		}
	}
	if _, has := schema.Properties["certificatePem"]["maxLength"]; has {
		t.Errorf("certificatePem has a maxLength")
	}
	if host, has := schema.Properties["hostname"]["maxLength"]; !has || host != float64(253) {
		t.Errorf("hostname maxLength = %v, want 253", host)
	}
}

// D-22: switching an uploaded hostname back to ACME queues a certificate for the hostname from the domain's issuer, leaves the
// retired upload listed as retired, and is audited; an ACME certificate, a retired upload and another environment's upload
// are 404.
func TestIngressCertSwitchBackToACME(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminB, f.envB, "other.example.com")
	ca := newUploadCA(t, f.clk.now())
	up := f.upload(t, f.adminA, ca, f.envA, "app.apps.example.com")
	acme := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	bUp := f.upload(t, f.adminB, ca, f.envB, "web.other.example.com")
	switchBack := func(env, id string) *httptest.ResponseRecorder {
		return f.credCall(t, f.adminA, http.MethodPost, certsPath(env, "/", id, "/acme"), "")
	}

	for name, id := range map[string]string{"an ACME certificate": acme.ID, "another environment's upload": bUp.ID, "an unknown id": "no-such-cert"} {
		rec := switchBack(f.envA, id)
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_cert_not_found" {
			t.Errorf("%s: %d %s, want 404 ingress_cert_not_found", name, rec.Code, rec.Body)
		}
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertSwitchedToACME); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none for refused switches", rows)
	}

	rec := switchBack(f.envA, up.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch back: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	fresh := decodeCert(t, rec)
	if fresh.ID == "" || fresh.ID == up.ID || fresh.Hostname != "app.apps.example.com" || fresh.Source != "acme" || fresh.State != "queued" || fresh.Issuer != "production" || fresh.Ready {
		t.Errorf("the new certificate = %+v", fresh)
	}

	byID := map[string]certResp{}
	for _, c := range decodeCerts(t, f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA), "")) {
		byID[c.ID] = c
	}
	if got := byID[up.ID]; got.State != "retired" || got.Source != "upload" {
		t.Errorf("the switched upload = %+v, want listed as retired", got)
	}
	if got := byID[fresh.ID]; got.State != "queued" {
		t.Errorf("the queued certificate = %+v", got)
	}

	rows := f.ingressAudit(t, audit.ActionIngressCertSwitchedToACME)
	if len(rows) != 1 {
		t.Fatalf("ingress.cert_switched_to_acme rows = %+v, want one", rows)
	}
	if r := rows[0]; r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_certificate" || r.TargetID != fresh.ID ||
		r.EnvID != f.envA || r.Details["hostname"] != "app.apps.example.com" || r.Details["issuer"] != "production" {
		t.Errorf("audit row = %+v", r)
	}

	// The upload is retired now: switching it again is a 404, and B's upload is untouched.
	if rec := switchBack(f.envA, up.ID); rec.Code != http.StatusNotFound {
		t.Errorf("a second switch of the same upload: %d %s, want 404", rec.Code, rec.Body)
	}
	var retired int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_certs WHERE id = ? AND retired_at = 0`, bUp.ID).Scan(&retired); err != nil || retired != 1 {
		t.Errorf("B's upload live rows = %d, %v, want it untouched", retired, err)
	}
}

// T-20-37: the upload and the switch back need ingress.manage on the environment: an Operator gets 403, the Admin of another
// environment cannot see it, and a refused caller changes nothing.
func TestIngressCertUploadRoutesNeedTheAdmin(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	ca := newUploadCA(t, f.clk.now())
	up := f.upload(t, f.adminA, ca, f.envA, "app.apps.example.com")
	chainPEM, keyPEM := ca.issue(t, f.clk.now(), uploadLeaf{sans: []string{"api.apps.example.com"}})

	routes := []struct {
		name, method, path, body string
		ok                       int
	}{
		{"upload", http.MethodPost, uploadPath(f.envA), uploadBody("api.apps.example.com", chainPEM, keyPEM), http.StatusCreated},
		{"switch back", http.MethodPost, certsPath(f.envA, "/", up.ID, "/acme"), "", http.StatusOK},
	}
	for _, r := range routes {
		before := f.certRows(t, f.envA)
		for who, u := range map[string]credentialUser{"an Operator of A": f.operatorA, "the Admin of B": f.adminB} {
			rec := f.credCall(t, u, r.method, r.path, r.body)
			if rec.Code != http.StatusForbidden && (who == "an Operator of A" || rec.Code != http.StatusNotFound) {
				t.Errorf("%s as %s: %d %s, want 403 (404 for an environment the caller cannot see)", r.name, who, rec.Code, rec.Body)
			}
			requireNoKey(t, r.name+" as "+who, rec.Body.String(), keyPEM)
		}
		if n := f.certRows(t, f.envA); n != before {
			t.Fatalf("after refused %s calls there are %d certificate rows of A, want %d", r.name, n, before)
		}
		rec := f.credCall(t, f.adminA, r.method, r.path, r.body)
		if rec.Code != r.ok {
			t.Errorf("%s as the Admin of A: %d %s, want %d", r.name, rec.Code, rec.Body, r.ok)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", r.name, cc)
		}
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertUploaded); len(rows) != 2 {
		t.Errorf("upload audit rows = %d, want 2 (the setup and the Admin's call): refused callers write none", len(rows))
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertSwitchedToACME); len(rows) != 1 {
		t.Errorf("switch audit rows = %d, want 1: refused callers write none", len(rows))
	}
}
