// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestStandaloneTokenRotationRefusedBeforeEnrolment: an Admin's rotation on a standalone host with ingress on, whose proxy
// has not enrolled, is refused with ErrTokenNotEnrolled and issues nothing.
func TestStandaloneTokenRotationRefusedBeforeEnrolment(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	writes, deploys := len(r.fake.Writes()), r.sys.deployCount()
	_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "203.0.113.5", "test-ua")
	if !errors.Is(err, ingressdeploy.ErrTokenNotEnrolled) {
		t.Fatalf("error = %v, want ErrTokenNotEnrolled", err)
	}
	if ingressdeploy.CodeTokenNotEnrolled != "ingress_token_not_enrolled" {
		t.Errorf("CodeTokenNotEnrolled = %q", ingressdeploy.CodeTokenNotEnrolled)
	}
	if got := r.generation(); got != 1 {
		t.Errorf("token generation = %d, want 1: nothing is issued", got)
	}
	if got := r.auditCount("ingress.join_token_rotated"); got != 0 {
		t.Errorf("ingress.join_token_rotated audit rows = %d, want 0", got)
	}
	if len(r.fake.Writes()) != writes || r.sys.deployCount() != deploys {
		t.Error("the refused rotation wrote to the host")
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the refused rotation kept the operation lock")
	}
}

// TestStandaloneTokenRotationAfterEnrolmentTouchesNothing: once a proxy node is active the rotation issues the next token
// and nothing else: no write to the host, no deploy, no wake of the ensure loop.
func TestStandaloneTokenRotationAfterEnrolmentTouchesNothing(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	r.addNodeRow("active")
	writes, deploys := len(r.fake.Writes()), r.sys.deployCount()
	tok, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "203.0.113.5", "test-ua")
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	if !tok.Rotated || tok.Generation != 2 || tok.Token == "" {
		t.Fatalf("token = gen %d rotated %v, want the second generation as a rotation", tok.Generation, tok.Rotated)
	}
	if got := r.generation(); got != 2 {
		t.Errorf("token generation = %d, want 2", got)
	}
	if len(r.fake.Writes()) != writes {
		t.Errorf("the rotation wrote %v to the host", r.fake.Writes()[writes:])
	}
	if r.sys.deployCount() != deploys {
		t.Error("the rotation called the deployer")
	}
	if r.svc.WakePending() {
		t.Error("the rotation woke the ensure loop: the proxy authenticates with its key")
	}
	if got := r.auditCount("ingress.join_token_rotated"); got != 1 {
		t.Errorf("ingress.join_token_rotated audit rows = %d, want 1", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the rotation kept the operation lock")
	}

	// The loop, which sees an enrolled proxy, does not take the rotation as a reason to touch it.
	r.round()
	if r.sys.deployCount() != deploys {
		t.Error("a pass after the rotation redeployed the proxy")
	}
}

// TestStandaloneTokenRotationIngressOff: with ingress off a standalone environment's rotation is ingresshub's, as before.
func TestStandaloneTokenRotationIngressOff(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	ctx := testsupport.Context(t)
	first, err := r.svc.IssueJoinToken(ctx, testActor, testOwner, r.envID, "", "")
	if err != nil || first.Generation != 1 || first.Rotated {
		t.Fatalf("first IssueJoinToken = %+v, %v; want a first token", first, err)
	}
	second, err := r.svc.IssueJoinToken(ctx, testActor, testOwner, r.envID, "", "")
	if err != nil || second.Generation != 2 || !second.Rotated {
		t.Fatalf("second IssueJoinToken = %+v, %v; want a rotation with no refusal", second, err)
	}
	if r.sys.deployCount() != 0 || r.svc.WakePending() {
		t.Error("an off environment's rotation touched the host")
	}
}
