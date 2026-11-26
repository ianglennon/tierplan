// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
)

// The upload proofs of the audit table test (A-01 class F, SC 5): an upload the server refuses is audited as a failure that
// names the hostname and the refusal code, and no row of an upload or a switch back holds the chain or the key.

// A-01 class F tracer: a key of another pair is refused, nothing is stored, and one failure row says which hostname and why.
func TestAuditProofCertUploadedFailure(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	now := f.clk.now()
	ca := newUploadCA(t, now)
	const host = "up.apps.example.com"
	chainPEM, keyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})
	_, otherKeyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})

	rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody(host, chainPEM, otherKeyPEM))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_cert_key_mismatch" {
		t.Fatalf("upload: %d %s, want 422 ingress_cert_key_mismatch", rec.Code, rec.Body)
	}
	if n := f.certRows(t, f.envA); n != 0 {
		t.Errorf("%d certificate rows, want none stored by a refusal", n)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)
	row := onlyClaimRow(t, rows, audit.ActionIngressCertUploaded)
	if row.ActorType != "user" || row.ActorName != "ing-admin-a" {
		t.Errorf("actor = %s/%s, want the uploader", row.ActorType, row.ActorName)
	}
	if row.Result != string(audit.ResultFailure) || row.Reason != "ingress_cert_key_mismatch" || row.EnvID != f.envA {
		t.Errorf("result = %q reason %q env %q, want a failure with the mismatch code in the environment", row.Result, row.Reason, row.EnvID)
	}
	if row.TargetType != "ingress_certificate" || row.TargetID != host {
		t.Errorf("target = %s/%s, want the hostname", row.TargetType, row.TargetID)
	}
	wantDetails(t, row, map[string]string{"hostname": host, "code": "ingress_cert_key_mismatch"})
	audittest.AssertNoSecrets(t, rows, keyPEM, otherKeyPEM, chainPEM)
}

// A valid upload writes its one success row, written in the store's transaction, and no failure row; the row holds no PEM.
func TestAuditProofCertUploaded(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	now := f.clk.now()
	ca := newUploadCA(t, now)
	const host = "up.apps.example.com"
	chainPEM, keyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})

	rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody(host, chainPEM, keyPEM))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s, want 201", rec.Code, rec.Body)
	}
	up := decodeUpload(t, rec)

	rows := audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)
	row := onlyClaimRow(t, rows, audit.ActionIngressCertUploaded)
	if row.Result != string(audit.ResultSuccess) || row.Reason != "" || row.TargetID != up.ID || row.EnvID != f.envA {
		t.Errorf("row = result %q reason %q target %q env %q, want a success on the new certificate", row.Result, row.Reason, row.TargetID, row.EnvID)
	}
	if row.Details["hostname"] != host {
		t.Errorf("details = %v, want the hostname %s", row.Details, host)
	}
	audittest.AssertNoSecrets(t, rows, keyPEM, chainPEM)
}

// Switching an uploaded hostname back to Let's Encrypt writes its one success row, with no PEM in it.
func TestAuditProofCertSwitchedToACME(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	now := f.clk.now()
	ca := newUploadCA(t, now)
	const host = "up.apps.example.com"
	chainPEM, keyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})
	rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody(host, chainPEM, keyPEM))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s, want 201", rec.Code, rec.Body)
	}
	up := decodeUpload(t, rec)

	rec = f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA, "/", up.ID, "/acme"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("switch back: %d %s, want 200", rec.Code, rec.Body)
	}
	fresh := decodeCert(t, rec)

	rows := audittest.Rows(t, f.st, audit.ActionIngressCertSwitchedToACME)
	row := onlyClaimRow(t, rows, audit.ActionIngressCertSwitchedToACME)
	if row.Result != string(audit.ResultSuccess) || row.TargetID != fresh.ID || row.EnvID != f.envA {
		t.Errorf("row = result %q target %q env %q, want a success on the new certificate", row.Result, row.TargetID, row.EnvID)
	}
	if row.Details["hostname"] != host {
		t.Errorf("details = %v, want the hostname %s", row.Details, host)
	}
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)); n != 1 {
		t.Errorf("ingress.cert_uploaded rows = %d, want only the upload's success", n)
	}
	audittest.AssertNoSecrets(t, rows, keyPEM, chainPEM)
}

// Every refusal after authorisation is a failure row with its own code; a refusal before the handler (a permission refusal)
// writes none; and no row holds the chain or the key.
func TestUploadRefusalsAreAudited(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	now := f.clk.now()
	ca := newUploadCA(t, now)
	const host = "up.apps.example.com"
	chainPEM, keyPEM := ca.issue(t, now, uploadLeaf{sans: []string{host}})
	otherChain, otherKey := ca.issue(t, now, uploadLeaf{sans: []string{"other.apps.example.com"}})

	// A call by an Operator is a 403 before the handler runs: it writes no ingress.cert_uploaded row.
	rec := f.credCall(t, f.operatorA, http.MethodPost, uploadPath(f.envA), uploadBody(host, chainPEM, keyPEM))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an Operator's upload: %d %s, want 403", rec.Code, rec.Body)
	}
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)); n != 0 {
		t.Fatalf("ingress.cert_uploaded rows = %d after a permission refusal, want none", n)
	}

	cases := []struct {
		name, code, host, chain, key string
	}{
		{"a chain that does not cover the hostname", "ingress_cert_host_not_covered", host, otherChain, otherKey},
		{"a too-large body", "ingress_cert_too_large", host, strings.Repeat("A", 300<<10), keyPEM},
		{"a hostname under no registered domain", "ingress_domain_not_allowed", "web.elsewhere.org", chainPEM, keyPEM},
	}
	for i, tc := range cases {
		rec := f.credCall(t, f.adminA, http.MethodPost, uploadPath(f.envA), uploadBody(tc.host, tc.chain, tc.key))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.code {
			t.Fatalf("%s: %d %s, want 422 %s", tc.name, rec.Code, rec.Body, tc.code)
		}
		rows := audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)
		if len(rows) != i+1 {
			t.Fatalf("%s: %d rows, want %d", tc.name, len(rows), i+1)
		}
		// Rows read newest first.
		row := rows[0]
		if row.Result != string(audit.ResultFailure) || row.Reason != tc.code || row.TargetID != tc.host {
			t.Errorf("%s: row = result %q reason %q target %q, want a failure %s on %s", tc.name, row.Result, row.Reason, row.TargetID, tc.code, tc.host)
		}
		wantDetails(t, row, map[string]string{"hostname": tc.host, "code": tc.code})
	}
	if n := f.certRows(t, f.envA); n != 0 {
		t.Errorf("%d certificate rows, want none stored by refusals", n)
	}
	rows := audittest.Rows(t, f.st, audit.ActionIngressCertUploaded)
	audittest.AssertNoSecrets(t, rows, keyPEM, chainPEM, otherKey, otherChain)
}
