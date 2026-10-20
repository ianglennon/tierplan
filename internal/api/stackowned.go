// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
)

// problemStackOwned is the type of the 409 a change to a service a stack owns gets from the
// service pages: such a service is changed through the stack.
const problemStackOwned = "stack_owned"

// stackOwner says which stack an object belongs to, from its labels. It is the one place that decides
// who owns an object: the label docker stack deploy writes (com.docker.stack.namespace), or the one
// Docker Compose writes on a container (com.docker.compose.project), whichever is set, the namespace
// first. A label that is set but empty names no stack. bayplanID is Bayplan's own label
// (bayplan.stack.id) as the object states it: a claim, not a fact, until stackIDs.confirm has seen a
// stack of the environment with that ID and name.
func stackOwner(labels map[string]string) (stack, bayplanID string, owned bool) {
	stack = labels[labelStack]
	if stack == "" {
		stack = labels[labelProject]
	}
	if stack == "" {
		return "", "", false
	}
	return stack, labels[stacks.LabelStackID], true
}

// namespaceOwner is stackOwner as the service operations in swarmops take it.
func namespaceOwner(labels map[string]string) (string, bool) {
	stack, _, owned := stackOwner(labels)
	return stack, owned
}

// stackIDs are the stacks Bayplan holds on one environment, by ID, with the name each goes by.
type stackIDs map[string]string

// loadStackIDs reads the stacks of an environment once, for the rows of one request. A store that is
// missing or cannot be read yields none: no link is drawn and nothing else changes.
func loadStackIDs(ctx context.Context, st *store.Store, envID string, log *slog.Logger) stackIDs {
	if st == nil {
		return nil
	}
	list, err := stackops.Repo{Store: st}.Stacks(ctx, envID)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Error("reading the stacks for their links failed", "environment", envID, "error", err)
		return nil
	}
	out := make(stackIDs, len(list))
	for _, s := range list {
		out[s.ID] = s.Name
	}
	return out
}

// confirm returns the ID when it names a stack of this environment under the name the object says it
// belongs to, else empty (T-07-146): a label anybody can put on an object is never a link target by
// itself, and an ID under another stack's name is a forgery or a stale label.
func (k stackIDs) confirm(stack, id string) string {
	if id == "" || stack == "" {
		return ""
	}
	if name, ok := k[id]; ok && name == stack {
		return id
	}
	return ""
}

// confirmedFor is the Bayplan stack ID of an object that shows stack as its stack, from its labels.
func (k stackIDs) confirmedFor(stack string, labels map[string]string) string {
	_, id, _ := stackOwner(labels)
	return k.confirm(stack, id)
}

// stackOwnedProblem is the 409 a refused change to a stack's service is answered with. The stack is
// named in the detail and in errors, as stack; a stack Bayplan manages is named there too, as
// bayplanStackId, so the SPA can link to it. bayplanID is empty for a stack Bayplan does not manage.
func stackOwnedProblem(stack, bayplanID string) error {
	errs := []*huma.ErrorDetail{{Location: "stack", Message: stack}}
	if bayplanID != "" {
		errs = append(errs, &huma.ErrorDetail{Location: "bayplanStackId", Message: bayplanID})
	}
	return &huma.ErrorModel{
		Type: problemStackOwned, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "Managed by stack " + stack + ". Change it through the stack.",
		Errors: errs,
	}
}
