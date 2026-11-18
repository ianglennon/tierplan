// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// The issuer change routes (Phase 20, DOM-05, D-25).

// issuerPath is the issuer route of one domain.
func issuerPath(env, domainID string) string { return ingressPath(env, "/", domainID, "/issuer") }

// issuerPreviewResp is a preview as the API answers it.
type issuerPreviewResp struct {
	Issuer       string   `json:"issuer"`
	Certificates int      `json:"certificates"`
	Uploads      []string `json:"uploads"`
	ConfirmCount int      `json:"confirmCount"`
}

func issuerChangeBody(issuer string, count int) string {
	b, _ := json.Marshal(map[string]any{"issuer": issuer, "confirmCount": count})
	return string(b)
}

// domainIssuer reads the issuer of a domain of env through the list route.
func (f *ingressFix) domainIssuer(t *testing.T, u credentialUser, env, domainID string) string {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, ingressPath(env), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list domains: %d %s", rec.Code, rec.Body)
	}
	var l struct {
		Items []ingressDomainResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	for _, d := range l.Items {
		if d.ID == domainID {
			return d.Issuer
		}
	}
	t.Fatalf("domain %s is not in %s", domainID, rec.Body)
	return ""
}

// D-25, DOM-05: the preview counts the certificates an issuer change would redo and lists the uploads it leaves alone; the
// change needs that count back and is refused, with the count now, when it is stale; the same issuer is a no-op without an
// audit row; and a confirmed change is audited with the figures.
func TestIngressIssuerChangePreviewAndConfirm(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	dom := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminB, f.envB, "other.example.com")
	f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	f.requestCert(t, f.adminA, f.envA, "api.apps.example.com")
	ca := newUploadCA(t, f.clk.now())
	f.upload(t, f.adminA, ca, f.envA, "app.apps.example.com")
	if dom.Issuer != "production" {
		t.Fatalf("the domain's issuer = %q, want production", dom.Issuer)
	}

	// The preview: two ACME certificates would be redone, the upload is listed and left alone, and nothing changes.
	rec := f.credCall(t, f.adminA, http.MethodGet, issuerPath(f.envA, dom.ID)+"?issuer=staging", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var pv issuerPreviewResp
	if err := json.Unmarshal(rec.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	if pv.Issuer != "staging" || pv.Certificates != 2 || len(pv.Uploads) != 1 || pv.Uploads[0] != "app.apps.example.com" || pv.ConfirmCount != 2 {
		t.Errorf("preview = %+v, want staging, 2 certificates, the one upload, confirmCount 2", pv)
	}
	if got := f.domainIssuer(t, f.adminA, f.envA, dom.ID); got != "production" {
		t.Errorf("the preview changed the issuer to %q", got)
	}

	// A stale count: 409 with the count now, and the domain is unchanged.
	rec = f.credCall(t, f.adminA, http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("staging", 1))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_issuer_count_mismatch" {
		t.Fatalf("stale count: %d %s, want 409 ingress_issuer_count_mismatch", rec.Code, rec.Body)
	}
	var mm struct {
		Errors []struct {
			Value any `json:"value"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mm); err != nil || len(mm.Errors) != 1 || mm.Errors[0].Value != float64(2) {
		t.Errorf("errors = %+v (%v), want the current count 2 in errors[0].value", mm.Errors, err)
	}
	if got := f.domainIssuer(t, f.adminA, f.envA, dom.ID); got != "production" {
		t.Errorf("a refused change left the issuer %q", got)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainIssuerChanged); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none for a refused change", rows)
	}

	// The confirmed change: 200 with the domain now staging, audited with the figures.
	rec = f.credCall(t, f.adminA, http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("staging", 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed change: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	changed := decodeIngressDomain(t, rec)
	if changed.ID != dom.ID || changed.Issuer != "staging" || changed.Suffix != "apps.example.com" {
		t.Errorf("changed domain = %+v", changed)
	}
	if got := f.domainIssuer(t, f.adminA, f.envA, dom.ID); got != "staging" {
		t.Errorf("the issuer is %q, want staging", got)
	}
	rows := f.ingressAudit(t, audit.ActionIngressDomainIssuerChanged)
	if len(rows) != 1 {
		t.Fatalf("ingress.domain_issuer_changed rows = %+v, want one", rows)
	}
	if r := rows[0]; r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_domain" || r.TargetID != dom.ID || r.EnvID != f.envA ||
		r.Details["suffix"] != "apps.example.com" || r.Details["issuer"] != "staging" || r.Details["previous"] != "production" ||
		r.Details["certificates"] != "2" || r.Details["uploads"] != "1" {
		t.Errorf("audit row = %+v", r)
	}

	// The same issuer again: 200, nothing written, whatever the count says.
	rec = f.credCall(t, f.adminA, http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("staging", 99))
	if rec.Code != http.StatusOK || decodeIngressDomain(t, rec).Issuer != "staging" {
		t.Errorf("the same issuer: %d %s, want 200 staging", rec.Code, rec.Body)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainIssuerChanged); len(rows) != 1 {
		t.Errorf("audit rows = %d after a no-op, want still 1", len(rows))
	}

	// An issuer the schema does not allow is 422 on both routes.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"the preview": f.credCall(t, f.adminA, http.MethodGet, issuerPath(f.envA, dom.ID)+"?issuer=bogus", ""),
		"the change":  f.credCall(t, f.adminA, http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("bogus", 0)),
	} {
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s with an unknown issuer: %d %s, want 422", name, rec.Code, rec.Body)
		}
	}

	// A domain of another environment is a 404, exactly like one that does not exist, and nothing of it changes.
	for name, id := range map[string]string{"another environment's domain": dom.ID, "an unknown id": "no-such-domain"} {
		rec := f.credCall(t, f.adminB, http.MethodGet, issuerPath(f.envB, id)+"?issuer=production", "")
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_domain_not_found" {
			t.Errorf("preview of %s: %d %s, want 404 ingress_domain_not_found", name, rec.Code, rec.Body)
		}
		rec = f.credCall(t, f.adminB, http.MethodPut, issuerPath(f.envB, id), issuerChangeBody("production", 0))
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_domain_not_found" {
			t.Errorf("change of %s: %d %s, want 404 ingress_domain_not_found", name, rec.Code, rec.Body)
		}
	}
	if got := f.domainIssuer(t, f.adminA, f.envA, dom.ID); got != "staging" {
		t.Errorf("B's calls changed A's domain to %q", got)
	}
}

// T-20-39: the preview and the change need ingress.manage on the environment: an Operator gets 403, the Admin of another
// environment cannot see it, and a refused caller changes nothing.
func TestIngressIssuerChangeNeedsTheAdmin(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	dom := f.register(t, f.adminA, f.envA, "apps.example.com")

	routes := []struct {
		name, method, path, body string
		ok                       int
	}{
		{"preview", http.MethodGet, issuerPath(f.envA, dom.ID) + "?issuer=staging", "", http.StatusOK},
		{"change", http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("staging", 0), http.StatusOK},
	}
	for _, r := range routes {
		for who, u := range map[string]credentialUser{"an Operator of A": f.operatorA, "the Admin of B": f.adminB} {
			rec := f.credCall(t, u, r.method, r.path, r.body)
			if rec.Code != http.StatusForbidden && (who == "an Operator of A" || rec.Code != http.StatusNotFound) {
				t.Errorf("%s as %s: %d %s, want 403 (404 for an environment the caller cannot see)", r.name, who, rec.Code, rec.Body)
			}
		}
		if got := f.domainIssuer(t, f.adminA, f.envA, dom.ID); got != "production" && r.name == "change" {
			t.Errorf("refused %s calls changed the issuer to %q", r.name, got)
		}
		if rows := f.ingressAudit(t, audit.ActionIngressDomainIssuerChanged); len(rows) != 0 {
			t.Errorf("after refused %s calls there are %d audit rows, want none", r.name, len(rows))
		}
		rec := f.credCall(t, f.adminA, r.method, r.path, r.body)
		if rec.Code != r.ok {
			t.Errorf("%s as the Admin of A: %d %s, want %d", r.name, rec.Code, rec.Body, r.ok)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", r.name, cc)
		}
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainIssuerChanged); len(rows) != 1 {
		t.Errorf("audit rows = %d, want the one change of the Admin of A", len(rows))
	}
}
