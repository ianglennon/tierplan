// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
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
