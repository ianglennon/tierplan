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
		owned  bool
	}{
		"no labels":         {nil, "", false},
		"unrelated labels":  {map[string]string{"app": "web"}, "", false},
		"an empty stack":    {map[string]string{"com.docker.stack.namespace": ""}, "", false},
		"a stack":           {map[string]string{"com.docker.stack.namespace": "shop", "app": "web"}, "shop", true},
		"a compose project": {map[string]string{"com.docker.compose.project": "shop"}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			stack, owned := stackOwner(tc.labels)
			if stack != tc.stack || owned != tc.owned {
				t.Errorf("stackOwner(%v) = %q, %v; want %q, %v", tc.labels, stack, owned, tc.stack, tc.owned)
			}
		})
	}
}

func TestStackOwnedProblemIsA409NamingTheStack(t *testing.T) {
	t.Parallel()
	var se huma.StatusError
	if !errors.As(stackOwnedProblem("shop"), &se) {
		t.Fatal("stackOwnedProblem is not a status error")
	}
	if se.GetStatus() != http.StatusConflict || !strings.Contains(se.Error(), "Managed by stack shop. Change it through the stack.") {
		t.Errorf("problem = %d %q, want 409 naming the stack", se.GetStatus(), se.Error())
	}
}
