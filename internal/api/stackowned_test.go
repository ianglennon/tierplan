// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func TestStackOwner(t *testing.T) {
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
		"a stack Bayplan manages": {
			map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stk-1"}, "web", "stk-1", true,
		},
		"a compose project": {map[string]string{"com.docker.compose.project": "shop"}, "shop", "", true},
		"a compose project Bayplan manages": {
			map[string]string{"com.docker.compose.project": "web", "bayplan.stack.id": "stk-2"}, "web", "stk-2", true,
		},
		"the namespace before the project": {
			map[string]string{"com.docker.stack.namespace": "a", "com.docker.compose.project": "b"}, "a", "", true,
		},
		"an id with no stack": {map[string]string{"bayplan.stack.id": "stk-3"}, "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			stack, id, owned := stackOwner(tc.labels)
			if stack != tc.stack || id != tc.id || owned != tc.owned {
				t.Errorf("stackOwner(%v) = %q, %q, %v; want %q, %q, %v", tc.labels, stack, id, owned, tc.stack, tc.id, tc.owned)
			}
		})
	}
}

// TestConfirmedStackID is T-07-146: a label is only a claim. An ID is linked when the environment's
// stacks hold that ID under the name the object says it belongs to.
func TestConfirmedStackID(t *testing.T) {
	t.Parallel()
	known := stackIDs{"stk-1": "web"}
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

func TestStackOwnedProblemCarriesTheBayplanStackIDOnlyForAStackBayplanManages(t *testing.T) {
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
	if got := errs(stackOwnedProblem("web", "stk-1")); got["stack"] != "web" || got["bayplanStackId"] != "stk-1" {
		t.Errorf("errors = %v, want stack web and bayplanStackId stk-1", got)
	}
	if got := errs(stackOwnedProblem("web", "")); got["stack"] != "web" || len(got) != 1 {
		t.Errorf("errors = %v, want the stack alone for a foreign stack", got)
	}
}
