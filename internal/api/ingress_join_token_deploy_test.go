// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The join token route on an environment whose ingress is on (Phase 24, WR-15): the token an Admin is given is the one the
// swarm's secret carries, so the deployer does not replace it, and a rotation that cannot be kept in step is refused.

// TestJoinTokenRouteStoresTheSwarmSecretWhenIngressIsOn: with ingress on, a rotation answers 200 with the token and the swarm
// holds a new secret of the token's whole length; with ingress off the token is made and no secret is written.
func TestJoinTokenRouteStoresTheSwarmSecretWhenIngressIsOn(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	path := nodesPath(f.envA, "join-token")

	off := f.credCall(t, f.adminA, http.MethodPost, path, "")
	if off.Code != http.StatusOK {
		t.Fatalf("issue with ingress off: %d %s", off.Code, off.Body)
	}
	if n := len(f.fake.SecretCreates()); n != 0 {
		t.Fatalf("secret creates with ingress off = %d, want none", n)
	}

	f.enableViaRoute(t)
	before := len(f.fake.SecretCreates())
	rec := f.credCall(t, f.adminA, http.MethodPost, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("issue with ingress on: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	tok := decodeBody[ingressTokenResp](t, rec)
	creates := f.fake.SecretCreates()
	if len(creates) != before+1 || creates[len(creates)-1].DataBytes != len(tok.Token) {
		t.Fatalf("secret creates = %+v, want one more holding the whole token", creates)
	}
	if !tok.Rotated || tok.Generation < 3 {
		t.Errorf("token = %+v, want a rotation past the enable's token", tok)
	}
}

// TestJoinTokenRouteRefusesWhatItCannotKeepInStep: with ingress on, a held operation lock is 409 operation_locked and a
// restore's pause 409 ingress_paused; no token is made in either, so the current generation does not move.
func TestJoinTokenRouteRefusesWhatItCannotKeepInStep(t *testing.T) {
	t.Parallel()
	f := newDeployFix(t, false)
	f.enableViaRoute(t)
	path := nodesPath(f.envA, "join-token")
	before := f.credCall(t, f.adminA, http.MethodGet, path, "")
	gen := decodeBody[struct {
		Generation int `json:"generation"`
	}](t, before).Generation

	release, _, ok := f.stackLocks.TryAcquire(f.envA, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
	if !ok {
		t.Fatal("could not take the lock")
	}
	rec := f.credCall(t, f.adminA, http.MethodPost, path, "")
	release()
	if typ, _ := deployProblem(t, rec.Body.Bytes()); rec.Code != http.StatusConflict || typ != "operation_locked" {
		t.Fatalf("issue while locked: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}

	if _, err := f.ingressStore.SetPaused(testsupport.Context(t), f.envA, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	rec = f.credCall(t, f.adminA, http.MethodPost, path, "")
	if typ, _ := deployProblem(t, rec.Body.Bytes()); rec.Code != http.StatusConflict || typ != "ingress_paused" {
		t.Fatalf("issue while paused: %d %s, want 409 ingress_paused", rec.Code, rec.Body)
	}

	after := f.credCall(t, f.adminA, http.MethodGet, path, "")
	if got := decodeBody[struct {
		Generation int `json:"generation"`
	}](t, after).Generation; got != gen {
		t.Errorf("generation = %d after two refusals, want %d: no token was made", got, gen)
	}
}
