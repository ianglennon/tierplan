// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrecon

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender"
)

// corruptedPrefix is a stored path that the parser would never let in: it holds braces.
const corruptedPrefix = "/a{{b}}"

// corruptRoute rewrites the stored path of host's route by SQL, bypassing the parser as corrupted data would.
func corruptRoute(t *testing.T, rg *rig, host, prefix string) {
	t.Helper()
	res, err := rg.st.Write.Exec(`UPDATE ingress_routes SET path_prefix = ? WHERE hostname = ?`, prefix, host)
	if err != nil {
		t.Fatalf("corrupt the route of %s: %v", host, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("corrupted %d routes of %s, want 1", n, host)
	}
}

// D-20 / D-08 / PROH-21-06 tracer: a stored route the renderer refuses leaves the last good bundle in place, tells
// Changes nothing and is recorded as a Failure that names the route and the code.
func TestFailedRenderKeepsTheLastGoodBundle(t *testing.T) {
	ctx := context.Background()
	rg := newRig(t)
	rg.certs.setServable("app.example.com")
	rg.seedSwarmStack("stack-shop", "shop")
	rg.storeRoutes("stack-shop", decl("app.example.com", "/"))

	r := New(rg.deps(nil))
	if err := r.Reconcile(ctx, rigEnv); err != nil {
		t.Fatalf("the first render: %v", err)
	}
	good, ok := r.Current(rigEnv)
	if !ok {
		t.Fatal("Current has no bundle after the first render")
	}
	if got := r.Failures(); len(got) != 0 {
		t.Fatalf("Failures = %+v after a good render, want none", got)
	}

	corruptRoute(t, rg, "app.example.com", corruptedPrefix)
	sub := r.Changes().Subscribe()
	t.Cleanup(sub.Close)

	err := r.Reconcile(ctx, rigEnv)
	var refused *ingressrender.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Reconcile = %v, want a *ingressrender.RefusedError", err)
	}
	kept, ok := r.Current(rigEnv)
	if !ok || kept.ID != good.ID {
		t.Fatalf("Current = %q (have %v), want the last good bundle %q", kept.ID, ok, good.ID)
	}
	select {
	case <-sub.C():
		t.Fatalf("Changes was notified for %v, want nobody told of a failed render", sub.Pending())
	default:
	}
	got := r.Failures()
	if len(got) != 1 {
		t.Fatalf("Failures = %+v, want one", got)
	}
	f := got[0]
	if f.EnvID != rigEnv || f.Hostname != "app.example.com" || f.PathPrefix != corruptedPrefix || f.Code != ingressrender.CodeUnsafeValue {
		t.Errorf("Failure = %+v, want %s, app.example.com, %q and %s", f, rigEnv, corruptedPrefix, ingressrender.CodeUnsafeValue)
	}
}
