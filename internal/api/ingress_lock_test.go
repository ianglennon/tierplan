// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// TestOperationLockedByIngress: a deploy refused because ingress holds the environment's lock is told that TierPlan is
// changing ingress there (D-11), with the kind and the job in the problem's details and no empty stack named.
func TestOperationLockedByIngress(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	alpha := newStack(t, admin, "alpha")
	h := stackops.Holder{
		Kind: stackops.HolderKindIngress, JobID: "job-ing", Workload: "ingress", By: "ada",
		Started: time.Date(2026, 1, 1, 8, 30, 0, 0, time.UTC),
	}
	release, _, ok := f.stackLocks.TryAcquire(e.ID, h)
	if !ok {
		t.Fatal("the lock was not free")
	}
	t.Cleanup(release)

	writes := len(f.fake.Writes())
	rec := operator.deploy(alpha.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "operation_locked" {
		t.Fatalf("a deploy while ingress changes: %d %s, want 409 operation_locked", rec.Code, rec.Body)
	}
	pr := problemOf(t, rec)
	if want := "TierPlan is changing ingress on " + e.Name + ", started 2026-01-01T08:30:00Z. Try again when it finishes."; pr.Detail != want {
		t.Errorf("detail = %q, want %q", pr.Detail, want)
	}
	var named []string
	for _, er := range pr.Errors {
		named = append(named, er.Location+"="+er.Message)
	}
	for _, want := range []string{"kind=ingress", "job=job-ing", "started=2026-01-01T08:30:00Z"} {
		if !slices.Contains(named, want) {
			t.Errorf("errors = %v, want %s", named, want)
		}
	}
	if slices.ContainsFunc(named, func(s string) bool { return strings.HasPrefix(s, "stack=") || strings.HasPrefix(s, "stackId=") }) {
		t.Errorf("errors = %v, want no empty stack named", named)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("the refused deploy wrote %d times", got-writes)
	}
	if updates.HolderIngress != "ingress" {
		t.Errorf("HolderIngress = %q, want ingress: it is what the API enum and the badge name", updates.HolderIngress)
	}
	if got := updates.HolderKind(h); got != updates.HolderIngress {
		t.Errorf("HolderKind of an ingress hold = %q, want ingress", got)
	}
}
