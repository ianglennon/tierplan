// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestAdminRotationIsKeptByTheEnsureLoop: an Admin rotates the join token of an environment whose ingress is on. The token
// the Admin receives is the one the swarm's secret carries, the loop then updates the service to name that secret and
// makes no further token, and the Admin's token is still the environment's current one afterwards (WR-15).
func TestAdminRotationIsKeptByTheEnsureLoop(t *testing.T) {
	r := enabledRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	st1, _ := r.tokens.TokenStatus(ctx, r.envID)
	secretsBefore := len(r.fake.SecretCreates())

	tok, err := r.svc.IssueJoinToken(ctx, testActor, testOwner, r.envID, "203.0.113.5", "test-ua")
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}
	if !tok.Rotated || tok.Generation != 2 || tok.ID == st1.ID {
		t.Fatalf("token = gen %d rotated %v, want the second generation as a rotation", tok.Generation, tok.Rotated)
	}
	if _, _, ok := ingresshub.ParseToken(tok.Token); !ok {
		t.Fatal("the token the Admin received is not a well-formed ingress join token")
	}
	name2 := ingressdeploy.SecretName(r.envID, 2, tok.ID)
	creates := r.fake.SecretCreates()
	if len(creates) != secretsBefore+1 || creates[len(creates)-1].Name != name2 || creates[len(creates)-1].DataBytes != len(tok.Token) {
		t.Fatalf("secret creates = %+v, want one new secret named %s holding the whole token", creates, name2)
	}
	if got := r.auditCount("ingress.join_token_rotated"); got != 1 {
		t.Errorf("ingress.join_token_rotated audit rows = %d, want 1", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the rotation kept the operation lock")
	}
	if !r.svc.WakePending() {
		t.Error("the rotation did not wake the ensure loop to move the service to the new secret")
	}

	// The loop's next passes bring the service to the new secret and mint nothing.
	r.round()
	r.round()
	st2, _ := r.tokens.TokenStatus(ctx, r.envID)
	if st2.Generation != 2 || st2.ID != tok.ID || st2.Disabled {
		t.Fatalf("the current token after the loop ran = %+v, want the Admin's (generation 2, %s)", st2, tok.ID)
	}
	if got := r.auditCount("ingress.join_token_rotated"); got != 1 {
		t.Errorf("ingress.join_token_rotated audit rows after the loop = %d, want still 1: the loop must not rotate again", got)
	}
	refs := r.liveSpec().TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 || refs[0].SecretName != name2 {
		t.Errorf("the service's secret references = %+v, want %s", refs, name2)
	}
	if names := secretNames(r); len(names) != 1 || names[0] != name2 {
		t.Errorf("secrets after the loop = %v, want only the Admin's generation %s", names, name2)
	}
}

// TestIssueJoinTokenWhereNothingIsDeployed: with ingress off, on a standalone environment or on an environment the
// deployer does not know of, the token is made as before and no swarm secret is written.
func TestIssueJoinTokenWhereNothingIsDeployed(t *testing.T) {
	t.Run("ingress off", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		tok, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", "")
		if err != nil || tok.Token == "" || tok.Generation != 1 {
			t.Fatalf("IssueJoinToken = %+v, %v; want a first token", tok, err)
		}
		if n := len(r.fake.SecretCreates()); n != 0 {
			t.Errorf("secret creates = %d, want none while ingress is off", n)
		}
	})
	t.Run("standalone", func(t *testing.T) {
		r := newRig(t, rigOptions{standalone: true})
		if _, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", ""); err != nil {
			t.Fatalf("IssueJoinToken: %v", err)
		}
		if n := len(r.fake.SecretCreates()); n != 0 {
			t.Errorf("secret creates = %d, want none on a standalone environment", n)
		}
	})
	t.Run("no such environment", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, "00000000-0000-0000-0000-000000000000", "", "")
		if !errors.Is(err, ingresshub.ErrEnvironmentNotFound) {
			t.Errorf("error = %v, want ErrEnvironmentNotFound", err)
		}
	})
}

// TestIssueJoinTokenRefusals: with ingress on, a rotation that cannot be kept in step with the swarm is refused before a
// token is made, so the Admin is never handed one the loop would replace: while another operation holds the environment
// (a *LockedError), while a restore's pause stands (ErrPaused) and while no manager is connected (ErrNoManager). A secret
// the swarm refuses after the token was made is a *TokenNotStoredError, with the swarm unchanged.
func TestIssueJoinTokenRefusals(t *testing.T) {
	gen := func(r *rig) int {
		st, err := r.tokens.TokenStatus(testsupport.Context(t), r.envID)
		if err != nil {
			t.Fatalf("TokenStatus: %v", err)
		}
		return st.Generation
	}
	t.Run("locked", func(t *testing.T) {
		r := enabledRig(t, rigOptions{})
		release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
		if !ok {
			t.Fatal("could not take the lock")
		}
		defer release()
		_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", "")
		var locked *ingressdeploy.LockedError
		if !errors.As(err, &locked) || locked.Holder.Kind != "deploy" {
			t.Fatalf("error = %v, want a *LockedError naming the stack deploy", err)
		}
		if gen(r) != 1 {
			t.Error("a token was made though the lock was held")
		}
	})
	t.Run("paused", func(t *testing.T) {
		r := enabledRig(t, rigOptions{})
		if _, err := r.istore.SetPaused(testsupport.Context(t), r.envID, true, "system:restore", nil); err != nil {
			t.Fatalf("SetPaused: %v", err)
		}
		_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", "")
		if !errors.Is(err, ingressdeploy.ErrPaused) {
			t.Fatalf("error = %v, want ErrPaused", err)
		}
		if gen(r) != 1 {
			t.Error("a token was made while the environment was paused")
		}
	})
	t.Run("no manager", func(t *testing.T) {
		r := enabledRig(t, rigOptions{})
		r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
		_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", "")
		if !errors.Is(err, ingressdeploy.ErrNoManager) {
			t.Fatalf("error = %v, want ErrNoManager", err)
		}
		if gen(r) != 1 {
			t.Error("a token was made with no manager to store it on")
		}
	})
	t.Run("the swarm refuses the secret", func(t *testing.T) {
		var base dockertest.FakeConfig
		r := enabledRig(t, rigOptions{config: func(c *dockertest.FakeConfig) { base = *c }})
		// The swarm refuses every write from here on, so the token is made and its secret is not.
		refusing := base
		refusing.WriteError = "the manager refused the write"
		r.fake.Reconfigure(refusing)
		_, err := r.svc.IssueJoinToken(testsupport.Context(t), testActor, testOwner, r.envID, "", "")
		var stored *ingressdeploy.TokenNotStoredError
		if !errors.As(err, &stored) {
			t.Fatalf("error = %v, want a *TokenNotStoredError", err)
		}
		if _, held := r.locks.Held(r.envID); held {
			t.Error("the failed rotation kept the lock")
		}
		if names := secretNames(r); len(names) != 1 {
			t.Errorf("secrets = %v, want the swarm unchanged", names)
		}
	})
}
