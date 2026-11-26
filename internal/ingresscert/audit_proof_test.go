// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// The certificate proofs of the audit table test (A-01 class P, D-23): each drives the real scheduler or order store to
// the action, finds the one row of it written by system:ingress with its documented detail keys, and shows no row holds
// the certificate's private key or the DNS token.

// keySecrets returns what the row's private key must never leave in an audit row: its PKCS#8 PEM and the base64 body of
// that PEM.
func keySecrets(t *testing.T, g *mgrRig, row Cert) []string {
	t.Helper()
	key, err := g.s.OpenKey(row)
	if err != nil {
		t.Fatalf("OpenKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	body := strings.Join(strings.Split(strings.TrimSpace(string(keyPEM)), "\n")[1:2], "")
	return []string{string(keyPEM), body, rigToken}
}

// proofRow returns the only row of rows and fails when there is not exactly one.
func proofRow(t *testing.T, rows []audit.Row, action string) audit.Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1", len(rows), action)
	}
	return rows[0]
}

// proofSystemRow fails unless row was written by system:ingress as a result on a certificate target in env.
func proofSystemRow(t *testing.T, row audit.Row, result audit.Result, certID, env string) {
	t.Helper()
	if row.ActorType != string(audit.ActorSystem) || row.ActorName != audit.SystemIngress.Name {
		t.Errorf("actor = %s/%s, want %s", row.ActorType, row.ActorName, audit.SystemIngress.Name)
	}
	if row.Result != string(result) || row.TargetType != auditTargetCertificate || row.TargetID != certID || row.EnvID != env {
		t.Errorf("row = result %q target %s/%q env %q, want %s on %s/%q in %q",
			row.Result, row.TargetType, row.TargetID, row.EnvID, result, auditTargetCertificate, certID, env)
	}
}

// proofDetails fails unless row's details are exactly want.
func proofDetails(t *testing.T, row audit.Row, want map[string]string) {
	t.Helper()
	if len(row.Details) != len(want) {
		t.Errorf("details = %v, want exactly %v", row.Details, want)
		return
	}
	for k, v := range want {
		if row.Details[k] != v {
			t.Errorf("details[%s] = %q, want %q (all: %v)", k, row.Details[k], v, row.Details)
		}
	}
}

// A-01 class P tracer: a certificate the scheduler issues writes one success row with the hostname and issuer.
func TestAuditProofCertIssued(t *testing.T) {
	g := newRenewRig(t, true)
	_, row := g.issueOne("app.example.com")

	rows := audittest.Rows(t, g.st, audit.ActionIngressCertIssued)
	got := proofRow(t, rows, audit.ActionIngressCertIssued)
	proofSystemRow(t, got, audit.ResultSuccess, row.ID, "env-1")
	proofDetails(t, got, map[string]string{"hostname": "app.example.com", "issuer": IssuerProduction})
	audittest.AssertNoSecrets(t, rows, keySecrets(t, g, row)...)
}

// A DNS provider that refuses the token fails the run: one failure row names the hostname, issuer and error class.
func TestAuditProofCertFailed(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	g.claim("app.example.com")
	if err := g.s.AcceptTerms(ctx, "user:1", nil); err != nil {
		t.Fatalf("AcceptTerms: %v", err)
	}
	g.tokenInvalid()
	m := g.newManager()
	g.failedRound(m)
	row := g.live("app.example.com")

	rows := audittest.Rows(t, g.st, audit.ActionIngressCertFailed)
	got := proofRow(t, rows, audit.ActionIngressCertFailed)
	proofSystemRow(t, got, audit.ResultFailure, row.ID, "env-1")
	proofDetails(t, got, map[string]string{"hostname": "app.example.com", "issuer": IssuerProduction, "reason": acme.ClassDNSTokenInvalid})
	// A failed first run holds no key; the PEM marker check in AssertNoSecrets still guards the row.
	audittest.AssertNoSecrets(t, rows, rigToken)
}

// A renewal at half life writes one success row that adds the renewal basis.
func TestAuditProofCertRenewed(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	m.round(ctx) // gives the certificate its renewal clock
	due := g.live("app.example.com")
	g.clock.Advance(due.RenewAt.Sub(g.clock.Now()))
	m.round(ctx)
	if end := g.awaitEnd(); end.outcome != OutcomeRenewed {
		t.Fatalf("the run ended %q, want renewed", end.outcome)
	}
	row := g.live("app.example.com")

	rows := audittest.Rows(t, g.st, audit.ActionIngressCertRenewed)
	got := proofRow(t, rows, audit.ActionIngressCertRenewed)
	proofSystemRow(t, got, audit.ResultSuccess, first.ID, "env-1")
	proofDetails(t, got, map[string]string{"hostname": "app.example.com", "issuer": IssuerProduction, "basis": string(acme.BasisHalf)})
	audittest.AssertNoSecrets(t, rows, keySecrets(t, g, row)...)
}

// An expired certificate nobody refers to is deleted past the grace: one success row says why and where it came from.
func TestAuditProofCertDeleted(t *testing.T) {
	ctx := context.Background()
	g := newRenewRig(t, true)
	m, first := g.issueOne("app.example.com")
	secrets := keySecrets(t, g, first)
	g.claim()
	m.round(ctx)
	g.clock.Advance(Grace + time.Minute)
	m.round(ctx)
	g.clock.Advance(first.NotAfter.Sub(g.clock.Now()) + time.Minute)
	m.round(ctx)
	if !g.gone("app.example.com") {
		t.Fatal("the expired unreferenced certificate is still there")
	}

	rows := audittest.Rows(t, g.st, audit.ActionIngressCertDeleted)
	got := proofRow(t, rows, audit.ActionIngressCertDeleted)
	proofSystemRow(t, got, audit.ResultSuccess, first.ID, "env-1")
	proofDetails(t, got, map[string]string{"hostname": "app.example.com", "reason": DeletedExpiredUnreferenced, "source": SourceACME})
	audittest.AssertNoSecrets(t, rows, secrets...)
}

// A hostname released by one environment and claimed by another under the same issuer moves the certificate: one success
// row in the new environment names both ends.
func TestAuditProofCertMoved(t *testing.T) {
	ctx := context.Background()
	g := newMgrRig(t, nil, "")
	d2 := g.secondEnv(ingress.IssuerProduction, rigToken)
	m, first := g.issueOne("app.example.com")
	secrets := keySecrets(t, g, first)
	g.claim()
	m.round(ctx)
	g.claimIn("env-2", "stack-2", "app.example.com")
	m.round(ctx)

	rows := audittest.Rows(t, g.st, audit.ActionIngressCertMoved)
	got := proofRow(t, rows, audit.ActionIngressCertMoved)
	proofSystemRow(t, got, audit.ResultSuccess, first.ID, "env-2")
	proofDetails(t, got, map[string]string{
		"hostname": "app.example.com", "from_env": "env-1", "to_env": "env-2", "from_domain": g.domain.ID, "to_domain": d2.ID,
		"outcome": movedRepointed, "cause": causeClaim,
	})
	audittest.AssertNoSecrets(t, rows, secrets...)
}
