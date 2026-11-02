// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// StackRevisionImageChange is one service whose image differs from the previous good revision's.
type StackRevisionImageChange struct {
	Service string `json:"service" doc:"The service's name in the compose file"`
	From    string `json:"from" doc:"The reference the previous good revision ran, tag@sha256; empty for a service that revision did not have"`
	To      string `json:"to" doc:"The reference this revision pinned, tag@sha256"`
}

// StackRevisionSource is where a git stack's revision was read from.
type StackRevisionSource struct {
	RepoID       string   `json:"repoId" doc:"The repository the revision was made from; compare it with the stack's repository to know whether a commit link applies"`
	RepoName     string   `json:"repoName" doc:"The repository's name when the revision was made"`
	RefKind      string   `json:"refKind" enum:"branch,tag,commit" doc:"What ref names"`
	Ref          string   `json:"ref" doc:"The branch, tag or commit ID the stack followed"`
	Folder       string   `json:"folder" doc:"The folder of the repository the stack lived in; empty for the root"`
	ComposeFiles []string `json:"composeFiles" doc:"The compose files of the folder the revision merged, in merge order"`
}

// StackRevisionItem is one revision of a stack.
type StackRevisionItem struct {
	Seq           int64                      `json:"seq" doc:"The revision number, unique per stack and never reused"`
	Origin        string                     `json:"origin" enum:"deploy,update,rollback,adopted" doc:"How the revision came to be: a deploy, an Update images deploy, a roll back that copied an earlier revision, or the first revision of an adopted stack"`
	RestoredFrom  int64                      `json:"restoredFrom" doc:"The number of the revision a roll back copied; 0 for any other revision"`
	Outcome       string                     `json:"outcome" enum:"pending,good,failed" doc:"pending while its deploy runs, then good or failed"`
	CreatedBy     string                     `json:"createdBy" doc:"Who started the deploy the revision was taken for"`
	CreatedAt     string                     `json:"createdAt" format:"date-time" doc:"When the revision was taken, RFC 3339 UTC"`
	FailedService string                     `json:"failedService" doc:"The service a failed deploy stopped at; empty when it did not fail or the failure names none"`
	Code          string                     `json:"code" doc:"The code of the failure; empty when the revision did not fail"`
	Reason        string                     `json:"reason" doc:"The sentence of the failure code; empty when there is no code or TierPlan has no sentence for it"`
	Deployed      bool                       `json:"deployed" doc:"True for the newest good revision, the one the stack stands at"`
	Images        []StackRevisionImageChange `json:"images" doc:"The services whose image differs from the next older good revision's, by service name. Empty for a revision that changed none, and for one with no older good revision to compare with"`
	Pinned        int                        `json:"pinned" doc:"How many images the revision pinned, when it has no older good revision to compare with (the first or an adopted one); 0 otherwise"`
	AdoptedFrom   string                     `json:"adoptedFrom" doc:"The swarm namespace or Compose project an adopted stack was taken over from; empty for any other revision"`
	Commit        string                     `json:"commit" doc:"The full ID of the commit the revision was made from; empty for a revision of a stack that was not from a git repository then"`
	Tree          string                     `json:"tree" doc:"The hash of the stack's folder at that commit; empty when commit is"`
	Source        *StackRevisionSource       `json:"source,omitempty" doc:"Where the revision was read from; left out for a revision that was not made from a git repository"`
	SyncTrigger   string                     `json:"syncTrigger" enum:",poll,webhook" doc:"What started the deploy of a revision no person started: poll for a check on a schedule, webhook for a push the repository announced. Empty for any other revision"`
}

// StackRevisionListBody is the History tab of a stack.
type StackRevisionListBody struct {
	Items  []StackRevisionItem `json:"items" doc:"The revisions of the stack, newest first by revision number. A pruned revision is gone from the list"`
	Capped bool                `json:"capped" doc:"True when the stack has more revisions than the 500 the list holds; the oldest are left out"`
	Kept   int                 `json:"kept" doc:"How many revisions of each stack TierPlan keeps (the instance setting, 5 to 200). The deployed revision and any still deploying are kept besides"`
}

// StackRevisionListOutput wraps StackRevisionListBody for huma.
type StackRevisionListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRevisionListBody
}

func registerStackHistory(r *httpx.Router, d StacksDeps, changesLimiter *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "listStackRevisions", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/revisions",
		Summary: "The revisions of a stack",
		Description: "Every deploy of the stack is a revision. They come newest first by revision number, with how each came to be (origin), the revision a roll back copied, " +
			"the result (outcome, and for a failed one the service and the sentence of its failure code), who started it and when, " +
			"and the images that changed against the next older good revision. The newest good revision is marked deployed. " +
			"Only references, codes and names are listed: no file, variable or rendered text. A stack with no revision answers an empty list. " +
			"A stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackRevisionListOutput, error) {
			return listStackRevisions(ctx, d, in)
		})
	registerStackRevision(r, d)
	registerStackRevisionChanges(r, d, changesLimiter)
}

func listStackRevisions(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackRevisionListOutput, error) {
	repo := d.repo()
	// The stack is read by its environment and ID first, so a stack of another environment is not found.
	st, err := repo.Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	rows, capped, err := repo.ListRevisions(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "list revisions", in.EnvID, err)
	}
	body := StackRevisionListBody{Items: make([]StackRevisionItem, 0, len(rows)), Capped: capped, Kept: d.revisionsKept()}
	for _, row := range rows {
		body.Items = append(body.Items, revisionItem(st, row))
	}
	return &StackRevisionListOutput{CacheControl: "no-store", Body: body}, nil
}

// revisionItem is a revision of the stack as the list and the detail name it.
func revisionItem(st stackops.Stack, row stackops.RevisionRow) StackRevisionItem {
	item := StackRevisionItem{
		Seq: row.Seq, Origin: row.Origin, RestoredFrom: row.RestoredFrom, Outcome: row.Outcome, CreatedBy: row.CreatedBy,
		CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339), FailedService: row.FailedService, Code: row.Code,
		Reason: stackops.CodeMessage(row.Code), Deployed: row.Deployed, Pinned: row.Pinned,
		Images: make([]StackRevisionImageChange, 0, len(row.Images)),
	}
	for _, c := range row.Images {
		item.Images = append(item.Images, StackRevisionImageChange{Service: pinService(st, c.Service), From: c.From, To: c.To})
	}
	if row.Origin == stackops.OriginAdopted {
		item.AdoptedFrom = st.Source.Ref
	}
	item.Commit, item.Tree, item.SyncTrigger = row.Git.Commit, row.Git.Tree, row.SyncTrigger
	if src := row.Git.Source; src.RepoID != "" {
		item.Source = &StackRevisionSource{
			RepoID: src.RepoID, RepoName: src.RepoName, RefKind: src.RefKind, Ref: src.Ref, Folder: src.Folder, ComposeFiles: composeFilesOf(src.ComposeFiles),
		}
	}
	return item
}

// pinService names a pinned service as the compose file does. A swarm revision pins by swarm service name,
// {stack}_{service}; a standalone one already pins by the compose name.
func pinService(st stackops.Stack, service string) string {
	if st.Target == stacks.TargetSwarm {
		return strings.TrimPrefix(service, st.Name+"_")
	}
	return service
}

// StackRevisionInput is the path of one revision of a stack.
type StackRevisionInput struct {
	EnvID   string `path:"envId" doc:"The environment ID"`
	StackID string `path:"stackId" doc:"The stack ID"`
	Seq     int64  `path:"seq" minimum:"1" doc:"The revision number"`
}

// StackRevisionFileBody is a file of a revision. Its content is never sent: a plain file's is the stored
// rendering's business, and a secret file's is nobody's.
type StackRevisionFileBody struct {
	Path   string `json:"path" doc:"The file's path in the stack"`
	Secret bool   `json:"secret" doc:"True for a secret file. Its content is not in any answer about a revision"`
}

// StackRevisionVarBody is a variable of a revision.
type StackRevisionVarBody struct {
	Name   string `json:"name" doc:"The variable's name"`
	Value  string `json:"value" doc:"The value; <hidden> for a secret variable, whoever asks"`
	Secret bool   `json:"secret" doc:"True for a secret variable"`
}

// StackRevisionPinBody is an image a revision was deployed with.
type StackRevisionPinBody struct {
	Service string `json:"service" doc:"The service's name in the compose file"`
	Image   string `json:"image" doc:"The reference the revision pinned, tag@sha256"`
	Digest  string `json:"digest" doc:"The sha256 digest of the reference; empty when it has none"`
}

// StackRevisionDetailBody is one revision of a stack.
type StackRevisionDetailBody struct {
	Item        StackRevisionItem       `json:"item" doc:"The revision as the list shows it"`
	DeployedSeq int64                   `json:"deployedSeq" doc:"The number of the deployed revision (the newest good one); 0 when the stack has none"`
	Rendered    string                  `json:"rendered" doc:"The rendering stored with the revision, masked when it was stored: a secret value is <hidden>. Empty for a revision with none"`
	Files       []StackRevisionFileBody `json:"files" doc:"The stack's files in the revision, by path"`
	Vars        []StackRevisionVarBody  `json:"vars" doc:"The stack's variables in the revision, by name"`
	Pins        []StackRevisionPinBody  `json:"pins" doc:"The images the revision was deployed with, by service"`
	Subnets     map[string]string       `json:"subnets" doc:"The subnets the revision's deploy gave the networks it created under the environment's small stack subnets, by network name on the swarm (name to CIDR). Empty when it gave none"`
	YAML        StackTextDiffBody       `json:"yaml" doc:"The deployed revision's rendering against this revision's. No hunks for the deployed revision itself, for a revision with the same rendering, and when either has no stored rendering"`
}

// StackRevisionDetailOutput wraps StackRevisionDetailBody for huma.
type StackRevisionDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRevisionDetailBody
}

const problemRevisionNotFound = "revision_not_found"

func registerStackRevision(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackRevision", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/revisions/{seq}",
		Summary: "One revision of a stack",
		Description: "What a revision deployed: the rendering stored with it, its files (path and whether secret; a file's content is never sent), " +
			"its variables (a secret variable's value is <hidden>, for every caller: there is no reveal for a revision), " +
			"the images it pinned by service with their digests, and the difference of its rendering from the deployed revision's. " +
			"A revision number the stack does not hold (never taken, or deleted by retention) is 404 revision_not_found; " +
			"a stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackRevisionInput) (*StackRevisionDetailOutput, error) {
			return getStackRevision(ctx, d, in)
		})
}

func getStackRevision(ctx context.Context, d StacksDeps, in *StackRevisionInput) (*StackRevisionDetailOutput, error) {
	repo := d.repo()
	// The stack is read by its environment and ID first, so a revision of another environment's stack is not
	// found; the revision is then read by that stack's ID and its number.
	st, err := repo.Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	rev, err := repo.RevisionDetail(ctx, st, in.Seq)
	if errors.Is(err, stackops.ErrRevisionNotFound) {
		return nil, httpx.Problem(http.StatusNotFound, problemRevisionNotFound, "That revision is gone.")
	}
	if err != nil {
		return nil, stackInternal(d, "read revision", in.EnvID, err)
	}
	body := StackRevisionDetailBody{
		Item: revisionItem(st, rev.Row), DeployedSeq: rev.DeployedSeq, Rendered: string(rev.Rendered),
		Files: make([]StackRevisionFileBody, 0, len(rev.Copy.Files)), Vars: make([]StackRevisionVarBody, 0, len(rev.Copy.Vars)),
		Pins: make([]StackRevisionPinBody, 0, len(rev.Pins)), Subnets: map[string]string{}, YAML: textDiffBody(rev.YAML),
	}
	maps.Copy(body.Subnets, rev.Subnets)
	// Only the path of a file is sent, whether plain or secret: the contents of a revision are its rendering.
	for _, f := range rev.Copy.Files {
		body.Files = append(body.Files, StackRevisionFileBody{Path: f.Path, Secret: f.Secret})
	}
	for _, v := range rev.Copy.Vars {
		value := v.Value
		if v.Secret {
			value = stacks.Hidden
		}
		body.Vars = append(body.Vars, StackRevisionVarBody{Name: v.Name, Value: value, Secret: v.Secret})
	}
	for _, p := range rev.Pins {
		body.Pins = append(body.Pins, StackRevisionPinBody{Service: pinService(st, p.Service), Image: p.Image, Digest: p.Digest})
	}
	return &StackRevisionDetailOutput{CacheControl: "no-store", Body: body}, nil
}

// StackRevisionChangesBody is what rolling back to a revision would change against what is running.
type StackRevisionChangesBody struct {
	DeployedSeq  int64                    `json:"deployedSeq" doc:"The number of the deployed revision (the newest good one) the revision is compared with; 0 when the stack has none"`
	Services     []StackReviewServiceBody `json:"services" doc:"The services a roll back would add, change or remove, by name, as the roll back review lists them. For a caller without secrets.reveal every environment value, command, argument, health check command and log option reads <hidden>"`
	Summary      StackReviewCountsBody    `json:"summary" doc:"How many services the roll back would add, change and remove"`
	Identical    bool                     `json:"identical" doc:"True when rolling back would change no service: the revision has the same content as what runs, or is the deployed revision"`
	DriftedPaths map[string][]string      `json:"driftedPaths" doc:"For each service of the roll back that the stack's newest drift check found drifted, by swarm service name (the name on a standalone host), the paths of the settings that check found to differ from the deployed revision, so the tab can say a setting differs because it was changed outside the stack and not because of the revision. Empty when the stack has not been checked since its newest deploy, start or stop. No value is sent"`
}

// StackRevisionChangesOutput wraps StackRevisionChangesBody for huma.
type StackRevisionChangesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRevisionChangesBody
}

// maxChangesPerUser is how many reads of a revision's changes one user may have running: each reads the swarm.
const maxChangesPerUser = 2

// newChangesLimiter is the limiter of the changes route.
func newChangesLimiter() *streamLimiter {
	return &streamLimiter{max: maxChangesPerUser, open: map[string]int{}}
}

func registerStackRevisionChanges(r *httpx.Router, d StacksDeps, lim *streamLimiter) {
	httpx.Register(r, huma.Operation{
		OperationID: "getStackRevisionChanges", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/revisions/{seq}/changes",
		Summary: "What rolling back to a revision would change",
		Description: "The services a roll back to the revision would add, change or remove against what is running now, the same comparison the roll back review makes (the revision converted with the digests it stored, no registry asked). " +
			"Only a revision that ended good qualifies: a pending or failed one is 409 revision_not_good, a number the stack does not hold is 404 revision_not_found, and a stack ID of another environment is 404 stack_not_found. " +
			"A revision that can no longer be redeployed is 409 revision_not_deployable. " +
			"The answer holds the live settings of running services, so for a caller without secrets.reveal on the environment every environment value, command, argument, health check command and log option is <hidden>, " +
			"and a setting that differs only in such a value is listed by its path in hiddenChanges, with no value. " +
			"One user may have two of these reads running; a third is 429 check_busy. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackRevisionInput) (*StackRevisionChangesOutput, error) {
			return getStackRevisionChanges(ctx, d, lim, in)
		})
}

func getStackRevisionChanges(ctx context.Context, d StacksDeps, lim *streamLimiter, in *StackRevisionInput) (*StackRevisionChangesOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	repo := d.repo()
	// The stack is read by its environment and ID, then the revision by (stack, seq): another environment's stack
	// or revision is not found (T-08-17).
	st, err := repo.Stack(ctx, env.ID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, env.ID, err)
	}
	target, err := restorableRevision(ctx, d, repo, env.ID, st.ID, in.Seq)
	if err != nil {
		return nil, err
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemCheckBusy, "Too many comparisons are running. Try again in a moment.")
	}
	defer release()
	if d.OnChanges != nil {
		d.OnChanges(ctx)
	}

	ri := stackops.ReviewInput{
		EnvID: env.ID, Stack: st, Copy: target.Copy, Mode: stackops.ReviewModeRollback, Target: &target,
		Pins: repo, Revisions: repo, Caller: stackops.Caller{Principal: p, Actor: p.Name}, SkipReverts: true,
	}
	if ri.TargetPins, err = repo.RevisionPins(ctx, target.ID); err != nil {
		return nil, stackInternal(d, "read the pins of the revision to compare", env.ID, err)
	}
	if ri.TargetRendering, err = repo.RevisionRendering(ctx, target.ID); err != nil {
		return nil, stackInternal(d, "read the rendering of the revision to compare", env.ID, err)
	}
	var deployedSeq int64
	if good, ok, err := repo.LatestGoodRevision(ctx, st.ID); err != nil {
		return nil, stackInternal(d, "read the running revision", env.ID, err)
	} else if ok {
		deployedSeq = good.Seq
		ri.Previous = &good.Copy
		ri.Deployed = &stackops.DeployedRevision{Seq: good.Seq, By: good.CreatedBy, At: good.CreatedAt}
		if ri.Rendering, err = repo.GoodRendering(ctx, st.ID); err != nil {
			return nil, stackInternal(d, "read the last rendering", env.ID, err)
		}
	}
	rv, err := d.ops().Review(ctx, ri)
	if err != nil {
		return nil, targetProblem(d, env.ID, err)
	}
	for _, pr := range rv.Problems {
		if pr.Code == stackops.CodeRevisionNotDeployable {
			// The sentence is masked by the review.
			return nil, httpx.Problem(http.StatusConflict, problemRevisionNotDeployable, pr.Message)
		}
	}
	// The services hold the live settings of what runs. Only a caller who may reveal secrets sees them (D-47).
	services := rv.Services
	if !redact.CanReveal(ctx, d.Checker, p, env.ID) {
		services = stackops.RedactChanges(services, st.Target)
	}
	return &StackRevisionChangesOutput{CacheControl: "no-store", Body: StackRevisionChangesBody{
		DeployedSeq: deployedSeq, Services: serviceBodies(services), Summary: countsBody(rv.Summary.Services), Identical: len(rv.Services) == 0,
		DriftedPaths: driftedPathsOf(d, st, deployedSeq),
	}}, nil
}

// driftedPathsOf are the paths the stack's cached drift check found to differ from the deployed revision, by service.
// It reads only the cache: a check that has not run, or that no longer describes the stack, gives none (D-21).
func driftedPathsOf(d StacksDeps, st stackops.Stack, deployedSeq int64) map[string][]string {
	if d.Drift == nil {
		return map[string][]string{}
	}
	cached, ok := d.Drift.Get(st.ID)
	if !ok || !driftCurrent(cached, st, deployedSeq) {
		return map[string][]string{}
	}
	return stackops.DriftPaths(cached)
}

// problemRevisionNotDeployable is the code of a revision that no longer loads, converts or has an image stored for
// each service.
const problemRevisionNotDeployable = stackops.CodeRevisionNotDeployable
