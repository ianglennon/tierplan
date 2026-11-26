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
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
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

// Changing a domain's issuer writes one row with the new and previous issuer and the two counts the change was confirmed on.
func TestAuditProofDomainIssuerChanged(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	dom := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPut, issuerPath(f.envA, dom.ID), issuerChangeBody("staging", 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d %s, want 200", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDomainIssuerChanged)
	row := onlyClaimRow(t, rows, audit.ActionIngressDomainIssuerChanged)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != dom.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the domain", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{
		"suffix": "apps.example.com", "issuer": "staging", "previous": "production", "certificates": "1", "uploads": "0",
	})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// Asking for a certificate ahead of any claim writes one row with the hostname and the issuer.
func TestAuditProofCertRequested(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")

	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")

	rows := audittest.Rows(t, f.st, audit.ActionIngressCertRequested)
	row := onlyClaimRow(t, rows, audit.ActionIngressCertRequested)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != c.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the certificate", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"hostname": "web.apps.example.com", "issuer": "production"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// Trying a certificate again now writes one row with the hostname.
func TestAuditProofCertRetried(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA, "/", c.ID, "/retry"), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry: %d %s, want 202", rec.Code, rec.Body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressCertRetried)
	row := onlyClaimRow(t, rows, audit.ActionIngressCertRetried)
	if row.ActorName != "ing-admin-a" || row.Result != string(audit.ResultSuccess) || row.TargetID != c.ID || row.EnvID != f.envA {
		t.Errorf("row = actor %q result %q target %q env %q, want a success by the Admin on the certificate", row.ActorName, row.Result, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"hostname": "web.apps.example.com"})
	audittest.AssertNoSecrets(t, rows, ingressToken)
}

// vrrpKeyProof is a recognisable stand-in for the virtual IP's VRRP key, which the handoff rows must never hold.
const vrrpKeyProof = "9f3c1a7e5b2d4086a1c3e5f70912b4d6" //nolint:gosec // G101: a recognisable fake key, so the scan can look for it

// Draining the node that holds the virtual IP writes one handoff row, a success when the address moved, with the node, the
// holder and the outcome and never the VRRP key.
func TestAuditProofVIPHandoff(t *testing.T) {
	t.Parallel()
	vip := &fakeVIP{holder: "node-4", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffMoved, Holder: "wrk-2", HolderNodeID: "node-5"}}
	f, e := naEnvVIP(t, vip)

	rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
	if rec.code != http.StatusOK {
		t.Fatalf("drain: %d %s, want 200 with the handoff", rec.code, rec.body)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPHandoff)
	row := onlyClaimRow(t, rows, audit.ActionIngressVIPHandoff)
	if row.Result != string(audit.ResultSuccess) || row.Reason != "" || row.TargetType != "node" || row.TargetID != "node-4" || row.EnvID != e.ID {
		t.Errorf("row = result %q reason %q target %s/%s env %q, want a success on node-4", row.Result, row.Reason, row.TargetType, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"node": "wrk-1", "holder": "wrk-2", "outcome": ingressdeploy.VIPHandoffMoved})
	audittest.AssertNoSecrets(t, rows, vrrpKeyProof)
}

// A handoff that does not finish still lets the drain happen, and writes one failure row whose reason is the outcome.
func TestAuditProofVIPHandoffFailure(t *testing.T) {
	t.Parallel()
	vip := &fakeVIP{holder: "node-4", result: ingressdeploy.VIPHandoff{Outcome: ingressdeploy.VIPHandoffTimeout, Holder: "wrk-2"}}
	f, e := naEnvVIP(t, vip)

	rec := f.setAvailability(t, e, "node-4", `{"availability":"drain","version":3}`)
	if rec.code != http.StatusOK {
		t.Fatalf("drain: %d %s, want 200 with the handoff outcome", rec.code, rec.body)
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 1 || ups[0].Spec.Availability != "drain" {
		t.Errorf("updates = %+v, want the drain made whatever the outcome", ups)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPHandoff)
	row := onlyClaimRow(t, rows, audit.ActionIngressVIPHandoff)
	if row.Result != string(audit.ResultFailure) || row.Reason != ingressdeploy.VIPHandoffTimeout || row.TargetID != "node-4" || row.EnvID != e.ID {
		t.Errorf("row = result %q reason %q target %q env %q, want a failure with the outcome as its reason", row.Result, row.Reason, row.TargetID, row.EnvID)
	}
	wantDetails(t, row, map[string]string{"node": "wrk-1", "holder": "wrk-2", "outcome": ingressdeploy.VIPHandoffTimeout})
	audittest.AssertNoSecrets(t, rows, vrrpKeyProof)
}
