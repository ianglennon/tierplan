// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// CodeIngressRefused ends the check step of a deploy whose hostname claims the store refused: a name another stack
// holds, a reserved one, or one no registered domain covers (D-15). Nothing was changed.
const CodeIngressRefused = "ingress_refused"

// A certificate is never read here: a route whose certificate is not issued yet deploys, and its claim is stored
// (DEP-05). Only the policy of the store can refuse a deploy for ingress.

// writeClaims stores the claims of a stack as cw.Decls say, inside the environment's operation lock the deploy
// holds. The store judges the policy again in its own write transaction, so a name another deploy took since the
// review is refused here. written reports that the claims now are what cw.Decls say (or already were), so a
// failed deploy has to put them back; refusal is the first reason of a refused plan, with the store's redaction
// already applied. A nil Ingress or a nil cw does nothing.
func (d Deps) writeClaims(ctx context.Context, envID, stackID string, cw *ClaimWrite) (written bool, refusal string, err error) {
	if d.Ingress == nil || cw == nil {
		return false, "", nil
	}
	_, err = d.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
		EnvID: envID, StackID: stackID, Actor: cw.Actor, Decls: cw.Decls, CanView: cw.CanView,
	})
	var refused *ingressstore.RefusedError
	switch {
	case err == nil:
		return true, "", nil
	case errors.As(err, &refused):
		return false, refusalReason(refused), nil
	default:
		return false, "", err
	}
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

// revertClaims puts the claims of a stack back as its last good revision declared them (D-16, D-45), for a deploy
// that wrote them and did not end well. It runs in the deploy's finishing seam, before the caller is told and with
// the lock still held, so a hostname only the failed revision asked for is free again at once. It does not stop for
// a cancel. With no good revision every claim is released; a restore the store refuses (the name was taken since)
// falls back to a release. An error is logged and never returned: the deploy's own result stands.
func (d Deps) revertClaims(ctx context.Context, envID, stackID string, cw *ClaimWrite) {
	if d.Ingress == nil || cw == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if cw.HasGood {
		_, err := d.Ingress.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{
			EnvID: envID, StackID: stackID, Actor: cw.Actor, Decls: cw.Restore, CanView: cw.CanView,
		})
		if err == nil {
			return
		}
		var refused *ingressstore.RefusedError
		if !errors.As(err, &refused) {
			d.log().Error("could not put a stack's hostname claims back after a failed deploy", "environment", envID, "stack", stackID, "error", err)
			return
		}
		d.log().Warn("a stack's last good hostname claims are refused now, so all its claims are released", "environment", envID, "stack", stackID, "reason", refusalReason(refused))
	}
	if err := d.Ingress.ReleaseStack(ctx, envID, stackID, cw.Actor); err != nil {
		d.log().Error("could not release a stack's hostname claims after a failed deploy", "environment", envID, "stack", stackID, "error", err)
	}
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
// good revision's routes are stored, a stack with no good revision releases every claim, and a restore the store refuses
// falls back to a release. Each claim write is audited by actor through the Ingress, and one stack's failure is collected
// while the others go on. A stack that no longer exists is skipped; a stack whose last good revision cannot be read or
// loaded is left as it is, and reported, because releasing the names of a stack that may be running would be worse than
// holding them a while longer.
func RestoreInterruptedClaims(ctx context.Context, repo Repo, in Ingress, interrupted []InterruptedStack, actor string) error {
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
		restore, hasGood, err := LastGoodDeclarations(ctx, repo, st)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the last good routes of stack %s: %w", st.Name, err))
			continue
		}
		d.revertClaims(ctx, s.EnvID, s.StackID, &ClaimWrite{Restore: restore, HasGood: hasGood, Actor: actor})
	}
	return errors.Join(errs...)
}
