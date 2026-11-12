// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The problem types the run actions add to the Let's Encrypt routes.
const (
	problemACMERefused     = "acme_refused"
	problemACMENoAccount   = "acme_no_account"
	problemACMEBlocked     = "acme_blocked"
	problemACMENotActive   = "acme_not_active"
	problemACMENotPaused   = "acme_not_paused"
	problemACMERunNotFound = "acme_run_not_found"
)

// HTTPSACMERunStartBody is what an issue or a renewal did: started a run, or found the certificate
// already in place.
type HTTPSACMERunStartBody struct {
	JobID      string     `json:"jobId,omitempty" doc:"The run's job; follow it on GET /api/https/acme/runs/{jobId}/stream. Left out when reused"`
	Reused     bool       `json:"reused" doc:"True when the stored certificate already covers the names and is not yet due for renewal, so no order was placed"`
	ValidUntil *time.Time `json:"validUntil,omitempty" doc:"When that certificate expires; set only when reused"`
}

// HTTPSACMERunStartOutput is the answer of issue and renew: 202 with a run, or 200 when reused.
type HTTPSACMERunStartOutput struct {
	Status       int
	CacheControl string `header:"Cache-Control"`
	Body         HTTPSACMERunStartBody
}

// actorOf names who an action is for in the manager's log: "type:id" of the principal.
func actorOf(ctx context.Context) string {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return ""
	}
	return string(p.Type) + ":" + p.ID
}

// runStartOutput maps a RunStart onto the answer.
func runStartOutput(rs acme.RunStart) *HTTPSACMERunStartOutput {
	if rs.Reused {
		until := rs.ValidUntil.UTC()
		return &HTTPSACMERunStartOutput{
			Status: http.StatusOK, CacheControl: "no-store",
			Body: HTTPSACMERunStartBody{Reused: true, ValidUntil: &until},
		}
	}
	return &HTTPSACMERunStartOutput{Status: http.StatusAccepted, CacheControl: "no-store", Body: HTTPSACMERunStartBody{JobID: rs.JobID}}
}

// HTTPSACMEActionInput is the input of the run actions: nothing the admin sends, only the User-Agent the audit row
// records. The hidden header leaves the published contract unchanged.
type HTTPSACMEActionInput struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// auditFailed answers a request whose audit row could not be written. The state change the request asked for may
// already have happened (a run started, the pause lifted), which the 500 does not undo; the row is the record, so a
// request that cannot be recorded is not reported as a clean success.
func auditFailed(d HTTPSDeps, err error) error {
	return acmeInternal(d.Log, "audit the Let's Encrypt request", err)
}

// registerHTTPSACMERuns wires the run actions: issue, renew now, resume and stop. Each needs
// settings.write (D-10); the stream that follows a run is in https_stream.go.
func registerHTTPSACMERuns(r *httpx.Router, d HTTPSDeps) {
	write := rbac.Requirement{Perm: rbac.PermSettingsWrite, Scope: rbac.ScopeGlobal}
	problems := "409 " + problemACMEPaused + " after a restore, 409 " + problemACMECertFiles + " while certificate files are set, 409 " +
		problemACMERunActive + " while a run is in progress, 429 " + problemACMEBlocked +
		" while the CA has asked TierPlan to wait or three runs failed within the hour (reason told_to_wait or failure_guard, the instant it reopens in errors[0].value, and a Retry-After header in seconds)"

	httpx.Register(r, huma.Operation{
		OperationID: "issueACMECertificate",
		Method:      http.MethodPost,
		Path:        "/api/https/acme/issue",
		Summary:     "Get a Let's Encrypt certificate for the public address",
		Description: "Orders a certificate for the names the public address and the agent address say now, in the background. " +
			"202 with a job id to follow on the run stream; 200 reused with the certificate's expiry when it already covers exactly these names and is not yet due, in which case nothing is ordered " +
			"(and Let's Encrypt is served again if it had been stopped). 422 " + problemACMERefused + " lists every refusal with its code and data (errors[].message is the code, errors[].value the data) and nothing reaches the CA; " +
			"409 " + problemACMENoAccount + " before an account is registered; 409 " + problemACMELocked + " while TIERPLAN_ACME=off fixes Let's Encrypt off (the variable's name is the detail; nothing is ordered). " + problems + ". Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *HTTPSACMEActionInput) (*HTTPSACMERunStartOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		ra := &runAuditor{d: d, ua: in.UserAgent, action: audit.ActionHTTPSIssueRequested}
		rs, err := d.ACME.Issue(ra.context(ctx), actorOf(ctx))
		var problem error
		if err != nil {
			problem = acmeProblem(d.Log, "start the Let's Encrypt issue", err)
		}
		if aerr := ra.after(ctx, rs, err, problem); aerr != nil {
			return nil, auditFailed(d, aerr)
		}
		if problem != nil {
			return nil, problem
		}
		return runStartOutput(rs), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "renewACMECertificate",
		Method:      http.MethodPost,
		Path:        "/api/https/acme/renew",
		Summary:     "Renew the Let's Encrypt certificate now",
		Description: "Renews the certificate at once, for the names it already carries, and names it as the one it replaces when the CA supports that. 202 with a job id to follow on the run stream. " +
			"409 " + problemACMENotActive + " when Let's Encrypt is not in use or there is no certificate; 409 " + problemACMENoAccount + " before an account is registered; 409 " + problemACMELocked + " while TIERPLAN_ACME=off fixes Let's Encrypt off. " + problems + ". Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *HTTPSACMEActionInput) (*HTTPSACMERunStartOutput, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		ra := &runAuditor{d: d, ua: in.UserAgent, action: audit.ActionHTTPSRenewRequested}
		rs, err := d.ACME.RenewNow(ra.context(ctx), actorOf(ctx))
		var problem error
		if err != nil {
			problem = acmeProblem(d.Log, "start the Let's Encrypt renewal", err)
		}
		if aerr := ra.after(ctx, rs, err, problem); aerr != nil {
			return nil, auditFailed(d, aerr)
		}
		if problem != nil {
			return nil, problem
		}
		return runStartOutput(rs), nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "resumeACME",
		Method:        http.MethodPost,
		Path:          "/api/https/acme/resume",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Resume Let's Encrypt after a restore",
		Description: "Lifts the pause a restore puts on renewals, once the person has seen what was restored. 204. " +
			"409 " + problemACMENotPaused + " when nothing is paused; 409 " + problemACMECertFiles + " while certificate files are set; 409 " + problemACMERunActive + " while a run is in progress. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *HTTPSACMEActionInput) (*struct{}, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		return nil, d.simpleAction(ctx, in.UserAgent, audit.ActionHTTPSResumed, "resume Let's Encrypt", d.ACME.Resume(ctx, actorOf(ctx)))
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "stopACME",
		Method:        http.MethodPost,
		Path:          "/api/https/acme/stop",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Stop using Let's Encrypt",
		Description: "Goes back to the self-signed certificate and stops renewals. The account and the stored certificate are kept, so issuing again for the same names is one step and no order. 204. " +
			"409 " + problemACMENotActive + " when Let's Encrypt is not in use; 409 " + problemACMECertFiles + " while certificate files are set; 409 " + problemACMERunActive + " while a run is in progress; 409 " + problemACMELocked +
			" while TIERPLAN_ACME is set, whichever value it has; 409 " + problemHSTSOn + " while HSTS is on, because browsers that hold the policy would refuse the self-signed certificate: turn HSTS off first. Needs settings.write.",
		Tags: []string{"https"},
	}, write, func(ctx context.Context, in *HTTPSACMEActionInput) (*struct{}, error) {
		if d.ACME == nil {
			return nil, acmeUnavailable()
		}
		return nil, d.simpleAction(ctx, in.UserAgent, audit.ActionHTTPSStopped, "stop using Let's Encrypt", d.ACME.Stop(ctx, actorOf(ctx)))
	})
}

// simpleAction records an action that starts no run (resume, stop) and answers it: the row of the action, as a failure
// row when the manager refused it, then the mapped problem or the audit failure.
func (d HTTPSDeps) simpleAction(ctx context.Context, ua, action, what string, err error) error {
	var problem error
	if err != nil {
		problem = acmeProblem(d.Log, what, err)
	}
	if aerr := d.audit(ctx, ua, action, problem, nil); aerr != nil {
		return auditFailed(d, aerr)
	}
	return problem
}

// runProblem maps the errors only the run actions return; it is nil for any other error.
func runProblem(err error) error {
	var refused *acme.RefusedError
	var blocked *acme.BlockedError
	switch {
	case errors.Is(err, acme.ErrNoAccount):
		return httpx.Problem(http.StatusConflict, problemACMENoAccount, "No Let's Encrypt account is registered yet. Register one first.")
	case errors.Is(err, acme.ErrNotActive):
		return httpx.Problem(http.StatusConflict, problemACMENotActive, "Let's Encrypt is not in use.")
	case errors.Is(err, acme.ErrNotPaused):
		return httpx.Problem(http.StatusConflict, problemACMENotPaused, "Let's Encrypt is not paused.")
	case errors.As(err, &refused):
		return refusedProblem(refused)
	case errors.As(err, &blocked):
		return blockedProblem(blocked)
	}
	return nil
}

// refusedProblem is the 422 listing every refusal. Each error carries the code as its message and the
// data the page words it from as its value; the detail is one English sentence for a person reading
// the response.
func refusedProblem(e *acme.RefusedError) error {
	details := make([]*huma.ErrorDetail, 0, len(e.Refusals))
	reasons := make([]string, 0, len(e.Refusals))
	for _, r := range e.Refusals {
		details = append(details, &huma.ErrorDetail{Location: "refusals", Message: r.Code, Value: refusalValue(r)})
		reasons = append(reasons, refusalReason(r))
	}
	return &huma.ErrorModel{
		Type: problemACMERefused, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
		Detail: "A certificate cannot be ordered yet: " + strings.Join(reasons, "; ") + ".",
		Errors: details,
	}
}

// refusalValue is a refusal's data as the status body shows it.
func refusalValue(r acme.Refusal) HTTPSACMERefusalBody {
	return HTTPSACMERefusalBody{Code: r.Code, Host: r.Host, Path: r.Path, Count: r.Count, Hosts: append([]string(nil), r.Hosts...)}
}

// refusalReason is one refusal in one English phrase.
func refusalReason(r acme.Refusal) string {
	switch r.Code {
	case acme.RefuseNamesIP:
		return fmt.Sprintf("the public address %q is an IP address, and Let's Encrypt only issues for host names", r.Host)
	case acme.RefuseAgentURLIP:
		return fmt.Sprintf("the agent address %q is an IP address, and Let's Encrypt only issues for host names", r.Host)
	case acme.RefuseNamesWildcard:
		return fmt.Sprintf("%q is a wildcard, which needs a challenge TierPlan does not use", r.Host)
	case acme.RefuseNamesNotPublic:
		return fmt.Sprintf("%q is not a public name Let's Encrypt can reach", r.Host)
	case acme.RefuseBasePath:
		return fmt.Sprintf("TierPlan is served under the path %q, so the challenge on /.well-known cannot reach it", r.Path)
	case acme.RefuseAgentDialHost:
		return fmt.Sprintf("%d connected agents dial a host the certificate would not cover (%s)", r.Count, strings.Join(r.Hosts, ", "))
	}
	return "the order is refused (" + r.Code + ")"
}

// blockedProblem is the 429 with the instant a manual attempt is allowed again. The reason is the
// first error's message and the instant, in RFC 3339, its value.
func blockedProblem(e *acme.BlockedError) error {
	until := e.Until.UTC()
	detail := "Let's Encrypt asked TierPlan to wait before another attempt."
	if e.Reason == acme.BlockFailureGuard {
		detail = "Three certificate runs failed within the hour, so new attempts are held back."
	}
	detail += " Try again after " + until.Format(time.RFC3339) + "."
	secs := int64(math.Ceil(e.Wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return huma.ErrorWithHeaders(&huma.ErrorModel{
		Type: problemACMEBlocked, Title: http.StatusText(http.StatusTooManyRequests), Status: http.StatusTooManyRequests, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: "reason", Message: e.Reason, Value: until.Format(time.RFC3339)}},
	}, http.Header{"Retry-After": {strconv.FormatInt(secs, 10)}})
}
