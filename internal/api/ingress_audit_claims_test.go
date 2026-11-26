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

// wantDetails fails the test unless the row's details are exactly want.
func wantDetails(t *testing.T, row audit.Row, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if row.Details[k] != v {
			t.Errorf("%s details[%s] = %q, want %q (all: %v)", row.Action, k, row.Details[k], v, row.Details)
		}
	}
	if len(row.Details) != len(want) {
		t.Errorf("%s details = %v, want only %v", row.Action, row.Details, want)
	}
}

func TestAuditProofClaimReplaced(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "proof-replaced")
	f.deployGood(t, admin, st.ID, ingressCompose("a.apps.example.com", "b.apps.example.com"))
	// a.apps.example.com changes its port and b.apps.example.com is dropped.
	f.deployGood(t, admin, st.ID, "services:\n  weba:\n    image: nginx:1.27\n    deploy:\n      labels:\n"+
		"        tierplan.ingress.host: a.apps.example.com\n        tierplan.ingress.port: \"8080\"\n")

	rows := audittest.Rows(t, f.st, audit.ActionIngressClaimReplaced)
	row := onlyClaimRow(t, rows, audit.ActionIngressClaimReplaced)
	if row.Result != string(audit.ResultSuccess) || row.TargetID != st.ID || row.ActorName != "envadmin-1" {
		t.Errorf("row = %+v, want a success by the deployer on the stack", row)
	}
	wantDetails(t, row, map[string]string{"stack": st.Name, "hostnames": "a.apps.example.com, b.apps.example.com", "count": "2"})
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressClaimed)); n != 1 {
		t.Errorf("ingress.claimed rows = %d, want only the first deploy's", n)
	}
	audittest.AssertNoSecrets(t, rows)
}

func TestAuditProofClaimReleased(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")

	// Removing a stack that holds two hostnames releases them.
	gone := newStack(t, admin, "proof-released")
	f.deployGood(t, admin, gone.ID, ingressCompose("one.apps.example.com", "two.apps.example.com"))
	if _, job := f.removeStack(t, admin, gone.ID, removeBody("proof-released", false)); job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	released := audittest.Rows(t, f.st, audit.ActionIngressClaimReleased)
	row := onlyClaimRow(t, released, audit.ActionIngressClaimReleased)
	if row.Result != string(audit.ResultSuccess) || row.TargetID != gone.ID || row.ActorName != "envadmin-1" {
		t.Errorf("row = %+v, want a success by the remover on the stack", row)
	}
	wantDetails(t, row, map[string]string{"stack": gone.Name, "count": "2"})

	// A deploy whose revision declares no route is a replacement, as it was before: both names are Removed.
	dropped := newStack(t, admin, "proof-dropped")
	f.deployGood(t, admin, dropped.ID, ingressCompose("three.apps.example.com", "four.apps.example.com"))
	f.deployGood(t, admin, dropped.ID, "services:\n  web:\n    image: nginx:1.27\n")
	replaced := audittest.Rows(t, f.st, audit.ActionIngressClaimReplaced)
	row = onlyClaimRow(t, replaced, audit.ActionIngressClaimReplaced)
	wantDetails(t, row, map[string]string{"stack": dropped.Name, "hostnames": "four.apps.example.com, three.apps.example.com", "count": "2"})
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressClaimReleased)); n != 1 {
		t.Errorf("ingress.claim_released rows = %d, want only the removal's", n)
	}
	audittest.AssertNoSecrets(t, append(released, replaced...))
}
