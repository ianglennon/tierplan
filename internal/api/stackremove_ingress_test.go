// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
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
	wantNamedActor(t, f, "ingress.claim_released", "envadmin-1")

	// The name is free at once: a second stack deploys with it.
	other := newStack(t, admin, "takes-the-name")
	f.deployGood(t, admin, other.ID, ingressCompose(host))
	if got := claimsOf(t, f, e.ID, other.ID); !slices.Equal(got, []string{host}) {
		t.Errorf("the second stack holds %v, want %s", got, host)
	}
}

func TestRemoveKeepDefinitionReleases(t *testing.T) {
	t.Parallel()
	const host = "keep.apps.example.com"
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "keeps-definition")
	f.deployGood(t, admin, st.ID, ingressCompose(host))

	_, job := f.removeStack(t, admin, st.ID, removeBody("keeps-definition", true))

	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if rec := admin.do(http.MethodGet, "stacks/"+st.ID, ""); rec.Code != http.StatusOK {
		t.Errorf("GET of the kept stack: %d %s, want 200", rec.Code, rec.Body)
	}
	if got := claimsOf(t, f, e.ID, st.ID); len(got) != 0 {
		t.Errorf("claims after a removal that keeps the definition = %v, want none", got)
	}
}

func TestRemovePreviewListsHostnames(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	routed := newStack(t, admin, "preview-routed")
	plain := newStack(t, admin, "preview-plain")
	f.deployGood(t, admin, routed.ID, ingressCompose("b.apps.example.com", "a.apps.example.com"))
	f.deployGood(t, admin, plain.ID, composeWebAndApi)

	rec := admin.do(http.MethodGet, removalPreviewPath(routed.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if got := answerOf[removalPreviewAnswer](t, rec).Hostnames; !slices.Equal(got, []string{"a.apps.example.com", "b.apps.example.com"}) {
		t.Errorf("hostnames = %v, want both, sorted", got)
	}

	rec = admin.do(http.MethodGet, removalPreviewPath(plain.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview of a stack with no route: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"hostnames":[]`) {
		t.Errorf("body = %s, want an empty hostnames array and not null", rec.Body)
	}
}

// pendingEnvs are the environments the reconciler was woken for since the last read.
func pendingEnvs(sub *ingress.Subscription) []string { return sub.Pending() }

func TestStackDeleteNotifies(t *testing.T) {
	t.Parallel()
	notifier := ingress.NewNotifier()
	f, e := stackEnvTuned(t, func(f *envFix) { f.ingressNotifier = notifier })
	seedIngressWorld(t, f, e)
	admin := f.caller(t, e, "envadmin-1", "admin")
	sub := notifier.Subscribe()
	t.Cleanup(sub.Close)

	// A stack that holds no hostname: the release changes nothing, so a wake-up after the definition is deleted can only
	// be the removal's own.
	plain := newStack(t, admin, "notifies-plain")
	f.deployGood(t, admin, plain.ID, composeWebAndApi)
	pendingEnvs(sub)
	_, job := f.removeStack(t, admin, plain.ID, removeBody("notifies-plain", false))
	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if got := pendingEnvs(sub); !slices.Equal(got, []string{e.ID}) {
		t.Errorf("woken environments after a deleted stack = %v, want [%s]", got, e.ID)
	}

	// A removal that keeps the definition deletes nothing, so nobody is woken for it.
	kept := newStack(t, admin, "notifies-kept")
	f.deployGood(t, admin, kept.ID, composeWebAndApi)
	pendingEnvs(sub)
	_, job = f.removeStack(t, admin, kept.ID, removeBody("notifies-kept", true))
	if job.State != "done" {
		t.Fatalf("removal = %+v, want it done", job)
	}
	if got := pendingEnvs(sub); len(got) != 0 {
		t.Errorf("woken environments after a removal that keeps the definition = %v, want none", got)
	}
}
