// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// The per-hostname certificate routes (Phase 20, CRT-03, CRT-05, CRT-06, D-33).

// certsPath is a route of one environment's certificates.
func certsPath(env string, rest ...string) string {
	return "/api/env/" + env + "/ingress/certificates" + strings.Join(rest, "")
}

// certResp is a certificate as the API answers it.
type certResp struct {
	ID                  string `json:"id"`
	Hostname            string `json:"hostname"`
	Issuer              string `json:"issuer"`
	Staging             bool   `json:"staging"`
	Source              string `json:"source"`
	State               string `json:"state"`
	StateText           string `json:"stateText"`
	Ready               bool   `json:"ready"`
	RequestedBy         string `json:"requestedBy"`
	RequestedAt         string `json:"requestedAt"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
}

func decodeCert(t *testing.T, rec *httptest.ResponseRecorder) certResp {
	t.Helper()
	var c certResp
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return c
}

func decodeCerts(t *testing.T, rec *httptest.ResponseRecorder) []certResp {
	t.Helper()
	var l struct {
		Items []certResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return l.Items
}

func certRequestBody(host string) string {
	b, _ := json.Marshal(map[string]string{"hostname": host})
	return string(b)
}

// request asks for a certificate ahead as u and requires 201.
func (f *ingressFix) requestCert(t *testing.T, u credentialUser, env, host string) certResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, certsPath(env), certRequestBody(host))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request %s in %s: %d %s", host, env, rec.Code, rec.Body)
	}
	return decodeCert(t, rec)
}

// CRT-03 tracer: an Admin asks for a certificate before any stack claims the name, sees it queued and listed, and the
// request is audited in the same transaction; nothing returned or recorded is key material.
func TestIngressCertRequestAheadThenList(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA), certRequestBody("web.apps.example.com"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: %d %s, want 201", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	c := decodeCert(t, rec)
	if c.ID == "" || c.Hostname != "web.apps.example.com" || c.State != "queued" || c.StateText != "Queued" || c.Source != "acme" ||
		c.Issuer != "production" || c.Staging || c.Ready || c.RequestedBy != "id-ing-admin-a" || c.RequestedAt == "" {
		t.Errorf("certificate = %+v", c)
	}
	bodies := rec.Body.String()

	rec = f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("list Cache-Control = %q, want no-store", cc)
	}
	items := decodeCerts(t, rec)
	if len(items) != 1 || items[0].ID != c.ID || items[0].State != "queued" || items[0].Hostname != "web.apps.example.com" {
		t.Errorf("list = %+v, want the one requested certificate", items)
	}
	bodies += rec.Body.String()

	rows := f.ingressAudit(t, audit.ActionIngressCertRequested)
	if len(rows) != 1 {
		t.Fatalf("ingress.cert_requested rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_certificate" || r.TargetID != c.ID || r.EnvID != f.envA ||
		r.Details["hostname"] != "web.apps.example.com" || r.Details["issuer"] != "production" {
		t.Errorf("audit row = %+v", r)
	}
	for name, text := range map[string]string{"the answers": bodies, "the audit log": f.everyAuditText(t)} {
		if strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("%s hold key material", name)
		}
	}
}
