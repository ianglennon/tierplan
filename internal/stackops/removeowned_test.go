// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// ownedCount counts the automations recorded for a resource.
func ownedCount(t *testing.T, f repoFixture, resourceID string) int {
	t.Helper()
	var n int
	if err := f.repo.Store.Read.QueryRow(`SELECT count(*) FROM owned_resources WHERE resource_id = ?`, resourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDeleteStackWithAutomationsForgetsTheOwnedRows: removing a stack forgets its webhook and its auto-deploy, which have
// no foreign key to the stack, in the same transaction, and touches no other stack's (WR-D04).
func TestDeleteStackWithAutomationsForgetsTheOwnedRows(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	work := stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}}
	gone, _, err := f.repo.CreateStack(ctx, f.envID, "gone", stacks.TargetSwarm, "ada", work)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, err := f.repo.CreateStack(ctx, f.envID, "kept", stacks.TargetSwarm, "ada", work)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Store.Write.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES ('u-owner','owner','x',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{gone.ID, kept.ID} {
		for _, kind := range []string{gitsync.KindGitWebhook, gitsync.KindGitAutoDeploy} {
			if _, err := f.repo.Store.Write.ExecContext(ctx,
				`INSERT INTO owned_resources (kind, resource_id, env_id, name, owner_id, state, created_at, updated_at) VALUES (?, ?, ?, 'x', 'u-owner', 'active', 1, 1)`,
				kind, id, f.envID); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := f.repo.DeleteStackWithAutomations(ctx, gone.ID); err != nil {
		t.Fatalf("DeleteStackWithAutomations: %v", err)
	}
	if n := ownedCount(t, f, gone.ID); n != 0 {
		t.Errorf("%d automations left for the removed stack, want none", n)
	}
	if n := ownedCount(t, f, kept.ID); n != 2 {
		t.Errorf("%d automations for the other stack, want its 2", n)
	}
	// A second removal changes nothing and says the stack is gone.
	if err := f.repo.DeleteStackWithAutomations(ctx, gone.ID); !errors.Is(err, stackops.ErrStackNotFound) {
		t.Errorf("second removal = %v, want ErrStackNotFound", err)
	}
	if n := ownedCount(t, f, kept.ID); n != 2 {
		t.Errorf("%d automations for the other stack after a second removal, want 2", n)
	}
}
