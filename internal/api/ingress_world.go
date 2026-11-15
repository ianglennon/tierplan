// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// ingressPolicyFor builds the ingress policy a stack's check judges its labels against: the world of the
// stack's environment (its registered domains, the install-wide reserved names and every claim) plus the
// rule for which other environments a message may name (D-19, D-20). It is the one builder the editor check,
// the review and every deploy gate share, so they all read the same world the same way. The ingress switch
// is not read: the policy applies whether or not ingress will later be switched on (D-18).
//
// Who may see another environment's stack is decided once, from the principal's access read here: an
// environment is visible when the principal's role on it is not rbac.RoleNone. With no principal (a refusal
// recorded where every viewer of the environment reads it, as an automatic deploy does) or no checker, only
// the stack's own environment is visible, so no other environment's names reach the message.
//
// The answer is nil with no error when the server has no ingress store. A read error is returned as it is
// (it carries no secret): the editor check and the review treat it as advisory and report the parser's
// problems without the policy, while a deploy gate refuses on it.
func ingressPolicyFor(ctx context.Context, d StacksDeps, p *rbac.Principal, envID, stackID string) (*stacks.IngressPolicy, error) {
	if d.Ingress == nil {
		return nil, nil
	}
	canView := func(id string) bool { return id == envID }
	if p != nil && d.Checker != nil {
		access, err := d.Checker.Access(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("read the access of the principal for the ingress policy: %w", err)
		}
		canView = func(id string) bool { return access.For(id).Role != rbac.RoleNone }
	}
	world, err := d.Ingress.World(ctx, envID, canView)
	if err != nil {
		return nil, err
	}
	return &stacks.IngressPolicy{StackID: stackID, World: world}, nil
}

// ingressGatePolicy is the policy a deploy gate (a deploy, a roll back, a restore, an update) judges a working copy
// against (D-18). The world is read for every copy, not only for one whose text names an ingress label: a label key
// can be assembled by interpolation (a list-form "${P}.host=..." with P set to the prefix) so that no file or value
// contains the prefix, and the gate must judge it all the same. A read that fails refuses the copy only when its text
// names an ingress label (declaresIngress); otherwise the copy is judged without the policy, so a stack with no ingress
// label is never refused because the ingress store cannot be read. The parser still runs in either case.
func ingressGatePolicy(ctx context.Context, d StacksDeps, p *rbac.Principal, c stacks.WorkingCopy, envID, stackID string) (*stacks.IngressPolicy, error) {
	pol, err := ingressPolicyFor(ctx, d, p, envID, stackID)
	if err != nil {
		if declaresIngress(c) {
			return nil, err
		}
		lg := d.Log
		if lg == nil {
			lg = slog.Default()
		}
		lg.WarnContext(ctx, "the ingress names could not be read; a stack that names no ingress label is judged without them",
			"env", envID, "stack", stackID, "error", err)
		return nil, nil
	}
	return pol, nil
}
