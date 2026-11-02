// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
)

// problemStackOwned is the type of the 409 a change to a service a stack owns gets from the
// service pages: such a service is changed through the stack.
const problemStackOwned = "stack_owned"

// stackOwner says which stack an object belongs to, from its labels. It is the one place that decides
// who owns an object: the label docker stack deploy writes (com.docker.stack.namespace), or the one
// Docker Compose writes on a container (com.docker.compose.project), whichever is set, the namespace
// first. A label that is set but empty names no stack. tierplanID is TierPlan's own label
// (tierplan.stack.id) as the object states it: a claim, not a fact, until stackIDs.confirm has seen a
// stack of the environment with that ID and name.
func stackOwner(labels map[string]string) (stack, tierplanID string, owned bool) {
	stack = labels[labelStack]
	if stack == "" {
		stack = labels[labelProject]
	}
	if stack == "" {
		return "", "", false
	}
	return stack, labels[stacks.LabelStackID], true
}

// containerStack is the stack or project name a container shows: the one stackOwner reads.
func containerStack(labels map[string]string) string {
	stack, _, _ := stackOwner(labels)
	return stack
}

// namespaceOwner is stackOwner as the service operations in swarmops take it.
func namespaceOwner(labels map[string]string) (string, bool) {
	stack, _, owned := stackOwner(labels)
	return stack, owned
}

// stackIDs are the stacks TierPlan holds on one environment, by name, each with the rule that says which objects are
// its own (stackops.Ownership). A name is unique in an environment, so the name an object states picks the stack.
type stackIDs map[string]stackops.Ownership

// knownStacks are the stacks of an environment as confirm reads them.
func knownStacks(list []stackops.Stack) stackIDs {
	out := make(stackIDs, len(list))
	for _, s := range list {
		out[s.Name] = stackops.OwnershipOf(s)
	}
	return out
}

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
	return knownStacks(list)
}

// confirm returns the ID of the stack of this environment that the object is linked to, else empty (T-07-146): the object
// states the stack's name and carries the stack's own ID, or, while an adopted stack's first deploy is pending, no ID or the
// one recorded at adoption (T-08-68). A label anybody can put on an object is never a link target by itself, and an ID
// under another stack's name is a forgery or a stale label.
func (k stackIDs) confirm(stack, id string) string {
	if own, ok := k[stack]; ok && stack != "" && own.Claims(stack, id) {
		return own.StackID
	}
	return ""
}

// confirmObject is confirm for a secret or a config: the stack's own ID alone links, because a secret or a config an
// adopted stack only references is not its own.
func (k stackIDs) confirmObject(stack, id string) string {
	if own, ok := k[stack]; ok && stack != "" && own.ClaimsObject(stack, id) {
		return own.StackID
	}
	return ""
}

// confirmedFor is the TierPlan stack ID of an object that shows stack as its stack, from its labels.
func (k stackIDs) confirmedFor(stack string, labels map[string]string) string {
	_, id, _ := stackOwner(labels)
	return k.confirm(stack, id)
}

// stackOwnedProblem is the 409 a refused change to a stack's service is answered with. The stack is
// named in the detail and in errors, as stack; a stack TierPlan manages is named there too, as
// tierplanStackId, so the SPA can link to it. tierplanID is empty for a stack TierPlan does not manage.
func stackOwnedProblem(stack, tierplanID string) error {
	errs := []*huma.ErrorDetail{{Location: "stack", Message: stack}}
	if tierplanID != "" {
		errs = append(errs, &huma.ErrorDetail{Location: "tierplanStackId", Message: tierplanID})
	}
	return &huma.ErrorModel{
		Type: problemStackOwned, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "Managed by stack " + stack + ". Change it through the stack.",
		Errors: errs,
	}
}
