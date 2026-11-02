// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

func TestStackOwnedLabelsNameTheStackAndTheTierPlanID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		labels map[string]string
		stack  string
		id     string
		owned  bool
	}{
		"no labels":        {nil, "", "", false},
		"unrelated labels": {map[string]string{"app": "web"}, "", "", false},
		"an empty stack":   {map[string]string{"com.docker.stack.namespace": ""}, "", "", false},
		"a stack":          {map[string]string{"com.docker.stack.namespace": "shop", "app": "web"}, "shop", "", true},
		"a stack TierPlan manages": {
			map[string]string{"com.docker.stack.namespace": "web", "tierplan.stack.id": "stk-1"}, "web", "stk-1", true,
		},
		"a compose project": {map[string]string{"com.docker.compose.project": "shop"}, "shop", "", true},
		"a compose project TierPlan manages": {
			map[string]string{"com.docker.compose.project": "web", "tierplan.stack.id": "stk-2"}, "web", "stk-2", true,
		},
		"the namespace before the project": {
			map[string]string{"com.docker.stack.namespace": "a", "com.docker.compose.project": "b"}, "a", "", true,
		},
		"an id with no stack": {map[string]string{"tierplan.stack.id": "stk-3"}, "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			stack, id, owned := stackOwner(tc.labels)
			if stack != tc.stack || id != tc.id || owned != tc.owned {
				t.Errorf("stackOwner(%v) = %q, %q, %v; want %q, %q, %v", tc.labels, stack, id, owned, tc.stack, tc.id, tc.owned)
			}
		})
	}
}

// TestStackOwnedIDIsConfirmedOnlyForAStackOfTheEnvironment is T-07-146: a label is only a claim. An ID is linked when the environment's
// stacks hold that ID under the name the object says it belongs to.
func TestStackOwnedIDIsConfirmedOnlyForAStackOfTheEnvironment(t *testing.T) {
	t.Parallel()
	known := knownStacks([]stackops.Stack{{ID: "stk-1", Name: "web"}})
	for name, tc := range map[string]struct {
		stack, id, want string
	}{
		"a stack of the environment":          {"web", "stk-1", "stk-1"},
		"an id the environment does not have": {"web", "stk-9", ""},
		"an id under another name":            {"db", "stk-1", ""},
		"no id":                               {"web", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := known.confirm(tc.stack, tc.id); got != tc.want {
				t.Errorf("confirm(%q, %q) = %q, want %q", tc.stack, tc.id, got, tc.want)
			}
		})
	}
	var none stackIDs
	if got := none.confirm("web", "stk-1"); got != "" {
		t.Errorf("confirm with no stacks read = %q, want empty", got)
	}
}

// TestStackOwnedLinksAPendingAdoptedStacksUnlabelledObject: an adopted stack that has not been deployed yet owns the
// services and containers of its name that carry no tierplan.stack.id, or the stale one recorded at adoption, so their
// pages link to it; any other ID under that name is a claim that links to nothing (T-07-146, T-08-68). Once the
// first deploy is done only its own ID links. A secret or a config is linked by its ID alone: the ones an adopted
// stack references are not its own.
func TestStackOwnedLinksAPendingAdoptedStacksUnlabelledObject(t *testing.T) {
	t.Parallel()
	adopted := func(pending bool) stackops.Stack {
		return stackops.Stack{
			ID: "stk-adopted", Name: "shop", Source: stacks.Source{Kind: stacks.SourceAdopted, Ref: "shop"},
			Adoption: stacks.Adoption{Pending: pending, PriorID: "stale-id"},
		}
	}
	managed := stackops.Stack{ID: "stk-1", Name: "web"}
	for name, tc := range map[string]struct {
		stacks          []stackops.Stack
		stack, id       string
		want, wantAsObj string
	}{
		"no ID, pending":                  {[]stackops.Stack{adopted(true)}, "shop", "", "stk-adopted", ""},
		"the stale ID, pending":           {[]stackops.Stack{adopted(true)}, "shop", "stale-id", "stk-adopted", ""},
		"its own ID, pending":             {[]stackops.Stack{adopted(true)}, "shop", "stk-adopted", "stk-adopted", "stk-adopted"},
		"another ID, pending":             {[]stackops.Stack{adopted(true)}, "shop", "stk-other", "", ""},
		"no ID, deployed":                 {[]stackops.Stack{adopted(false)}, "shop", "", "", ""},
		"the stale ID, deployed":          {[]stackops.Stack{adopted(false)}, "shop", "stale-id", "", ""},
		"its own ID, deployed":            {[]stackops.Stack{adopted(false)}, "shop", "stk-adopted", "stk-adopted", "stk-adopted"},
		"no ID, under another name":       {[]stackops.Stack{adopted(true)}, "blog", "", "", ""},
		"its ID under another name":       {[]stackops.Stack{adopted(true)}, "blog", "stk-adopted", "", ""},
		"no ID, a stack TierPlan made":     {[]stackops.Stack{managed}, "web", "", "", ""},
		"the stale ID, a stack it made":   {[]stackops.Stack{managed}, "web", "stale-id", "", ""},
		"its own ID, a stack it made":     {[]stackops.Stack{managed}, "web", "stk-1", "stk-1", "stk-1"},
		"no stack of that name at all":    {nil, "shop", "", "", ""},
		"a stack with an empty name says": {[]stackops.Stack{adopted(true)}, "", "", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			known := knownStacks(tc.stacks)
			if got := known.confirm(tc.stack, tc.id); got != tc.want {
				t.Errorf("confirm(%q, %q) = %q, want %q", tc.stack, tc.id, got, tc.want)
			}
			if got := known.confirmObject(tc.stack, tc.id); got != tc.wantAsObj {
				t.Errorf("confirmObject(%q, %q) = %q, want %q", tc.stack, tc.id, got, tc.wantAsObj)
			}
		})
	}
}

func TestStackOwnedProblemIsA409NamingTheStack(t *testing.T) {
	t.Parallel()
	var se huma.StatusError
	if !errors.As(stackOwnedProblem("shop", ""), &se) {
		t.Fatal("stackOwnedProblem is not a status error")
	}
	if se.GetStatus() != http.StatusConflict || !strings.Contains(se.Error(), "Managed by stack shop. Change it through the stack.") {
		t.Errorf("problem = %d %q, want 409 naming the stack", se.GetStatus(), se.Error())
	}
}

func TestStackOwnedProblemCarriesTheTierPlanStackIDOnlyForAStackTierPlanManages(t *testing.T) {
	t.Parallel()
	errs := func(err error) map[string]string {
		var em *huma.ErrorModel
		if !errors.As(err, &em) {
			t.Fatalf("%v is not an error model", err)
		}
		got := map[string]string{}
		for _, d := range em.Errors {
			got[d.Location] = d.Message
		}
		return got
	}
	if got := errs(stackOwnedProblem("web", "stk-1")); got["stack"] != "web" || got["tierplanStackId"] != "stk-1" {
		t.Errorf("errors = %v, want stack web and tierplanStackId stk-1", got)
	}
	if got := errs(stackOwnedProblem("web", "")); got["stack"] != "web" || len(got) != 1 {
		t.Errorf("errors = %v, want the stack alone for a foreign stack", got)
	}
}
