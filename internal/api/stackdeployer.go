// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// StackDeployer starts the deploy of a stack's draft for a principal that is not on a request: the git sync
// and the webhook (Phase 9) deploy through it. It is the deploy route without the HTTP, so the draft is
// judged, locked, recorded and run exactly as a person pressing Deploy would have it, for the principal it is
// given (RESEARCH Pitfall 9, D-20). The principal must come from the access service's RunAs, never be built by
// the caller: the checks below judge it as the user it acts for, as of now (T-09-09).
type StackDeployer struct{ d StacksDeps }

// NewStackDeployer is the deployer of the stack routes' own dependencies. Locks must be the ones the routes
// use, so an automated deploy and a person's take turns on an environment; a nil Locks is a lock of this
// deployer's own, which is only right when nothing else deploys.
func NewStackDeployer(d StacksDeps) *StackDeployer {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Locks == nil {
		d.Locks = stackops.NewLocks()
	}
	return &StackDeployer{d: d}
}

// DeployStarted is a deploy that has started: the job to follow and the revision the deploy took from the draft.
type DeployStarted struct {
	JobID string
	Seq   int64
}

// DeployRefusal is the answer to a deploy that did not start. Code is stable and says why; Retry says whether
// the same deploy may succeed later without anyone changing anything (a held lock, a full job slot).
type DeployRefusal struct {
	Code, Detail string
	Retry        bool
}

// Error implements error.
func (r *DeployRefusal) Error() string { return "deploy refused: " + r.Code + ": " + r.Detail }

// DeployDraft deploys the draft of a stack at draftVersion on behalf of p. trigger says what started it
// ("poll" or "webhook") and is audited with the system actor and the user it acts for. No reviewed pins are
// sent, so the server's own pinning applies (P7 D-15). It answers a *DeployRefusal for a deploy that did not
// start.
func (s *StackDeployer) DeployDraft(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64, trigger string) (DeployStarted, error) {
	if p == nil {
		return DeployStarted{}, &DeployRefusal{Code: refusalForbidden, Detail: "no principal"}
	}
	env, err := s.environment(ctx, envID, stackID)
	if err != nil {
		return DeployStarted{}, err
	}
	// The route's own permission, which the router judges before its handler runs.
	if s.d.Checker == nil {
		return DeployStarted{}, s.internal("check permission", stackID, errors.New("no checker"))
	}
	dec := s.d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermStacksDeploy, Scope: rbac.ScopeEnvironment}, envID)
	if dec.Err != nil {
		return DeployStarted{}, s.internal("check permission", stackID, dec.Err)
	}
	if !dec.Allowed {
		return DeployStarted{}, &DeployRefusal{Code: refusalForbidden, Detail: "The user this deploy acts for may not deploy here."}
	}
	out, err := deployDraft(ctx, s.d, p, env, stackID, StackDeployRequest{DraftVersion: draftVersion}, "", deployOpts{Trigger: trigger})
	if err != nil {
		return DeployStarted{}, s.refusal(stackID, err)
	}
	return DeployStarted{JobID: out.Body.JobID, Seq: out.Body.Revision}, nil
}

// environment resolves the environment the way the router does: one that is gone or disabled is refused.
func (s *StackDeployer) environment(ctx context.Context, envID, stackID string) (*httpx.Env, error) {
	if s.d.Envs == nil {
		return nil, &DeployRefusal{Code: refusalEnvNotFound, Detail: "The environment is not available."}
	}
	e, err := s.d.Envs.Resolve(ctx, envID)
	switch {
	case errors.Is(err, httpx.ErrEnvNotFound):
		return nil, &DeployRefusal{Code: refusalEnvNotFound, Detail: "The environment is gone."}
	case errors.Is(err, httpx.ErrEnvDisabled):
		return nil, &DeployRefusal{Code: refusalEnvDisabled, Detail: "The environment is disabled."}
	case err != nil:
		return nil, s.internal("resolve the environment", stackID, err)
	}
	return &e, nil
}

// internal is a failure of Bayplan itself: it is logged with the stack only and may pass on its own.
func (s *StackDeployer) internal(what, stackID string, err error) error {
	s.d.Log.Error("an automated stack deploy failed", "step", what, "stack", stackID, "error", err)
	return &DeployRefusal{Code: refusalInternal, Detail: "Bayplan could not start the deploy.", Retry: true}
}

// refusal turns the error of deployDraft into the refusal an automated caller reads. The code comes from what
// the problem constructors set (the code a call site gave it, or the problem's type), never from its wording.
func (s *StackDeployer) refusal(stackID string, err error) error {
	var ce *codedProblem
	if errors.As(err, &ce) {
		return &DeployRefusal{Code: ce.code, Detail: problemDetail(ce.err)}
	}
	var em *huma.ErrorModel
	if errors.As(err, &em) && em.Status < http.StatusInternalServerError {
		switch em.Type {
		case problemStackInvalid:
			return &DeployRefusal{Code: refusalNotDeployable, Detail: em.Detail}
		case problemNameClash:
			return &DeployRefusal{Code: refusalNameClash, Detail: em.Detail}
		case problemDraftChanged:
			return &DeployRefusal{Code: refusalDraftChanged, Detail: em.Detail}
		case problemPinsInvalid:
			return &DeployRefusal{Code: refusalPinsInvalid, Detail: em.Detail}
		case problemOperationLocked:
			return &DeployRefusal{Code: refusalOperationLocked, Detail: em.Detail, Retry: true}
		case problemTooManyDeploys:
			return &DeployRefusal{Code: refusalJobsBusy, Detail: em.Detail, Retry: true}
		case problemTargetChanged, problemConcurrentUnacknowledged, problemStackNotFound, problemServicesInvalid:
			return &DeployRefusal{Code: em.Type, Detail: em.Detail}
		}
	}
	// Anything else is a failure of Bayplan or of the environment, which may pass: only the stack is logged.
	s.d.Log.Error("an automated stack deploy failed", "stack", stackID, "error", err)
	return &DeployRefusal{Code: refusalInternal, Detail: "Bayplan could not start the deploy.", Retry: true}
}

// problemDetail is the sentence of a problem, empty for any other error.
func problemDetail(err error) string {
	var em *huma.ErrorModel
	if errors.As(err, &em) {
		return em.Detail
	}
	return ""
}

// refuse gives a problem the code an automated caller reads. The route answers the problem itself (plainProblem).
func refuse(code string, err error) error { return &codedProblem{code: code, err: err} }

// The codes of a refusal. They are stable: an automated caller decides on them.
const (
	refusalForbidden       = "forbidden"
	refusalEnvNotFound     = "environment_not_found"
	refusalEnvDisabled     = "environment_disabled"
	refusalPolicy          = "policy_refused"
	refusalNotDeployable   = "not_deployable"
	refusalNameClash       = "name_clash"
	refusalDraftChanged    = "draft_changed"
	refusalObjects         = "objects_invalid"
	refusalConvert         = "convert_failed"
	refusalPinsInvalid     = "pins_invalid"
	refusalOperationLocked = "operation_locked"
	refusalJobsBusy        = "jobs_busy"
	refusalInternal        = "internal"
)

// plainProblem is err as the route answers it: a refusal that was given a code for an automated caller is
// the problem it wraps.
func plainProblem(err error) error {
	var ce *codedProblem
	if errors.As(err, &ce) {
		return ce.err
	}
	return err
}

// codedProblem is a problem of a deploy with the code an automated caller reads. The route answers the problem
// itself (plainProblem); only DeployDraft reads the code.
type codedProblem struct {
	code string
	err  error
}

func (c *codedProblem) Error() string { return c.err.Error() }
func (c *codedProblem) Unwrap() error { return c.err }
