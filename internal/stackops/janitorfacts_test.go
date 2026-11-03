// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// What a stack's own compose file says about the janitor (plan 11-29, JAN-07, D-10, D-19, Pitfall 9, A10).

// composeWith is a one-service compose file with the given text added at the top level.
func composeWith(top string) string {
	return "services:\n  web:\n    image: nginx\n" + top
}

// exclusionOf is the one stack's answer from the facts.
func exclusionOf(t *testing.T, f repoFixture, facts stackops.JanitorFacts, st stackops.Stack) stackops.StackExclusion {
	t.Helper()
	got, err := facts.Exclusions(testsupport.Context(t), f.envID)
	if err != nil {
		t.Fatalf("Exclusions: %v", err)
	}
	for _, e := range got {
		if e.ID == st.ID {
			if e.Name != st.Name {
				t.Errorf("exclusion = %+v, want the name %q", e, st.Name)
			}
			return e
		}
	}
	t.Fatalf("Exclusions = %+v, want one for %s", got, st.Name)
	return stackops.StackExclusion{}
}

func TestJanitorFactsDeclared(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	facts := stackops.JanitorFacts{Repo: f.repo}
	for name, top := range map[string]string{
		"boolean": "x-tierplan:\n  keep: true\n",
		"string":  "x-tierplan:\n  keep: \"true\"\n",
		"flow":    "x-tierplan: {keep: true, other: 1}\n",
	} {
		st := f.addStack(t, name, composeWith(top))
		if e := exclusionOf(t, f, facts, st); !e.Declared || e.Unknown {
			t.Errorf("%s: %+v, want declared and known", name, e)
		}
	}
	// The key may sit anywhere in the file, and the stack that does not say it is not declared.
	st := f.addStack(t, "first", "x-tierplan:\n  keep: true\n"+composeWith(""))
	if e := exclusionOf(t, f, facts, st); !e.Declared {
		t.Errorf("x-tierplan before services: %+v, want declared", e)
	}
	plain := f.addStack(t, "plain", composeWith(""))
	if e := exclusionOf(t, f, facts, plain); e.Declared || e.Unknown {
		t.Errorf("a stack with no x-tierplan: %+v, want neither", e)
	}
}

// Only the YAML boolean true or the exact string "true" declares a stack out of cleanup (T-11-83).
func TestJanitorFactsEncoding(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	facts := stackops.JanitorFacts{Repo: f.repo}
	for name, top := range map[string]string{
		"absent":       "",
		"empty":        "x-tierplan: {}\n",
		"null":         "x-tierplan:\n",
		"false":        "x-tierplan:\n  keep: false\n",
		"upper":        "x-tierplan:\n  keep: \"True\"\n",
		"allupper":     "x-tierplan:\n  keep: \"TRUE\"\n",
		"one":          "x-tierplan:\n  keep: 1\n",
		"quotedone":    "x-tierplan:\n  keep: \"1\"\n",
		"yes":          "x-tierplan:\n  keep: \"yes\"\n",
		"padded":       "x-tierplan:\n  keep: \" true\"\n",
		"list":         "x-tierplan:\n  keep: [true]\n",
		"map":          "x-tierplan:\n  keep: {a: true}\n",
		"scalar":       "x-tierplan: true\n",
		"othername":    "x-tierplan-keep: true\n",
		"nested":       "x-tierplan:\n  other:\n    keep: true\n",
		"othercase":    "x-Tierplan:\n  keep: true\n",
		"stringparent": "x-tierplan: \"keep: true\"\n",
	} {
		st := f.addStack(t, name, composeWith(top))
		if e := exclusionOf(t, f, facts, st); e.Declared || e.Unknown {
			t.Errorf("%s: %+v, want a known stack that is not declared", name, e)
		}
	}
}

// A tierplan.keep label on a service protects that service's containers only; it never declares the stack (D-19).
func TestJanitorFactsServiceLabelNotStack(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	facts := stackops.JanitorFacts{Repo: f.repo}
	st := f.addStack(t, "labelled", "services:\n  web:\n    image: nginx\n    labels:\n      tierplan.keep: \"true\"\n  db:\n    image: postgres\n    labels:\n      - tierplan.keep=true\n")
	if e := exclusionOf(t, f, facts, st); e.Declared || e.Unknown {
		t.Fatalf("a service label alone: %+v, want not declared", e)
	}
}

// A definition that does not load is Unknown, never Declared, and is parsed again next time.
func TestJanitorFactsUnknown(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	var checks atomic.Int64
	facts := stackops.JanitorFacts{Repo: f.repo, Check: func(ctx context.Context, in stacks.CheckInput) stacks.CheckResult {
		checks.Add(1)
		return stacks.Check(ctx, in)
	}}
	// Declares keep in the text but does not load: the declaration is not believed.
	broken := f.addStack(t, "broken", "x-tierplan:\n  keep: true\nservices: [this is not a mapping\n")
	good := f.addStack(t, "good", composeWith("x-tierplan:\n  keep: true\n"))
	for range 2 {
		if e := exclusionOf(t, f, facts, broken); !e.Unknown || e.Declared {
			t.Fatalf("a stack that does not load: %+v, want unknown and not declared", e)
		}
	}
	if e := exclusionOf(t, f, facts, good); !e.Declared || e.Unknown {
		t.Fatalf("the good stack beside it: %+v, want declared", e)
	}
	// Each read lists every stack. The broken one was parsed on all three reads (a failure is not remembered)
	// and the good one once.
	if n := checks.Load(); n != 4 {
		t.Errorf("%d parses over three reads of a broken stack and a good one, want 4 (failures are not memoised)", n)
	}
	// A cancelled read is an error, not an answer.
	ctx, cancel := context.WithCancel(testsupport.Context(t))
	cancel()
	if got, err := facts.Exclusions(ctx, f.envID); err == nil {
		t.Errorf("Exclusions with a cancelled context = %+v, want an error", got)
	}
	// Another environment's stacks are not this one's.
	if got, err := facts.Exclusions(testsupport.Context(t), "env-2"); err != nil || len(got) != 0 {
		t.Errorf("env-2 = %+v, %v, want none", got, err)
	}
}

// The answer is remembered per version of the stack, and shared with the network facts: one parse serves both.
func TestJanitorFactsMemo(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	var checks atomic.Int64
	check := func(ctx context.Context, in stacks.CheckInput) stacks.CheckResult {
		checks.Add(1)
		return stacks.Check(ctx, in)
	}
	facts := stackops.JanitorFacts{Repo: f.repo, Check: check}
	st := f.addStack(t, "web", composeWith("x-tierplan:\n  keep: true\nnetworks:\n  shared:\n    external: true\n"))
	for range 3 {
		if e := exclusionOf(t, f, facts, st); !e.Declared {
			t.Fatalf("exclusion = %+v, want declared", e)
		}
	}
	net := stackops.NetFacts{Repo: f.repo, Check: check}
	if ext, _, err := net.ExternalNetworks(testsupport.Context(t), f.envID); err != nil || !ext["shared"] {
		t.Fatalf("ExternalNetworks = %v, %v", ext, err)
	}
	if n := checks.Load(); n != 1 {
		t.Fatalf("the unchanged stack was parsed %d times across four reads, want once", n)
	}
	// A saved draft is a new version: parsed again, with its new answer.
	draft, err := f.repo.Draft(testsupport.Context(t), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.repo.SaveDraft(testsupport.Context(t), st.ID, draft.Version, "ada", stacks.WorkingCopy{Files: []stacks.File{compose(composeWith(""))}}); err != nil {
		t.Fatal(err)
	}
	if e := exclusionOf(t, f, facts, st); e.Declared {
		t.Errorf("after the keep was removed from the file: %+v, want not declared", e)
	}
	if n := checks.Load(); n != 2 {
		t.Errorf("parsed %d times after one edit, want twice", n)
	}
	if (stackops.JanitorFacts{}).Configured() || !facts.Configured() {
		t.Errorf("Configured() must be false with no store and true with one")
	}
}
