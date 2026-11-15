// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"fmt"

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
