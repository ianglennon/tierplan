// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// CodeIngressRefused ends the check step of a deploy whose hostname claims the store refused: a name another stack
// holds, a reserved one, or one no registered domain covers (D-15). Nothing was changed.
const CodeIngressRefused = "ingress_refused"

// A certificate is never read here: a route whose certificate is not issued yet deploys, and its claim is stored
// (DEP-05). Only the policy of the store can refuse a deploy for ingress.

// claimWrite is what writeClaims did, for the deploy that called it.
type claimWrite struct {
	// Written says the claims now are what cw.Decls say (or already were), so a deploy that does not end well has
	// to put them back.
	Written bool
	// Refused says the store refused the plan; Reason is its first sentence, with the store's redaction already
	// applied and never empty.
	Refused bool
	Reason  string
	// Prior are the routes the stack held just before the write, the exact state a failed deploy puts back.
	Prior []ingress.Declaration
}

// refusedFallback is the reason given for a refused plan whose first problem carries no sentence.
const refusedFallback = "A hostname the stack asks for cannot be claimed."

// writeClaims stores the claims of a stack as cw.Decls say, inside the environment's operation lock the deploy
// holds. The store judges the policy again in its own write transaction, so a name another deploy took since the
// review is refused here. The claims the stack holds are read first and kept in the answer, so a failure of the
// deploy can put back exactly those, whatever its last good revision says (D-16). A nil Ingress or a nil cw does
// nothing. An error means nothing was written.
func (d Deps) writeClaims(ctx context.Context, envID, stackID string, cw *ClaimWrite) (claimWrite, error) {
	if d.Ingress == nil || cw == nil {
		return claimWrite{}, nil
	}
	held, err := d.Ingress.HeldRoutes(ctx, envID, stackID)
	if err != nil {
		return claimWrite{}, fmt.Errorf("read the claims the stack holds: %w", err)
	}
	prior := declarationsOf(held)
	_, err = d.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
		EnvID: envID, StackID: stackID, Actor: cw.Actor.ID, Decls: cw.Decls, CanView: cw.CanView,
	}, cw.Actor)
	var refused *ingressstore.RefusedError
	switch {
	case err == nil:
		return claimWrite{Written: true, Prior: prior}, nil
	case errors.As(err, &refused):
		reason := refusalReason(refused)
		if reason == "" {
			reason = refusedFallback
		}
		return claimWrite{Refused: true, Reason: reason}, nil
	default:
		return claimWrite{}, err
	}
}

// declarationsOf are the routes as declarations, which is what the store takes back.
func declarationsOf(routes []ingress.Route) []ingress.Declaration {
	out := make([]ingress.Declaration, 0, len(routes))
	for _, r := range routes {
		out = append(out, ingress.Declaration{
			Service: r.Service, Name: r.Name, Host: r.Hostname, PathPrefix: r.PathPrefix, Port: r.Port, Scheme: r.Scheme, Strip: r.Strip,
		})
	}
	return out
}

// refusalReason is the sentence of the first error a refused plan found.
func refusalReason(e *ingressstore.RefusedError) string {
	for _, p := range e.Problems {
		if p.Severity == ingress.SeverityError {
			return p.Message
		}
	}
	if len(e.Problems) > 0 {
		return e.Problems[0].Message
	}
	return ""
}

// revertClaims puts the claims of a stack back as they were before its deploy wrote them (D-16, D-45): restore is
// the snapshot writeClaims took. It runs in the deploy's finishing seam, before the caller is told and with the lock
// still held, so a hostname only the failed revision asked for is free again at once. It does not stop for a cancel.
// An empty restore releases every claim; a restore the store refuses (the name was taken since) falls back to a
// release. A failure is logged and returned: the deploy's own result stands, but the caller must say the claims were
// not put back.
func (d Deps) revertClaims(ctx context.Context, envID, stackID string, actor audit.Actor, canView func(string) bool, restore []ingress.Declaration) error {
	if d.Ingress == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	if len(restore) > 0 {
		_, err := d.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
			EnvID: envID, StackID: stackID, Actor: actor.ID, Decls: restore, CanView: canView,
		}, actor)
		if err == nil {
			return nil
		}
		var refused *ingressstore.RefusedError
		if !errors.As(err, &refused) {
			d.log().Error("could not put a stack's hostname claims back after a failed deploy", "environment", envID, "stack", stackID, "error", err)
			return err
		}
		d.log().Warn("a stack's earlier hostname claims are refused now, so all its claims are released", "environment", envID, "stack", stackID, "reason", refusalReason(refused))
	}
	if err := d.Ingress.ReleaseStack(ctx, envID, stackID, actor); err != nil {
		d.log().Error("could not release a stack's hostname claims after a failed deploy", "environment", envID, "stack", stackID, "error", err)
		return err
	}
	return nil
}

// ClaimsRevertFailedNote is the sentence a failed deploy adds to its output when its hostname claims could not be put
// back. The deploy's own failure code stands; this says that the names it claimed are still held.
const ClaimsRevertFailedNote = "The hostnames this deploy claimed could not be put back, so they stay claimed. Deploy this stack again, or roll it back, to set them right."

// noteClaimsRevertFailed records on the deploy that its claims were not put back: the result, a line in the job's
// output and a mark on the last item (which the deploy's audit row repeats as claimsRevert).
func noteClaimsRevertFailed(res *DeployResult, prog *progress, final *itemEnd) {
	res.ClaimsRevertFailed = true
	prog.line(ClaimsRevertFailedNote)
	out := make(map[string]string, len(final.result)+1)
	for k, v := range final.result {
		out[k] = v
	}
	out["claimsRevert"] = "failed"
	final.result = out
}

// LastGoodDeclarations are the routes the stack's newest good revision declares, with true; a stack with no good
// revision gives nil and false. It is what ClaimWrite.Restore is made of.
func LastGoodDeclarations(ctx context.Context, repo Repo, st Stack) ([]ingress.Declaration, bool, error) {
	snap, ok, err := repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !ok {
		return nil, false, err
	}
	// ingress: unattached, only the declarations are read
	checked := stacks.Check(ctx, stacks.CheckInput{Name: st.Name, Target: st.Target, Copy: snap.Copy})
	if checked.Loaded == nil {
		return nil, false, fmt.Errorf("the last good revision of stack %q does not load", st.Name)
	}
	return stacks.IngressDeclarations(checked.Loaded.Project, st.Target), true, nil
}

// RestoreInterruptedClaims puts the claims of every stack whose deploy, roll back or update was interrupted back as its
// last good revision declared them (D-45). The server calls it once at start, after MarkInterrupted: a crash in the
// middle of a deploy runs no revert, so the claims of the failed revision would stay. The rule is revertClaims': the last
// good revision's routes are stored (there is no snapshot of what was held, the process that took it is gone), a stack
// with no good revision releases every claim, and a restore the store refuses falls back to a release. Each claim write is audited by actor through the Ingress, and one stack's failure is collected
// while the others go on. A stack that no longer exists is skipped; a stack whose last good revision cannot be read or
// loaded is left as it is, and reported, because releasing the names of a stack that may be running would be worse than
// holding them a while longer.
func RestoreInterruptedClaims(ctx context.Context, repo Repo, in Ingress, interrupted []InterruptedStack, actor audit.Actor) error {
	if in == nil {
		return nil
	}
	d := Deps{Ingress: in}
	var errs []error
	for _, s := range interrupted {
		st, err := repo.Stack(ctx, s.EnvID, s.StackID)
		if errors.Is(err, ErrStackNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("read stack %s: %w", s.StackID, err))
			continue
		}
		restore, _, err := LastGoodDeclarations(ctx, repo, st)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the last good routes of stack %s: %w", st.Name, err))
			continue
		}
		if err := d.revertClaims(ctx, s.EnvID, s.StackID, actor, nil, restore); err != nil {
			errs = append(errs, fmt.Errorf("put back the claims of stack %s: %w", st.Name, err))
		}
	}
	return errors.Join(errs...)
}
