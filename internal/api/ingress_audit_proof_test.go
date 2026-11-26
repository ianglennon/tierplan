// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
)

// The domain, reservation, issuer, request, retry and handoff proofs of the audit table test (A-01 class P, D-23): each
// drives the real route, finds the one row of its action with its documented detail keys, and shows no row holds a secret.

// A-01 class P tracer: registering a domain writes its one success row, with the suffix, issuer, provider and only the word
// "set" for the credential; the DNS token is in no row.
func TestAuditProofDomainCreated(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainCreated)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainCreated)
	if row.ActorType != "user" || row.ActorName != "ing-admin-a" {
		t.Errorf("actor = %s/%s, want the Admin who registered", row.ActorType, row.ActorName)
	}
	if row.Result != string(audit.ResultSuccess) || row.Reason != "" || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = result %q reason %q target %q env %q, want a success on the new domain", row.Result, row.Reason, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"suffix": "apps.example.com", "issuer": "production", "provider": "cloudflare", "credential": "set"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// A token the DNS provider refuses stores nothing and writes one failure row whose reason is the DNS class.
func TestAuditProofDomainCreatedFailure(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", ingressWrongTok))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != dns.ClassTokenInvalid {
		t.Fatalf("register: %d %s, want 422 %s", rec.Code, rec.Body, dns.ClassTokenInvalid)
	}
	var stored int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_domains`).Scan(&stored); err != nil || stored != 0 {
		t.Errorf("domains stored = %d, %v, want none after a refusal", stored, err)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainCreated)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainCreated)
	if row.Result != string(audit.ResultFailure) || row.Reason != dns.ClassTokenInvalid || row.TargetID != "" || row.EnvID != f.envA {
		t.Errorf("row = result %q reason %q target %q env %q, want a failure with the DNS class as its reason", row.Result, row.Reason, row.TargetID, row.EnvID)
	}
	if row.Details["suffix"] != "apps.example.com" {
		t.Errorf("details = %v, want the suffix that was tried", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, ingressWrongTok)
}

// Replacing the token writes one success row that says only "replaced"; neither the old nor the new token is in it.
func TestAuditProofDomainCredentialReplaced(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressNextToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s, want 200", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainCredentialReplaced)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainCredentialReplaced)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the domain", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"suffix": "apps.example.com", "credential": "replaced"})
	audittest.AssertNoSecrets(t, rows, ingressToken, ingressNextToken)
}

// A replacement the provider refuses keeps the old token and writes one failure row with the DNS class as its reason.
func TestAuditProofDomainCredentialReplacedFailure(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressWrongTok+`"}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != dns.ClassTokenInvalid {
		t.Fatalf("replace: %d %s, want 422 %s", rec.Code, rec.Body, dns.ClassTokenInvalid)
	}
	tok, err := f.ingressStore.DomainToken(context.Background(), f.envA, d.ID)
	if err != nil || string(tok) != ingressToken {
		t.Errorf("the saved token changed after a refusal: %v", err)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainCredentialReplaced)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainCredentialReplaced)
	if row.Result != string(audit.ResultFailure) || row.Reason != dns.ClassTokenInvalid || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = result %q reason %q target %q env %q, want a failure with the DNS class on the domain", row.Result, row.Reason, row.TargetID, row.EnvID)
	}
	if row.Details["suffix"] != "apps.example.com" {
		t.Errorf("details = %v, want the suffix", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, ingressToken, ingressNextToken, ingressWrongTok)
}

// Deleting a domain writes one row with its suffix, issuer and the count of certificates left to expire.
func TestAuditProofDomainDeleted(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainDeleted)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainDeleted)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the domain", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"suffix": "apps.example.com", "issuer": "production", "certificates": "0"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// Reserving a hostname writes one row with the suffix and the normalised hostname.
func TestAuditProofHostReserved(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA, "/", d.ID, "/reserved"), `{"hostname":"Admin.apps.example.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s, want 201", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressHostReserved)
	row := onlyClaimRow(t, rows, audit.ActionIngressHostReserved)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the domain", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"suffix": "apps.example.com", "hostname": "admin.apps.example.com"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// Releasing a reserved hostname writes one row with the suffix and the hostname.
func TestAuditProofHostUnreserved(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	if rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA, "/", d.ID, "/reserved"), `{"hostname":"admin.apps.example.com"}`); rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}

	rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID, "/reserved/admin.apps.example.com"), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s, want 204", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressHostUnreserved)
	row := onlyClaimRow(t, rows, audit.ActionIngressHostUnreserved)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != d.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the domain", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"suffix": "apps.example.com", "hostname": "admin.apps.example.com"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}
