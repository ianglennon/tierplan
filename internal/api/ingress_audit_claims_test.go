// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
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

// wantNoOtherEnvironment fails the test when the row names the other environment or its stack, which the refusal's own
// message would have (A-01, T-27-24).
func wantNoOtherEnvironment(t *testing.T, row audit.Row, seed ingressWorldSeed) {
	t.Helper()
	text := row.Reason + " " + row.TargetID + " " + row.EnvID
	for k, v := range row.Details {
		text += " " + k + "=" + v
	}
	for _, name := range []string{seed.otherStack, seed.otherEnvID, seed.otherEnvName, seed.otherStackID} {
		if name != "" && strings.Contains(text, name) {
			t.Errorf("%s row %q names %q of the other environment", row.Action, text, name)
		}
	}
}

func TestAuditProofClaimedFailure(t *testing.T) {
	t.Parallel()
	const host = "race.apps.example.com"
	f, e := stackEnvTuned(t, func(f *envFix) { claimBeforeJob(f, host) })
	seed := seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "proof-claimed-failure")

	// The other environment's stack takes the name after the gate found it free, so the store refuses the first claim.
	if job := jobOf(t, f, f.deployDraft(t, admin, st.ID, ingressCompose(host))); job.State == "done" {
		t.Fatalf("job = %+v, want it failed", job)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressClaimed)
	row := onlyClaimRow(t, rows, audit.ActionIngressClaimed)
	if row.Result != string(audit.ResultFailure) || row.Reason != ingress.CodeHostTaken {
		t.Errorf("result = %q reason %q, want a failure with %s", row.Result, row.Reason, ingress.CodeHostTaken)
	}
	if row.ActorName != "envadmin-1" || row.TargetType != "stack" || row.TargetID != st.ID || row.EnvID != e.ID {
		t.Errorf("row = %+v, want the deployer, on the stack", row)
	}
	wantDetails(t, row, map[string]string{"stack": st.Name, "hostnames": host, "count": "1"})
	wantNoOtherEnvironment(t, row, seed)
	if n := len(audittest.Rows(t, f.st, audit.ActionIngressClaimReplaced)); n != 0 {
		t.Errorf("ingress.claim_replaced rows = %d, want none", n)
	}
	audittest.AssertNoSecrets(t, rows)
}

func TestAuditProofClaimReplacedFailure(t *testing.T) {
	t.Parallel()
	const late = "late.apps.example.com"
	var armed atomic.Bool
	var domainID, envID string
	f, e := stackEnvTuned(t, func(f *envFix) {
		// After the gate found the name free, an Admin reserves it, so the store refuses the claim.
		f.stackBeforeJob = func(ctx context.Context) {
			if !armed.CompareAndSwap(true, false) {
				return
			}
			if _, _, err := f.stackDeps.Ingress.Reserve(ctx, ingressstore.ReserveInput{EnvID: envID, DomainID: domainID, Hostname: late, Actor: "admin"}, nil); err != nil {
				panic("the reservation was refused: " + err.Error())
			}
		}
	})
	seed := seedIngressWorld(t, f, e)
	domainID, envID = seed.domainID, e.ID
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "proof-replaced-failure")
	f.deployGood(t, admin, st.ID, ingressCompose("a.apps.example.com"))
	armed.Store(true)

	if job := jobOf(t, f, f.deployDraft(t, admin, st.ID, ingressCompose("a.apps.example.com", late))); job.State == "done" {
		t.Fatalf("job = %+v, want it failed", job)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressClaimReplaced)
	row := onlyClaimRow(t, rows, audit.ActionIngressClaimReplaced)
	if row.Result != string(audit.ResultFailure) || row.Reason != ingress.CodeHostReserved {
		t.Errorf("result = %q reason %q, want a failure with %s", row.Result, row.Reason, ingress.CodeHostReserved)
	}
	wantDetails(t, row, map[string]string{"stack": st.Name, "hostnames": "a.apps.example.com, " + late, "count": "2"})
	wantNoOtherEnvironment(t, row, seed)
	if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{"a.apps.example.com"}) {
		t.Errorf("claims = %v, want the stack still holding a.apps.example.com", got)
	}
	audittest.AssertNoSecrets(t, rows)
}
