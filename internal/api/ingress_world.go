// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
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
	canView, err := ingressCanView(ctx, d, p, envID)
	if err != nil {
		return nil, err
	}
	world, err := d.Ingress.World(ctx, envID, canView)
	if err != nil {
		return nil, err
	}
	return &stacks.IngressPolicy{StackID: stackID, World: world}, nil
}

// ingressCanView says which environments a message about a held name may name: the stack's own, and, for a principal
// with a role on another, that one too. With no principal, or no checker, only the stack's own environment is visible.
func ingressCanView(ctx context.Context, d StacksDeps, p *rbac.Principal, envID string) (func(string) bool, error) {
	canView := func(id string) bool { return id == envID }
	if p != nil && d.Checker != nil {
		access, err := d.Checker.Access(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("read the access of the principal for the ingress policy: %w", err)
		}
		canView = func(id string) bool { return access.For(id).Role != rbac.RoleNone }
	}
	return canView, nil
}

// claimWriteFor is the claim write a deploy of stack st carries (D-15, D-16, D-17): the routes of the revision it
// deploys (project, loaded for that revision; nil means the routes are unchanged, as an Update images is, and the last
// good revision's stand) and, for a deploy that fails, the routes of the last good revision to put back. p is who
// acts; viewer is who a refusal may be worded for (nil for an automatic deploy, whose refusal is read by every viewer
// of the environment, so it names only the stack's own environment, D-20). The answer is nil when the server has no
// ingress store, and also when neither the deployed revision nor the last good one declares a route (the stack holds no
// claim, so there is nothing to write or put back). It must be called holding the environment's operation lock and before the new revision exists, so
// the last good revision it reads is the one that runs.
//
// A last good revision that cannot be read or loaded is treated as none (a failed deploy then releases every claim)
// and logged: it must never stop the deploy that would replace it.
func claimWriteFor(ctx context.Context, d StacksDeps, p, viewer *rbac.Principal, st stackops.Stack, project *types.Project) (*stackops.ClaimWrite, error) {
	if d.Ingress == nil {
		return nil, nil
	}
	restore, hasGood, err := stackops.LastGoodDeclarations(ctx, d.repo(), st)
	if err != nil {
		lg := d.Log
		if lg == nil {
			lg = slog.Default()
		}
		lg.WarnContext(ctx, "the hostname claims of a stack's last good revision could not be read; a failed deploy releases them all",
			"env", st.EnvID, "stack", st.ID, "error", err)
		restore, hasGood = nil, false
	}
	decls := restore
	if project != nil {
		decls = stacks.IngressDeclarations(project, st.Target)
	}
	// A stack that declares no route and whose last good revision declared none holds no claim, so its deploy has nothing to
	// write and never needs the ingress store: a stack with no ingress label is not refused because that store cannot be read.
	if len(decls) == 0 && len(restore) == 0 {
		return nil, nil
	}
	canView, err := ingressCanView(ctx, d, viewer, st.EnvID)
	if err != nil {
		return nil, err
	}
	actor := p.ID
	if p.Type == rbac.PrincipalSystem && !strings.HasPrefix(actor, "system:") {
		actor = "system:" + actor
	}
	return &stackops.ClaimWrite{Decls: decls, Restore: restore, HasGood: hasGood, Actor: actor, CanView: canView}, nil
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

// ingressOps adapts the ingress store to stackops.Ingress: the one door the stack pipeline asks about ingress by
// (D-18). It adds the audit rows of the claim writes, in the transaction of the change.
type ingressOps struct{ d StacksDeps }

// ingressOps is the adaptor of the deps' ingress store, or nil (no ingress) when the server has none. The nil is
// the interface's own, not a nil adaptor in an interface.
func (d StacksDeps) ingressOps() stackops.Ingress {
	if d.Ingress == nil {
		return nil
	}
	return ingressOps{d: d}
}

// ingressClaimTarget is the audit target type of a claim write: the stack that holds the claim.
const ingressClaimTarget = "stack"

// AttachFor is the attachment of a swarm target of an environment whose ingress is switched on, and nil otherwise
// (D-13, D-19). A standalone target is never attached (the standalone attach is Phase 25).
func (o ingressOps) AttachFor(ctx context.Context, envID string, target stacks.Target) (*stacks.IngressAttach, error) {
	if target != stacks.TargetSwarm {
		return nil, nil
	}
	set, err := o.d.Ingress.Settings(ctx, envID)
	if err != nil {
		return nil, err
	}
	if !set.Enabled {
		return nil, nil
	}
	return &stacks.IngressAttach{Network: ingress.NetworkName}, nil
}

// World reads the ingress world of the environment.
func (o ingressOps) World(ctx context.Context, envID string, canView func(string) bool) (ingress.World, error) {
	return o.d.Ingress.World(ctx, envID, canView)
}

// claimActor is the audit actor of a claim write whose caller named itself by a string: a "system:" name is a
// system actor, anything else a user.
func claimActor(actor string) audit.Actor {
	if strings.HasPrefix(actor, "system:") {
		return audit.Actor{Type: audit.ActorSystem, ID: actor, Name: actor}
	}
	return audit.Actor{Type: audit.ActorUser, ID: actor, Name: actor}
}

// claimHook records one audit row of a claim write in the write's own transaction. A nil Audit writes none.
func (o ingressOps) claimHook(action, envID, stackID, actor string) ingressstore.ClaimHook {
	if o.d.Audit == nil {
		return nil
	}
	return func(ctx context.Context, tx *sql.Tx, p ingress.Plan) error {
		return o.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: claimActor(actor), Action: action, TargetType: ingressClaimTarget, TargetID: stackID, EnvID: envID,
			Result: audit.ResultSuccess, Details: map[string]string{"hostnames": strconv.Itoa(len(p.Claims))},
		})
	}
}

// ReplaceStackRoutes stores the stack's claims and audits a change as ingress.claim_replaced.
func (o ingressOps) ReplaceStackRoutes(ctx context.Context, in ingressstore.ReplaceInput) (ingress.Plan, error) {
	if in.Hook == nil {
		in.Hook = o.claimHook(audit.ActionIngressClaimReplaced, in.EnvID, in.StackID, in.Actor)
	}
	return o.d.Ingress.ReplaceStackRoutes(ctx, in)
}

// ReleaseStack lets go of the stack's claims and audits a change as ingress.claim_released.
func (o ingressOps) ReleaseStack(ctx context.Context, envID, stackID, actor string) error {
	return o.d.Ingress.ReleaseStack(ctx, envID, stackID, actor, o.claimHook(audit.ActionIngressClaimReleased, envID, stackID, actor))
}

// Notify wakes the reconciler of the environment.
func (o ingressOps) Notify(envID string) { o.d.Ingress.Notify(envID) }

// ingressAttachFor is the attachment a swarm conversion of a stack in envID carries, nil when ingress is off or the
// server has no ingress store. A read that fails is an error: the caller refuses instead of converting without the
// attachment.
func ingressAttachFor(ctx context.Context, d StacksDeps, envID string, target stacks.Target) (*stacks.IngressAttach, error) {
	ops := d.ingressOps()
	if ops == nil {
		return nil, nil
	}
	return ops.AttachFor(ctx, envID, target)
}
