// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// problemStackOwned is the type of the 409 a change to a service a stack owns gets from the
// service pages: such a service is changed through the stack.
const problemStackOwned = "stack_owned"

// stackOwner says which stack a service belongs to, from its labels. It is the one place that
// decides who owns a service: today the label docker stack deploy writes
// (com.docker.stack.namespace), and Bayplan's own stack labels join it when stacks arrive in
// Phase 7. A label that is set but empty names no stack.
func stackOwner(labels map[string]string) (stack string, owned bool) {
	if v := labels[labelStack]; v != "" {
		return v, true
	}
	return "", false
}

// stackOwnedProblem is the 409 a refused change to a stack's service is answered with. The
// stack is named in the detail and in errors, as stack.
func stackOwnedProblem(stack string) error {
	return &huma.ErrorModel{
		Type: problemStackOwned, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "Managed by stack " + stack + ". Change it through the stack.",
		Errors: []*huma.ErrorDetail{{Location: "stack", Message: stack}},
	}
}
