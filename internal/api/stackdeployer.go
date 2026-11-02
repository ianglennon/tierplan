// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
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
	// HostEscalating is true when the stack asks for host access that only an env Admin may deploy. The deploy was
	// judged as the user it acts for, so that user is an Admin of the environment.
	HostEscalating bool
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
	var escalating bool
	out, err := deployDraft(ctx, s.d, p, env, stackID, StackDeployRequest{DraftVersion: draftVersion}, "", deployOpts{Trigger: trigger, Escalating: &escalating})
	if err != nil {
		return DeployStarted{}, s.refusal(stackID, err)
	}
	return DeployStarted{JobID: out.Body.JobID, Seq: out.Body.Revision, HostEscalating: escalating}, nil
}

// RevertedDrift says, as the lines an automatic deploy records, what deploying the draft of a stack on behalf of p would
// put back that was changed outside TierPlan (D-23): the "Reverts drift" part of the review that the Review page shows, read
// the same way (the engine as it runs now against the revision that is deployed and the draft), with the values of
// settings that hold secrets withheld as for a caller who may not reveal them, since what is recorded is read by others
// than the owner. A stack with no good revision has nothing to put back. It saves and starts nothing; an error is a
// drift that could not be worked out, which never stops the deploy.
func (s *StackDeployer) RevertedDrift(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64) ([]string, error) {
	if p == nil {
		return nil, errors.New("no principal")
	}
	repo := s.d.repo()
	st, err := repo.Stack(ctx, envID, stackID)
	if err != nil {
		return nil, err
	}
	draft, err := repo.Draft(ctx, stackID)
	if err != nil {
		return nil, err
	}
	if draft.Version != draftVersion {
		return nil, &DeployRefusal{Code: refusalDraftChanged, Detail: "The draft changed."}
	}
	good, ok, err := repo.LatestGoodRevision(ctx, stackID)
	if err != nil || !ok {
		return []string{}, err
	}
	rv, err := s.d.ops().Review(ctx, stackops.ReviewInput{
		EnvID: envID, Stack: st, Copy: draft.Copy, DraftVersion: draft.Version, Pins: repo, Revisions: repo,
		Caller: stackops.Caller{Principal: p, Actor: p.Name}, Audit: s.d.Store,
		Previous: &good.Copy, Deployed: &stackops.DeployedRevision{Seq: good.Seq, By: good.CreatedBy, At: good.CreatedAt},
		BaseSeq: draft.BaseSeq,
	})
	if err != nil {
		return nil, err
	}
	if rv.RevertsDriftUnavailable {
		return nil, errors.New("the drift could not be computed")
	}
	return stackops.RevertLines(stackops.RedactReverts(rv.RevertsDrift, st.Target)), nil
}

// GitDeployer is the deployer as the git sync reads it (gitsync.Deployer): the same deploy with the types of that
// package, which cannot import this one.
func (s *StackDeployer) GitDeployer() gitsync.Deployer { return gitDeployer{s} }

type gitDeployer struct{ s *StackDeployer }

// DeployDraft implements gitsync.Deployer.
func (g gitDeployer) DeployDraft(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64, trigger string) (gitsync.DeployStarted, error) {
	started, err := g.s.DeployDraft(ctx, p, envID, stackID, draftVersion, trigger)
	var ref *DeployRefusal
	if errors.As(err, &ref) {
		return gitsync.DeployStarted{}, &gitsync.DeployRefusal{Code: ref.Code, Detail: ref.Detail, Retry: ref.Retry}
	}
	if err != nil {
		return gitsync.DeployStarted{}, err
	}
	return gitsync.DeployStarted{JobID: started.JobID, Seq: started.Seq, HostEscalating: started.HostEscalating}, nil
}

// RevertedDrift implements gitsync.Deployer.
func (g gitDeployer) RevertedDrift(ctx context.Context, p *rbac.Principal, envID, stackID string, draftVersion int64) ([]string, error) {
	return g.s.RevertedDrift(ctx, p, envID, stackID, draftVersion)
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

// internal is a failure of TierPlan itself: it is logged with the stack only and may pass on its own.
func (s *StackDeployer) internal(what, stackID string, err error) error {
	s.d.Log.Error("an automated stack deploy failed", "step", what, "stack", stackID, "error", err)
	return &DeployRefusal{Code: refusalInternal, Detail: "TierPlan could not start the deploy.", Retry: true}
}

// refusal turns the error of deployDraft into the refusal an automated caller reads. The code comes from what
// the problem constructors set (the code a call site gave it, or the problem's type), never from its wording.
func (s *StackDeployer) refusal(stackID string, err error) error {
	var ce *codedProblem
	if errors.As(err, &ce) {
		if ce.code == refusalPolicy {
			if detail, ok := automaticPolicyDetail(ce.err); ok {
				return &DeployRefusal{Code: ce.code, Detail: detail}
			}
		}
		return &DeployRefusal{Code: ce.code, Detail: problemDetail(ce.err)}
	}
	var em *huma.ErrorModel
	if errors.As(err, &em) && em.Status < http.StatusInternalServerError {
		switch em.Type {
		case problemStackInvalid:
			return &DeployRefusal{Code: refusalNotDeployable, Detail: firstProblem(em)}
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
		case problemConcurrentUnacknowledged:
			// The route's sentence speaks to a person who started editing; nobody did here.
			return &DeployRefusal{Code: em.Type, Detail: concurrentAutomationDetail}
		case problemTargetChanged, problemStackNotFound, problemServicesInvalid:
			return &DeployRefusal{Code: em.Type, Detail: em.Detail}
		}
	}
	// Anything else is a failure of TierPlan or of the environment, which may pass: only the stack is logged.
	s.d.Log.Error("an automated stack deploy failed", "stack", stackID, "error", err)
	return &DeployRefusal{Code: refusalInternal, Detail: "TierPlan could not start the deploy.", Retry: true}
}

// concurrentAutomationDetail is what an automatic deploy says when the concurrent gate stops it: the gate is a backstop
// for a roll back or a deploy that landed after the automation looked at the draft.
const concurrentAutomationDetail = "Another deploy or roll back of this stack ran while the automatic deploy was starting."

// automaticPolicyDetail words a policy refusal for an automatic deploy: what needs Admin, each setting at its place, and
// no instruction to anyone at a screen. A refusal that lists no setting (a compose file that can't be checked) keeps its
// own sentence, so it answers false.
func automaticPolicyDetail(err error) (string, bool) {
	var em *huma.ErrorModel
	if !errors.As(err, &em) || em.Type != rbac.ReasonPrivilegedCompose || len(em.Errors) == 0 {
		return "", false
	}
	lines := make([]string, 0, len(em.Errors))
	for _, e := range em.Errors {
		if e == nil {
			continue
		}
		lines = append(lines, e.Message+" at "+e.Location)
	}
	if len(lines) == 0 {
		return "", false
	}
	return "This needs Admin: " + strings.Join(lines, "; ") + ".", true
}

// firstProblem is the first problem a not deployable stack lists with where it is, which is what a person needs to
// fix it; the problem's own sentence when it lists none.
func firstProblem(em *huma.ErrorModel) string {
	for _, e := range em.Errors {
		if e != nil && e.Message != "" {
			if e.Location != "" {
				return e.Location + ": " + e.Message
			}
			return e.Message
		}
	}
	return em.Detail
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
