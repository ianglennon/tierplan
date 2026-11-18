// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// seedChallengeRecord puts a DNS ledger row of a hostname certificate, made with the token of domain domainID.
func (f *ingressFix) seedChallengeRecord(t *testing.T, domainID string) {
	t.Helper()
	if _, err := f.st.Write.Exec(
		`INSERT INTO acme_dns_records (id, provider, zone_id, record_id, name, value, owner_kind, owner_id, created_at, credential_ref)
		 VALUES ('ledger-1', 'cloudflare', 'zone-1', 'rec-1', '_acme-challenge.web.apps.example.com', 'v', 'order', 'order-1', 1, ?)`, domainID); err != nil {
		t.Fatalf("seed the ledger row: %v", err)
	}
}

// seedDomainCert puts a live certificate row of a hostname under a domain.
func (f *ingressFix) seedDomainCert(t *testing.T, id, host, domainID string) {
	t.Helper()
	if _, err := f.st.Write.Exec(
		`INSERT INTO ingress_certs (id, hostname, env_id, domain_id, source, created_at, updated_at) VALUES (?, ?, ?, ?, 'acme', 1, 1)`,
		id, host, f.envA, domainID); err != nil {
		t.Fatalf("seed the certificate %s: %v", id, err)
	}
}

// D-27: a delete and a token replacement are refused while a record made with the domain's token is in the ledger; each
// starts the sweep that removes it, the domain and its token stay, and once the row is gone the same calls succeed.
func TestIngressDomainDeleteAndTokenReplaceWaitForRecordCleanup(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.seedChallengeRecord(t, d.ID)

	rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_domain_cleanup_pending" {
		t.Fatalf("delete: %d %s, want 409 ingress_domain_cleanup_pending", rec.Code, rec.Body)
	}
	if got := f.ingressSweeps.Load(); got != 1 {
		t.Errorf("sweeps asked for = %d after the refused delete, want 1", got)
	}
	if views, err := f.ingressStore.Domains(context.Background(), f.envA); err != nil || len(views) != 1 {
		t.Errorf("domains = %+v, %v, want the domain kept", views, err)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainDeleted); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused delete", rows)
	}

	body, _ := json.Marshal(map[string]string{"token": ingressNextToken})
	rec = f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), string(body))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_domain_cleanup_pending" {
		t.Fatalf("token replace: %d %s, want 409 ingress_domain_cleanup_pending", rec.Code, rec.Body)
	}
	if got := f.ingressSweeps.Load(); got != 2 {
		t.Errorf("sweeps asked for = %d after the refused replacement, want 2", got)
	}
	if got, err := f.ingressStore.DomainToken(context.Background(), f.envA, d.ID); err != nil || string(got) != ingressToken {
		t.Errorf("the saved token changed: %q, %v", got, err)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainCredentialReplaced); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused replacement", rows)
	}

	// The sweep has done its work: the row is gone, and both calls go through.
	if _, err := f.st.Write.Exec(`DELETE FROM acme_dns_records`); err != nil {
		t.Fatal(err)
	}
	if rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), string(body)); rec.Code != http.StatusOK {
		t.Fatalf("token replace after the cleanup: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete after the cleanup: %d %s, want 204", rec.Code, rec.Body)
	}
}

// D-28, D-46: the list shows each domain's live certificates; deleting a domain that leaves certificates answers 200
// with certificatesLeft and the audit row records the same count; deleting one that leaves none answers 204.
func TestIngressDomainListAndDeleteReportCertificates(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	empty := f.register(t, f.adminA, f.envA, "empty.example.com")
	f.seedDomainCert(t, "cert-1", "one.apps.example.com", d.ID)
	f.seedDomainCert(t, "cert-2", "two.apps.example.com", d.ID)

	rec := f.credCall(t, f.adminA, http.MethodGet, ingressPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []struct {
			ID           string `json:"id"`
			Certificates *int   `json:"certificates"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	got := map[string]int{}
	for _, it := range list.Items {
		if it.Certificates == nil {
			t.Fatalf("an item has no certificates field: %s", rec.Body)
		}
		got[it.ID] = *it.Certificates
	}
	if got[d.ID] != 2 || got[empty.ID] != 0 {
		t.Errorf("certificates = %v, want 2 for the domain with certificates and 0 for the other", got)
	}

	rec = f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("delete: %d %s, want 200 with no-store", rec.Code, rec.Body)
	}
	var left struct {
		CertificatesLeft *int `json:"certificatesLeft"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &left); err != nil || left.CertificatesLeft == nil || *left.CertificatesLeft != 2 {
		t.Fatalf("delete body = %s (%v), want certificatesLeft 2", rec.Body, err)
	}
	rows := f.ingressAudit(t, audit.ActionIngressDomainDeleted)
	if len(rows) != 1 || rows[0].Details["certificates"] != "2" {
		t.Fatalf("audit rows = %+v, want one ingress.domain_deleted row with certificates 2", rows)
	}

	rec = f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", empty.ID), "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("delete of a domain with no certificates: %d %s, want 204 with no body", rec.Code, rec.Body)
	}
}
