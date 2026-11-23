// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/storetest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestAttachForStandalone: the ingress model answers for a standalone stack as it does for a swarm one: the shared
// network when the environment's ingress is on, nothing when it is off (D-16).
func TestAttachForStandalone(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	st := storetest.New(t)
	if _, err := st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('env-a', 'agent', 'env a', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	store := ingressstore.New(ingressstore.Deps{St: st})
	deps := StacksDeps{Ingress: store}

	for _, target := range []stacks.Target{stacks.TargetStandalone, stacks.TargetSwarm} {
		// Never switched: off.
		got, err := ingressAttachFor(ctx, deps, "env-a", target)
		if err != nil || got != nil {
			t.Fatalf("%s with ingress off: %+v, %v, want nothing attached", target, got, err)
		}
	}

	if _, err := store.SetEnabled(ctx, "env-a", true, "admin", nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []stacks.Target{stacks.TargetStandalone, stacks.TargetSwarm} {
		got, err := ingressAttachFor(ctx, deps, "env-a", target)
		if err != nil || got == nil || got.Network != ingress.NetworkName {
			t.Fatalf("%s with ingress on: %+v, %v, want the %s network", target, got, err, ingress.NetworkName)
		}
	}

	if _, err := store.SetEnabled(ctx, "env-a", false, "admin", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ingressAttachFor(ctx, deps, "env-a", stacks.TargetStandalone); err != nil || got != nil {
		t.Fatalf("standalone with ingress switched off again: %+v, %v, want nothing attached", got, err)
	}
}
