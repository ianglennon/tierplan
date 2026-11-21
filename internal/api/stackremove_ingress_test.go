// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"slices"
	"testing"
)

// Removing a stack lets go of its hostnames once the engine has nothing of it, and tells the reconciler (plan 24-25,
// LBL-06, D-17).

func TestRemoveReleasesClaimsThroughRoute(t *testing.T) {
	t.Parallel()
	const host = "remove.apps.example.com"
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "removes-claims")
	f.deployGood(t, admin, st.ID, ingressCompose(host))
	if got := claimsOf(t, f, e.ID, st.ID); !slices.Equal(got, []string{host}) {
		t.Fatalf("claims before the removal = %v, want %s", got, host)
	}
	released := claimAudits(t, f, "ingress.claim_released")

	_, job := f.removeStack(t, admin, st.ID, removeBody("removes-claims", false))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if n := countPrefix(f.fake.Writes(), "DELETE /services/"); n == 0 {
		t.Errorf("writes = %v, want the service removed", f.fake.Writes())
	}
	if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
		t.Errorf("claims after the removal = %v, want none", got)
	}
	if n := claimAudits(t, f, "ingress.claim_released"); n != released+1 {
		t.Errorf("ingress.claim_released rows = %d, want %d", n, released+1)
	}

	// The name is free at once: a second stack deploys with it.
	other := newStack(t, admin, "takes-the-name")
	f.deployGood(t, admin, other.ID, ingressCompose(host))
	if got := claimsOf(t, f, e.ID, other.ID); !slices.Equal(got, []string{host}) {
		t.Errorf("the second stack holds %v, want %s", got, host)
	}
}
