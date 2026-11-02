// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

// Adopting a stack TierPlan did not make (plan 08-13, STK-13, D-14). Nothing is sent to an engine: the server
// rebuilds the definition from what runs and stores it as the stack's draft and first revision.

const (
	// problemAdoptStale is the type of the 409 for a stack that changed after the page showed it.
	problemAdoptStale = "adopt_stale"
	// problemStackAlreadyManaged is the type of the 409 for a stack TierPlan already manages under that name.
	problemStackAlreadyManaged = "stack_already_managed"
	// problemStackNotAdoptable is the type of the 409 for TierPlan's own stack, its agent's and a name the target cannot hold.
	problemStackNotAdoptable = "stack_not_adoptable"
	// problemPastedFileInvalid is the type of the 422 for a pasted compose file that does not pass the check at the commit.
	problemPastedFileInvalid = "pasted_file_invalid"

	// adoptLockKind is the kind of operation the adoption holds the environment's operation lock as.
	adoptLockKind = "adopt"
)

// AdoptStackRequest is the body of the adopt route.
type AdoptStackRequest struct {
	Fingerprint string         `json:"fingerprint" required:"true" maxLength:"65536" doc:"The fingerprint the unmanaged stack's page read. When what runs has changed since, the adoption is refused 409 adopt_stale and nothing is stored"`
	Source      string         `json:"source,omitempty" enum:"rebuild,paste" default:"rebuild" doc:"rebuild: TierPlan writes the definition from what runs. paste: the definition is the compose file in compose, checked again here and stored exactly as written"`
	Compose     string         `json:"compose,omitempty" maxLength:"1048576" doc:"For source paste, the original compose file. Untrusted: it goes through the same check as any draft (no include, no extends, no file read from the server, at most 1 MiB). Never rewritten"`
	Vars        []DraftVarBody `json:"vars,omitempty" maxItems:"500" doc:"For source paste, the values of the variables the pasted file uses. A secret one is stored sealed, like any secret variable"`
}

// AdoptCheckRequest is the body of the adopt check route. It may be left out, which is the rebuild.
type AdoptCheckRequest struct {
	Source  string         `json:"source,omitempty" enum:"rebuild,paste" default:"rebuild" doc:"rebuild: TierPlan writes the definition from what runs. paste: check the compose file in compose against what runs; needs secrets.reveal on the environment, because the comparison is made on the real values"`
	Compose string         `json:"compose,omitempty" maxLength:"1048576" doc:"For source paste, the original compose file. Untrusted: it goes through the same check as any draft (no include, no extends, no file read from the server, at most 1 MiB)"`
	Vars    []DraftVarBody `json:"vars,omitempty" maxItems:"500" doc:"For source paste, the values of the variables the pasted file uses"`
}

// AdoptCheckInput is the path and optional body of the adopt check route: a pointer, so that a request with no body
// is the rebuild.
type AdoptCheckInput struct {
	UnmanagedStackPathInput
	Body *AdoptCheckRequest
}

// pastedCopyOf is the working copy of a pasted adoption: the compose text as written, as the stack's only file, and
// the variables sent with it, judged by the rules of a save. It is nil for the rebuild. The text is never trimmed or
// rewritten. A secret sent as the placeholder has no saved value to keep, so it is refused.
func pastedCopyOf(source, compose string, vars []DraftVarBody) (*stacks.WorkingCopy, error) {
	if source != stackops.AdoptSourcePaste {
		return nil, nil
	}
	wc, err := draftCopy(StackDraftSaveRequest{
		Files: []DraftFileBody{{Path: stacks.ComposeFile, Content: compose}}, Vars: vars,
	}, stacks.WorkingCopy{})
	if err != nil {
		return nil, err
	}
	return &wc, nil
}

// AdoptStackInput is the path and body of the adopt route.
type AdoptStackInput struct {
	UnmanagedStackPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      AdoptStackRequest
}

// maxAdoptChecksPerUser is how many adopt checks one user may have running: each reads the engine and rebuilds a stack.
const maxAdoptChecksPerUser = 2

// newAdoptCheckLimiter is the limiter of the adopt check route.
func newAdoptCheckLimiter() *streamLimiter {
	return &streamLimiter{max: maxAdoptChecksPerUser, open: map[string]int{}}
}

func registerStackAdopt(r *httpx.Router, d StacksDeps, checkLimiter *streamLimiter) {
	registerStackAdoptCheck(r, d, checkLimiter)
	httpx.Register(r, huma.Operation{
		OperationID: "adoptStack", Method: http.MethodPost, Path: "/api/env/{envId}/unmanaged-stacks/{name}/adopt",
		Summary: "Adopt a stack TierPlan did not make",
		Description: "Takes over a swarm namespace or a Compose project that TierPlan does not manage. TierPlan reads what runs, rebuilds the compose definition itself (the body carries none), " +
			"and in one transaction stores the stack (source adopted), its draft and revision 1 (origin adopted, good, pinned to the images that run). Nothing is sent to any engine: the stack's objects get TierPlan's labels on its first deploy. " +
			"The definition holds the environment values as literals. The fingerprint is the one the stack's page read; when what runs has changed since, the answer is 409 adopt_stale and nothing is stored. " +
			"Adopting never fails on the host-access policy: it only records what already runs. " +
			"With source paste the definition is the original compose file in compose instead (the person still has it): it is checked again here by the same check as any draft and never trusted from the page, " +
			"stored exactly as written (comments and key order kept) as the draft and revision 1 together with vars (a secret one sealed), pinned to the digests that run for the services that run; a file that does not pass is 422 pasted_file_invalid. " +
			"A stack that is TierPlan's own, runs TierPlan's agent, or has a name its target cannot hold is 409 stack_not_adoptable with the reason; a name TierPlan already manages (on a swarm regardless of case, as for creating a stack) is 409 stack_already_managed with the stack's ID in errors as stackId; " +
			"a name nothing carries is 404 unmanaged_stack_not_found; a stack that cannot be written as a compose file that passes the check is 422 rebuild_failed. " +
			"Only one stack operation at a time runs on an environment: while one does, the answer is 409 operation_locked and nothing is stored. Answers the stack, as the stack route does. Audited as stack.adopted. Needs stacks.create (Env Admin).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *AdoptStackInput) (*StackDetailOutput, error) {
			return adoptStack(ctx, d, in)
		})
}

// stackAlreadyManaged is the 409 for a name a stack of TierPlan's holds, with that stack's ID.
func stackAlreadyManaged(name, stackID string) error {
	return &huma.ErrorModel{
		Type: problemStackAlreadyManaged, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: name + " is already managed by TierPlan.",
		Errors: []*huma.ErrorDetail{{Location: "stackId", Message: stackID}},
	}
}

func adoptStack(ctx context.Context, d StacksDeps, in *AdoptStackInput) (*StackDetailOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	pasted, err := pastedCopyOf(in.Body.Source, in.Body.Compose, in.Body.Vars)
	if err != nil {
		return nil, err
	}
	repo := d.repo()
	own, err := repo.Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "list", env.ID, err)
	}
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	// The rule create holds a name to: on a swarm a case variant of a managed name is that stack's (T-08-95).
	if held, taken := stackops.HoldingName(own, target, in.Name); taken {
		return nil, stackAlreadyManaged(in.Name, held.ID)
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "adopt: read the environment", env.ID, err)
	}

	// The lock is held for the commit only: nothing is deployed, so there is no job to hand it to.
	release, held, ok := d.Locks.TryAcquire(env.ID, stackops.Holder{
		StackName: in.Name, Kind: adoptLockKind, By: p.Name, Started: time.Now().UTC(),
	})
	if !ok {
		return nil, operationLocked(env.Name, held)
	}
	defer release()

	adopted, err := d.ops().AdoptWithOutcome(ctx, repo, stackops.AdoptInput{
		EnvID: env.ID, Name: in.Name, Target: target, Fingerprint: in.Body.Fingerprint,
		Caller: stackops.Caller{Principal: p, Actor: p.Name}, Managed: own, Self: d.selfMarks(row.Kind),
		Source: in.Body.Source, Pasted: pasted,
	})
	switch {
	case err == nil:
	case errors.Is(err, stackops.ErrAdoptStale):
		return nil, httpx.Problem(http.StatusConflict, problemAdoptStale,
			"What is running on "+env.Name+" changed since the check. Run the check again before you adopt.")
	default:
		return nil, adoptProblem(ctx, d, repo, env.ID, in.Name, err)
	}

	st := adopted.Stack
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackAdopted, st.ID, audit.ResultSuccess, "", map[string]string{
		"stack": st.Name, "target": string(st.Target), "source": st.Source.Ref,
		"services": itoa(int64(adopted.Services)), "revision": itoa(adopted.Revision),
	})
	body, err := stackDetailOf(ctx, d, st)
	if err != nil {
		return nil, stackInternal(d, "detail", env.ID, err)
	}
	return &StackDetailOutput{CacheControl: "no-store", Body: body}, nil
}

// adoptProblem is the answer for the refusals an adoption and its check share: a name TierPlan holds, a stack that
// cannot be adopted, one that is gone, and one that cannot be written as a compose file. Anything else is the
// engine's failure.
func adoptProblem(ctx context.Context, d StacksDeps, repo stackops.Repo, envID, name string, err error) error {
	var failed *stackops.RebuildError
	var refused *stackops.NotAdoptableError
	var badPaste *stackops.PasteError
	var badVar *stacks.VarError
	switch {
	case errors.As(err, &badPaste):
		return httpx.Problem(http.StatusUnprocessableEntity, problemPastedFileInvalid, badPaste.Reason)
	case errors.As(err, &badVar):
		return varRuleProblem(err)
	case errors.Is(err, stackops.ErrNameTaken):
		// Also the loser of two adoptions that raced: the winner's stack holds the name now.
		now, lerr := repo.Stacks(ctx, envID)
		if lerr != nil {
			return stackInternal(d, "list", envID, lerr)
		}
		target, terr := d.stackTargetOf(ctx, envID)
		if terr != nil {
			return terr
		}
		if held, taken := stackops.HoldingName(now, target, name); taken {
			return stackAlreadyManaged(name, held.ID)
		}
		return stackInternal(d, "adopt", envID, err)
	case errors.As(err, &refused):
		return httpx.Problem(http.StatusConflict, problemStackNotAdoptable, notAdoptableReason(refused.Found))
	case errors.Is(err, stackops.ErrUnmanagedGone):
		return unmanagedNotFound("")
	case errors.As(err, &failed):
		return httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, failed.Reason)
	case errors.Is(err, stackops.ErrRebuildTarget):
		return httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, "TierPlan can't rebuild this kind of stack.")
	}
	return snapshotProblem(d.Log, envID, err)
}

// AdoptCheckBody is what adopting a stack would be like: the first deploy compared with what runs, before anything is
// stored.
type AdoptCheckBody struct {
	Fingerprint   string                   `json:"fingerprint" doc:"The state of the engine the check was made of. Adopt sends it back; when what runs has changed since, the adoption is refused 409 adopt_stale"`
	Clean         bool                     `json:"clean" doc:"True when nothing will change on the first deploy except TierPlan's ownership labels"`
	Gaps          []StackReviewServiceBody `json:"gaps" doc:"The services the first deploy would change in more than an ownership label, as the review lists services. For a caller without secrets.reveal every environment value, command, argument, health check command and log option reads <hidden>, and a setting that differs only in such a value is listed by its path in hiddenChanges"`
	GapCount      int                      `json:"gapCount" doc:"How many services are in gaps"`
	OwnershipOnly []string                 `json:"ownershipOnly" doc:"The names, in the compose file, of the services that differ from what runs in nothing but TierPlan's ownership labels: the first deploy recreates each of them (a brief restart) and changes nothing else"`
	Objects       []AdoptCheckObjectBody   `json:"objects" doc:"The networks, volumes, secrets and configs of the stack and how each is treated, by kind then name"`
	Counts        AdoptCheckCountsBody     `json:"counts" doc:"How many objects of each kind there are"`
	Policy        AdoptCheckPolicyBody     `json:"policy" doc:"The host-access settings of the stack, listed for every caller whatever their role. They block nothing here: they say what a later deploy needs an Admin for"`
	Portainer     bool                     `json:"portainer" doc:"True when the stack carries a mark of Portainer. The advice is to remove it from Portainer without deleting it"`
	Orphan        bool                     `json:"orphan" doc:"True when an object of the stack carries the ID of a TierPlan stack this instance does not hold"`
	Notes         []GeneratedNoteBody      `json:"notes" doc:"What the rebuild could not say plainly, by service then code. Empty for a pasted file"`
	Source        string                   `json:"source" enum:"rebuild,paste" doc:"Which definition was checked: the rebuild from what runs, or the pasted file"`
	Problems      []StackProblemBody       `json:"problems" doc:"For a pasted file, what the check found in it, as the editor's check lists problems. Always empty for the rebuild"`
	Variables     []StackVariableBody      `json:"variables" doc:"For a pasted file, the variables it uses with the value a deploy would use; a secret one reads <hidden>. Always empty for the rebuild"`
	Matches       bool                     `json:"matches" doc:"True when a pasted file differs from what runs in nothing but TierPlan's ownership labels. Always false for the rebuild"`
	Blocked       bool                     `json:"blocked" doc:"True when a pasted file has an error, so nothing was compared with what runs: gaps and ownershipOnly are then empty"`
}

// AdoptCheckObjectBody is one network, volume, secret or config of the stack and how adopting treats it (D-13, D-18).
type AdoptCheckObjectBody struct {
	Kind      string `json:"kind" enum:"network,volume,secret,config" doc:"What it is"`
	Name      string `json:"name" doc:"Its name on the engine"`
	Treatment string `json:"treatment" enum:"owned,external" doc:"owned: the stack owns it, under this name, and removes it with the stack (a network or volume that carries the stack's label). external: the stack only names it and TierPlan never changes it (any other network or volume, and every existing secret and config)"`
}

// AdoptCheckCountsBody is how many objects of each kind a stack has.
type AdoptCheckCountsBody struct {
	Networks int `json:"networks" doc:"Networks, owned and external"`
	Volumes  int `json:"volumes" doc:"Volumes, owned and external"`
	Secrets  int `json:"secrets" doc:"Secrets the stack references"`
	Configs  int `json:"configs" doc:"Configs the stack references"`
}

// AdoptCheckPolicyBody is the host access a stack uses.
type AdoptCheckPolicyBody struct {
	Kinds   []string                `json:"kinds" doc:"The kinds of host access, such as bind mounts or extra privileges, in a fixed order"`
	Entries []AdoptCheckPolicyEntry `json:"entries" doc:"Each setting, as a phrase with its service, such as a bind mount of /var/run/docker.sock in web"`
}

// AdoptCheckPolicyEntry is one host-access setting.
type AdoptCheckPolicyEntry struct {
	Text string `json:"text" doc:"What the setting is and the service that has it"`
}

// AdoptCheckOutput wraps AdoptCheckBody for huma.
type AdoptCheckOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AdoptCheckBody
}

func registerStackAdoptCheck(r *httpx.Router, d StacksDeps, lim *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "checkAdoption", Method: http.MethodPost, Path: "/api/env/{envId}/unmanaged-stacks/{name}/adopt-check",
		Summary: "Check what adopting a stack would change on its first deploy",
		Description: "Rebuilds the compose definition of a stack TierPlan does not manage from what runs, as adopting does, and reviews it against what runs: every difference that is not one of TierPlan's ownership labels is a gap, " +
			"a change the first deploy would make, and the services that differ only in an ownership label are listed apart. It reads the engine and changes nothing, on the engine or in the store, and a gap never blocks adopting. " +
			"The fingerprint it answers is the one to send with the adoption. " +
			"With source paste the body carries the original compose file and its variables instead: the file goes through the same check as any draft (no include, no extends, no file read from the server, at most 1 MiB) and is compared with what runs; " +
			"a file with an error answers blocked true with its problems and nothing compared, and matches says it differs from what runs in nothing but the ownership labels. The body may be left out, which is the rebuild. " +
			"The comparison is made on the real values, so a pasted file is compared only for a caller who holds secrets.reveal on the environment (Env Admin, or an Operator granted it): any other caller is 403 secret_reveal_required with nothing compared, because the answer would tell whether a guessed value is the running one. The rebuild stays open to every caller with stacks.read. " +
			"The answer holds the live settings of running services, so for a caller without secrets.reveal on the environment every environment value, command, argument, health check command and log option is <hidden>. " +
			"A stack that is TierPlan's own, runs TierPlan's agent, or has a name its target cannot hold is 409 stack_not_adoptable; a name TierPlan already manages (on a swarm regardless of case, as for creating a stack) is 409 stack_already_managed with the stack's ID in errors as stackId; " +
			"a name nothing carries is 404 unmanaged_stack_not_found; a stack that cannot be written as a compose file that passes the check is 422 rebuild_failed. " +
			"One user may have two checks running; a third is 429 check_busy. Needs stacks.read (Viewer); source paste also needs secrets.reveal.",
		Tags: []string{"stacks"}, MaxBodyBytes: maxStackSaveBody,
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *AdoptCheckInput) (*AdoptCheckOutput, error) {
			return checkAdoption(ctx, d, lim, in)
		})
}

func checkAdoption(ctx context.Context, d StacksDeps, lim *streamLimiter, in *AdoptCheckInput) (*AdoptCheckOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	var body AdoptCheckRequest
	if in.Body != nil {
		body = *in.Body
	}
	// A pasted file is compared with what runs on the real values, so what the answer says (clean, matches, the
	// number of gaps, the paths of the hidden changes) would tell a caller whether a guess at a secret value is
	// right. Only a caller who may see the values may supply one side of that comparison (T-08-52). The rebuild
	// takes nothing from the caller and is masked, so it stays open.
	reveal := redact.CanReveal(ctx, d.Checker, p, env.ID)
	if body.Source == stackops.AdoptSourcePaste && !reveal {
		return nil, httpx.Problem(http.StatusForbidden, problemSecretRevealRequired,
			"Checking a pasted file compares it with the values that run, which needs the reveal secrets permission. Ask an administrator, or check the file that was rebuilt from what runs.")
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemCheckBusy, "Too many checks are running. Try again in a moment.")
	}
	defer release()
	if d.OnAdoptCheck != nil {
		d.OnAdoptCheck(ctx)
	}

	repo := d.repo()
	own, err := repo.Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "list", env.ID, err)
	}
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "adopt check: read the environment", env.ID, err)
	}
	pasted, err := pastedCopyOf(body.Source, body.Compose, body.Vars)
	if err != nil {
		return nil, err
	}
	res, err := d.ops().AdoptCheck(ctx, stackops.AdoptInput{
		EnvID: env.ID, Name: in.Name, Target: target, Caller: stackops.Caller{Principal: p, Actor: p.Name}, Managed: own, Self: d.selfMarks(row.Kind),
		Source: body.Source, Pasted: pasted,
	}, reveal)
	if err != nil {
		return nil, adoptProblem(ctx, d, repo, env.ID, in.Name, err)
	}
	return &AdoptCheckOutput{CacheControl: "no-store", Body: adoptCheckBody(res)}, nil
}

func adoptCheckBody(res stackops.AdoptCheckResult) AdoptCheckBody {
	only := res.OwnershipOnly
	if only == nil {
		only = []string{}
	}
	body := AdoptCheckBody{
		Fingerprint: res.Fingerprint, Clean: res.Clean, Gaps: serviceBodies(res.Gaps), GapCount: len(res.Gaps), OwnershipOnly: only,
		Objects: make([]AdoptCheckObjectBody, 0, len(res.Objects)), Notes: make([]GeneratedNoteBody, 0, len(res.Notes)),
		Policy:    AdoptCheckPolicyBody{Kinds: []string{}, Entries: []AdoptCheckPolicyEntry{}},
		Portainer: res.Portainer, Orphan: res.Orphan,
		Source: res.Source, Problems: problemBodies(res.Problems), Variables: variableBodies(res.Variables), Matches: res.Matches, Blocked: res.Blocked,
	}
	for _, o := range res.Objects {
		body.Objects = append(body.Objects, AdoptCheckObjectBody{Kind: o.Kind, Name: o.Name, Treatment: o.Treatment})
		switch o.Kind {
		case rebuild.KindNetwork:
			body.Counts.Networks++
		case rebuild.KindVolume:
			body.Counts.Volumes++
		case rebuild.KindSecret:
			body.Counts.Secrets++
		case rebuild.KindConfig:
			body.Counts.Configs++
		}
	}
	for _, n := range res.Notes {
		body.Notes = append(body.Notes, GeneratedNoteBody{Service: n.Service, Code: n.Code, Message: n.Message})
	}
	body.Policy.Kinds = append(body.Policy.Kinds, res.Policy.Kinds...)
	for _, e := range res.Policy.Entries {
		body.Policy.Entries = append(body.Policy.Entries, AdoptCheckPolicyEntry{Text: e.Text})
	}
	return body
}
