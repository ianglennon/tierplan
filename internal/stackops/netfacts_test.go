// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// What the stacks say about networks (plan 10-10, NET-04): which stacks exist, which networks they
// declare external and who holds the operation lock.

const proxyCompose = `services:
  web:
    image: nginx
    networks: [proxy, internal]
networks:
  proxy:
    external: true
    name: traefik_public
  internal: {}
  shared:
    external: true
`

func (f repoFixture) addStack(t *testing.T, name, content string) stackops.Stack {
	t.Helper()
	st, _, err := f.repo.CreateStack(testsupport.Context(t), f.envID, name, stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose(content)}})
	if err != nil {
		t.Fatalf("CreateStack %s: %v", name, err)
	}
	return st
}

func TestNetFactsExternals(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	web := f.addStack(t, "web", proxyCompose)
	f.addStack(t, "broken", "services: [this is not a mapping\n")
	facts := stackops.NetFacts{Repo: f.repo, Locks: stackops.NewLocks()}
	ctx := testsupport.Context(t)

	names, ids, err := facts.Stacks(ctx, f.envID)
	if err != nil || !names["web"] || !names["broken"] || !ids[web.ID] || len(names) != 2 {
		t.Fatalf("Stacks = %v %v, %v, want both stacks by name and by ID", names, ids, err)
	}
	ext, unknown, err := facts.ExternalNetworks(ctx, f.envID)
	if err != nil {
		t.Fatalf("ExternalNetworks: %v", err)
	}
	// The declared name wins over the key; the key stands in for a missing name; an owned network is not external.
	if !ext["traefik_public"] || !ext["shared"] || ext["proxy"] || ext["internal"] || len(ext) != 2 {
		t.Errorf("externals = %v, want traefik_public and shared", ext)
	}
	if !slices.Equal(unknown, []string{"broken"}) {
		t.Errorf("unknown = %v, want the stack that does not load", unknown)
	}
	// Another environment's stacks are not this one's.
	if other, _, err := facts.ExternalNetworks(ctx, "env-2"); err != nil || len(other) != 0 {
		t.Errorf("env-2 externals = %v, %v, want none", other, err)
	}
}

func TestNetFactsMemoized(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	web := f.addStack(t, "web", proxyCompose)
	var checks atomic.Int64
	facts := stackops.NetFacts{Repo: f.repo, Check: func(ctx context.Context, in stacks.CheckInput) stacks.CheckResult {
		checks.Add(1)
		return stacks.Check(ctx, in)
	}}
	ctx := testsupport.Context(t)
	for range 3 {
		if ext, _, err := facts.ExternalNetworks(ctx, f.envID); err != nil || !ext["traefik_public"] {
			t.Fatalf("ExternalNetworks = %v, %v", ext, err)
		}
	}
	if n := checks.Load(); n != 1 {
		t.Fatalf("the unchanged stack was parsed %d times, want once", n)
	}
	// A saved draft is a new version and is parsed again, with its new answer.
	draft, err := f.repo.Draft(ctx, web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.SaveDraft(ctx, web.ID, draft.Version, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\nnetworks:\n  other:\n    external: true\n")}}); err != nil {
		t.Fatal(err)
	}
	ext, _, err := facts.ExternalNetworks(ctx, f.envID)
	if err != nil || !ext["other"] || ext["traefik_public"] {
		t.Fatalf("after the edit externals = %v, %v, want other only", ext, err)
	}
	if n := checks.Load(); n != 2 {
		t.Errorf("parsed %d times after one edit, want twice", n)
	}
}

func TestNetFactsBusy(t *testing.T) {
	t.Parallel()
	locks := stackops.NewLocks()
	facts := stackops.NetFacts{Locks: locks}
	if name, ok := facts.Busy("env-1"); ok || name != "" {
		t.Fatalf("Busy = %q %v with nothing held", name, ok)
	}
	release, _, ok := locks.TryAcquire("env-1", stackops.Holder{StackID: "s1", StackName: "web", Kind: "deploy"})
	if !ok {
		t.Fatal("could not take the lock")
	}
	if name, ok := facts.Busy("env-1"); !ok || name != "web" {
		t.Errorf("Busy = %q %v, want web", name, ok)
	}
	if _, ok := facts.Busy("env-2"); ok {
		t.Errorf("another environment is busy too")
	}
	release()
	if _, ok := facts.Busy("env-1"); ok {
		t.Errorf("still busy after the release")
	}
	if _, ok := (stackops.NetFacts{}).Busy("env-1"); ok {
		t.Errorf("no locks cannot be busy")
	}
}
