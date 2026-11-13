// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
)

// The audit target of every HTTPS row (Phase 17, SET-04): the instance's HTTPS setup, not a resource.
const httpsAuditTarget = "https"

// auditInternal is the reason of a refusal no problem type names: an internal failure.
const auditInternal = "internal"

// audit writes the row of one admin request on the HTTPS page: who pressed it, from where, and what came of it (D-09,
// D-11). A nil err is a success row. A non-nil err is the problem the request was answered with, written as a failure
// row whose reason is the problem type or, for a CA failure, its class (reasonOf). Nothing the admin typed and nothing
// the CA said reaches the row (T-17-10): the details are the closed facts the caller builds.
//
// A nil d.Audit writes nothing, so the API fixtures that do not read the log are unchanged.
func (d HTTPSDeps) audit(ctx context.Context, ua, action string, err error, details map[string]string) error {
	if d.Audit == nil {
		return nil
	}
	e := audit.Entry{
		Actor:      httpx.PrincipalFrom(ctx).AuditActor(),
		Action:     action,
		TargetType: httpsAuditTarget,
		TargetID:   httpsAuditTarget,
		Result:     audit.ResultSuccess,
		IP:         httpx.ClientIP(ctx),
		UserAgent:  ua,
		Details:    details,
	}
	if err != nil {
		e.Result = audit.ResultFailure
		e.Reason = reasonOf(err)
	}
	return d.Audit.Record(ctx, e)
}

// reasonOf is the closed-vocabulary reason of a refused request: the problem type it was answered with, or the class
// of a CA failure. It never reads a problem's Detail or errors[].Value, which can echo what the admin typed or what the
// CA said. A 500 and anything that is not a problem is "internal".
func reasonOf(err error) string {
	var m *huma.ErrorModel
	if !errors.As(err, &m) { // also unwraps the huma.ErrorWithHeaders that blockedProblem returns
		return auditInternal
	}
	if m.Type == problemACMECAError {
		if len(m.Errors) > 0 && m.Errors[0] != nil && m.Errors[0].Message != "" {
			return m.Errors[0].Message
		}
		return m.Type
	}
	if m.Status >= 500 || m.Type == "" || m.Type == "about:blank" {
		return auditInternal
	}
	return m.Type
}

// refusalDetails is what a refused run request says about why, read from the manager's own error: the codes of a
// preflight refusal, or the instant the attempts reopen. Nothing else is taken from the error.
func refusalDetails(err error) map[string]string {
	var refused *acme.RefusedError
	var blocked *acme.BlockedError
	switch {
	case errors.As(err, &refused):
		codes := make([]string, 0, len(refused.Refusals))
		for _, r := range refused.Refusals {
			codes = append(codes, r.Code)
		}
		return map[string]string{"refusals": strings.Join(codes, ",")}
	case errors.As(err, &blocked):
		return map[string]string{"until": blocked.Until.UTC().Format(time.RFC3339)}
	}
	return nil
}

// runAuditor writes the row of an Issue or a Renew now press. The Manager calls its started hook (acme.WithRunStarted)
// once, synchronously, when the run is accepted and before its body does anything, so the request row precedes the
// run's outcome row even for a run that fails at its first CA call. Whatever the hook did is kept for the handler.
type runAuditor struct {
	d      HTTPSDeps
	ua     string
	action string
	// hooked is true once the hook ran; err is its audit write error.
	hooked bool
	err    error
}

// started is the hook for acme.WithRunStarted: it writes the success row of the run the Manager has just accepted.
// ctx is the request's, whose principal, address and cancellation the row takes.
func (a *runAuditor) started(ctx context.Context) func(acme.RunStart) {
	return func(rs acme.RunStart) {
		a.hooked = true
		a.err = a.d.audit(ctx, a.ua, a.action, nil, map[string]string{"job": rs.JobID, "reused": "false"})
	}
}

// after writes the one row the hook did not: a reused certificate, or a refusal (err is the manager's error, problem
// the problem the handler maps it to). It returns the audit write error, which the handler answers with a 500. A
// started run has its row from the hook already.
func (a *runAuditor) after(ctx context.Context, rs acme.RunStart, err, problem error) error {
	switch {
	case a.hooked:
		return a.err
	case err != nil:
		return a.d.audit(ctx, a.ua, a.action, problem, refusalDetails(err))
	case rs.Reused:
		return a.d.audit(ctx, a.ua, a.action, nil, map[string]string{"reused": "true", "valid_until": rs.ValidUntil.UTC().Format(time.RFC3339)})
	}
	return nil
}

// registeredDetails are the facts of a registered account: the contact it was registered with (left out when it has
// none), that the terms were agreed, the CA directory and that an admin did it. The contact is the address the
// account holds, never the text of a refused request.
func (d HTTPSDeps) registeredDetails(ctx context.Context, contact string) map[string]string {
	details := map[string]string{"terms_agreed": "true", "trigger": "admin"}
	if contact != "" {
		details["contact"] = contact
	}
	if st, err := d.ACME.Status(ctx); err == nil && st.DirectoryURL != "" {
		details["directory"] = st.DirectoryURL
	}
	return details
}
