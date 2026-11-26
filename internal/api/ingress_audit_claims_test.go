// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
)

// The claim proofs of the audit table test (A-01, A-06, D-22): each ingress claim, replacement, release and refusal is its own
// audit row, with the acting principal, the stack's name and the hostnames, and no secret.

// onlyClaimRow fails the test unless rows holds exactly one row, and returns it.
func onlyClaimRow(t *testing.T, rows []audit.Row, action string) audit.Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want 1: %+v", action, len(rows), rows)
	}
	return rows[0]
}

func TestAuditProofClaimed(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "proof-claimed")

	f.deployGood(t, admin, st.ID, ingressCompose("app.apps.example.com"))

	rows := audittest.Rows(t, f.st, audit.ActionIngressClaimed)
	row := onlyClaimRow(t, rows, audit.ActionIngressClaimed)
	if row.ActorType != "user" || row.ActorName != "envadmin-1" {
		t.Errorf("actor = %s/%s, want the deployer", row.ActorType, row.ActorName)
	}
	if row.TargetType != "stack" || row.TargetID != st.ID || row.EnvID != e.ID {
		t.Errorf("target = %s/%s in %s, want the stack %s in %s", row.TargetType, row.TargetID, row.EnvID, st.ID, e.ID)
	}
	if row.Result != string(audit.ResultSuccess) || row.Reason != "" {
		t.Errorf("result = %q reason %q, want success with no reason", row.Result, row.Reason)
	}
	want := map[string]string{"stack": st.Name, "hostnames": "app.apps.example.com", "count": "1"}
	for k, v := range want {
		if row.Details[k] != v {
			t.Errorf("details[%s] = %q, want %q (all: %v)", k, row.Details[k], v, row.Details)
		}
	}
	if len(row.Details) != len(want) {
		t.Errorf("details = %v, want only %v", row.Details, want)
	}
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressClaimReplaced)); n != 0 {
		t.Errorf("ingress.claim_replaced rows = %d, want none for a first claim", n)
	}
	audittest.AssertNoSecrets(t, rows)
}
