// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// DeployedMasker hides what a stack's containers were given (plan 11-13, D-06, RESEARCH F8).

func TestDeployedMasker(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	wc := stacks.WorkingCopy{
		Files: []stacks.File{compose("a")},
		Vars: []stacks.Var{
			{Name: "DB_PASSWORD", Value: "s3cr3t-value", Secret: true},
			{Name: "PORT", Value: "8080"},
		},
	}
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", wc)
	if err != nil {
		t.Fatal(err)
	}

	// Before any revision deployed well, nothing is hidden and nothing is an error.
	m, err := f.repo.DeployedMasker(ctx, st.ID)
	if err != nil || m.Mask("x s3cr3t-value y") != "x s3cr3t-value y" {
		t.Fatalf("a stack never deployed: Mask = %q, %v; want the empty masker and no error", m.Mask("x s3cr3t-value y"), err)
	}

	rev, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	m, err = f.repo.DeployedMasker(ctx, st.ID)
	if err != nil {
		t.Fatalf("DeployedMasker: %v", err)
	}
	got := m.Mask("connect failed with s3cr3t-value on port 8080")
	if strings.Contains(got, "s3cr3t-value") || !strings.Contains(got, "8080") {
		t.Fatalf("Mask = %q, want the secret variable hidden and the plain one left", got)
	}

	// The same through the facts the janitor engine holds.
	facts := stackops.NetFacts{Repo: f.repo}
	fm, err := facts.DeployedMasker(ctx, st.ID)
	if err != nil || strings.Contains(fm.Mask("s3cr3t-value"), "s3cr3t") {
		t.Fatalf("NetFacts.DeployedMasker: %q, %v; want the secret hidden", fm.Mask("s3cr3t-value"), err)
	}
}

func TestDeployedMaskerOfAnUnknownStackIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	m, err := f.repo.DeployedMasker(context.Background(), "no-such-stack")
	if err != nil {
		t.Fatalf("DeployedMasker of an unknown stack = %v, want no error", err)
	}
	if got := m.Mask("anything at all"); got != "anything at all" {
		t.Fatalf("Mask = %q, want the empty masker", got)
	}
}
