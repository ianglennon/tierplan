// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

const (
	stackTargetType = "stack"

	problemStackNameInvalid    = "stack_name_invalid"
	problemStackNameTaken      = "stack_name_taken"
	problemStackNotFound       = "stack_not_found"
	problemStackStartInvalid   = "stack_start_invalid"
	problemStackFileInvalid    = "stack_file_invalid"
	problemStackVarInvalid     = "stack_var_invalid"
	problemVariableNameInvalid = "variable_name_invalid"
	problemVariableNameTaken   = "variable_name_taken"
	problemSecretTooShort      = "secret_too_short"
	problemVariableNotFound    = "variable_not_found"
	problemFileNotFound        = "file_not_found"
	problemStackPlaceholder    = "stack_placeholder_without_value"
	problemFilePathInvalid     = "file_path_invalid"
	problemFileNameTaken       = "file_name_taken"
	problemFileTooLarge        = "file_too_large"
	problemStackTooLarge       = "stack_too_large"
	problemComposeMissing      = "compose_missing"
	problemDraftConflict       = "draft_conflict"
	problemTargetEngineAbsent  = "target_engine_missing"
	problemComposeInvalid      = "compose_invalid"
	problemComposeUnsupported  = "compose_unsupported"
	problemTooManyDeploys      = "too_many_deploys"
	problemStackJobNotFound    = "stack_job_not_found"
	problemNameClash           = "stack_name_clash"
	problemTargetChanged       = "target_changed"

	// The states a stack is listed in, from its newest operation.
	stackNotDeployed  = "not_deployed"
	stackDeploying    = "deploying"
	stackDeployFailed = "deploy_failed"
	stackDeployed     = "deployed"

	stackStartExample = "example"
	stackStartEmpty   = "empty"

	// maxStackBody is the largest request body of a stack route: a compose file and files beside it.
	maxStackBody = 8 << 20
)

// StacksDeps are the dependencies of the stack routes.
type StacksDeps struct {
	// Aggregator is the one write path of a cluster-scoped change: WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters reads the swarm a stack deploys to and says whether an environment is one.
	Clusters *envs.Clusters
	// Resolver chooses the registry credential each service is sent with.
	Resolver *registry.Resolver
	Audit    *audit.Writer
	Checker  *rbac.Checker
	// Jobs holds the deploys in progress. Nil answers 503 jobs_unavailable.
	Jobs *jobs.Hub
	// Store and Vault hold the stored stacks and seal their secret values.
	Store *store.Store
	Vault *vault.Vault
	// CheckDeadline bounds one check of a stack's working copy. Zero is five seconds.
	CheckDeadline time.Duration
	// OnCheck, when set, is called once a check holds its slot and before it runs. Tests use it to
	// hold a check in flight.
	OnCheck func(ctx context.Context)
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

func (d StacksDeps) repo() stackops.Repo { return stackops.Repo{Store: d.Store, Vault: d.Vault} }

func (d StacksDeps) ops() stackops.Deps {
	return stackops.Deps{Aggregator: d.Aggregator, Clusters: d.Clusters, Resolver: d.Resolver, Checker: d.Checker, Log: d.Log}
}

// StackItem is one stack as the list shows it.
type StackItem struct {
	ID             string `json:"id" doc:"The stack ID"`
	Name           string `json:"name" doc:"The stack name, also its swarm namespace or compose project"`
	Target         string `json:"target" enum:"swarm,standalone" doc:"Where the stack deploys: fixed when it was created, from the kind of the environment"`
	Source         string `json:"source" enum:"inline,adopted,git" doc:"Where the stack's definition lives. Only inline exists today"`
	State          string `json:"state" enum:"not_deployed,deploying,deploy_failed,deployed" doc:"Where the stack is, from its newest operation: not_deployed before the first deploy, deploying while one runs, deploy_failed when the newest one failed, was cancelled or was interrupted, deployed otherwise"`
	LastDeployedAt string `json:"lastDeployedAt" doc:"When the newest successful deploy ended, RFC 3339 UTC; empty when there is none"`
	LastDeployedBy string `json:"lastDeployedBy" doc:"Who started it; empty when there is none"`
	HasDraft       bool   `json:"hasDraft" doc:"True when the draft holds changes that have not been deployed"`
}

// StackListBody is the stacks of an environment.
type StackListBody struct {
	Items []StackItem `json:"items" doc:"The environment's stacks by name"`
}

// StackListOutput wraps StackListBody for huma.
type StackListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackListBody
}

// StackDetail is one stack.
type StackDetail struct {
	StackItem
	EnvID     string `json:"envId" doc:"The environment the stack belongs to"`
	SourceRef string `json:"sourceRef" doc:"A reference to the stack's source: empty for an inline stack"`
	CreatedBy string `json:"createdBy" doc:"Who created the stack"`
	CreatedAt string `json:"createdAt" doc:"When it was created, RFC 3339 UTC"`
	Stopped   bool   `json:"stopped" doc:"True while the stack is stopped"`
}

// StackDetailOutput wraps StackDetail for huma.
type StackDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackDetail
}

// StackPathInput is the path of a stack route.
type StackPathInput struct {
	EnvID   string `path:"envId" doc:"The environment ID"`
	StackID string `path:"stackId" doc:"The stack ID"`
}

// StackCreateRequest is the body of POST /api/env/{envId}/stacks.
type StackCreateRequest struct {
	Name    string `json:"name" maxLength:"200" doc:"The stack's name: lowercase letters, numbers, dash and underscore, starting with a letter or number, at most 63 characters. Anything else is 422 stack_name_invalid"`
	Start   string `json:"start,omitempty" doc:"What the first draft holds when compose is left out: example (the default) is a starter file with one web service, empty is an empty compose.yaml. Anything else is 422 stack_start_invalid"`
	Compose string `json:"compose,omitempty" maxLength:"1100000" doc:"The text of the first compose.yaml. When it is given, start is ignored"`
}

// StackCreateInput is the path and body of the create route.
type StackCreateInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      StackCreateRequest
}

// RegisterStacks registers the stack routes: list, create and read stacks, read and save a
// stack's draft, and deploy it. Saving or creating a stack sends nothing to any engine; only a
// deploy writes to Docker (STK-05).
func RegisterStacks(r *httpx.Router, d StacksDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listStacks", Method: http.MethodGet, Path: "/api/env/{envId}/stacks",
		Summary: "The stacks Bayplan manages on an environment",
		Description: "Every stack of the environment by name, with where it deploys, its state and when it was last deployed. " +
			"The state comes from Bayplan's own records of the stack's deploys, never from a read of the engine. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*StackListOutput, error) {
			return listStacks(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "createStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks",
		Summary: "Create a stack",
		Description: "Creates a stack and its first draft. Nothing is sent to any engine: the stack is Not deployed until it is deployed. " +
			"The target is fixed here from the environment: swarm when the environment is a swarm, standalone otherwise. " +
			"The name must match ^[a-z0-9][a-z0-9_-]{0,62}$ (422 stack_name_invalid) and be new on the environment (409 stack_name_taken). " +
			"A name that something Bayplan does not manage already uses on the environment (a swarm namespace or a Compose project with that name, without this stack's bayplan.stack.id) is 409 stack_name_clash. " +
			"start chooses the first compose.yaml: example (a starter with one service) or empty; compose overrides it. Audited as stack.created. Needs stacks.create (Admin).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxStackBody,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackCreateInput) (*StackDetailOutput, error) {
			return createStack(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getStack", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}",
		Summary:     "One stack",
		Description: "The stack with its state. A stack ID of another environment is 404 stack_not_found, as one that does not exist. Needs stacks.read (Viewer).",
		Tags:        []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackDetailOutput, error) {
			return getStack(ctx, d, in)
		})

	registerStackDraft(r, d)
	// A render is a check of the working copy followed by a view of what it loaded, so the two
	// share one limit of checks running per user.
	checkLimiter := &streamLimiter{max: maxChecksPerUser, open: map[string]int{}}
	registerStackCheck(r, d, checkLimiter)
	registerStackRender(r, d, checkLimiter)
	registerStackVariables(r, d)
	registerStackReview(r, d)
	registerStackDeploy(r, d)
}

// stackNotFound is the 404 for a stack the environment does not have.
func stackNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemStackNotFound, "This stack does not exist on this environment. It may have been removed.")
}

// stackInternal logs a server-side failure and answers 500.
func stackInternal(d StacksDeps, what, envID string, err error) error {
	d.Log.Error("a stack request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// stackState is the state of a stack from its newest operation.
func stackState(latest stackops.DeployRow, has bool) string {
	switch {
	case !has:
		return stackNotDeployed
	case latest.State == stackops.DeployRunning:
		return stackDeploying
	case latest.State == stackops.DeployDone:
		return stackDeployed
	default:
		return stackDeployFailed
	}
}

// stackItemOf builds a stack's list entry from its stored rows.
func stackItemOf(st stackops.Stack, latest stackops.DeployRow, hasLatest bool, last stackops.DeployRow, hasLast, hasDraft bool) StackItem {
	item := StackItem{
		ID: st.ID, Name: st.Name, Target: string(st.Target), Source: string(st.Source.Kind),
		State: stackState(latest, hasLatest), HasDraft: hasDraft,
	}
	if hasLast {
		item.LastDeployedAt, item.LastDeployedBy = rfc3339(last.EndedAt), last.StartedBy
	}
	return item
}

func listStacks(ctx context.Context, d StacksDeps, in *EnvironmentPathInput) (*StackListOutput, error) {
	repo := d.repo()
	list, err := repo.Stacks(ctx, in.EnvID)
	if err != nil {
		return nil, stackInternal(d, "list", in.EnvID, err)
	}
	latest, err := repo.LatestDeploys(ctx, in.EnvID)
	if err != nil {
		return nil, stackInternal(d, "latest deploys", in.EnvID, err)
	}
	done, err := repo.LastDeployed(ctx, in.EnvID)
	if err != nil {
		return nil, stackInternal(d, "last deployed", in.EnvID, err)
	}
	drafts, err := repo.DraftStates(ctx, in.EnvID)
	if err != nil {
		return nil, stackInternal(d, "drafts", in.EnvID, err)
	}
	body := StackListBody{Items: make([]StackItem, 0, len(list))}
	for _, st := range list {
		l, hasL := latest[st.ID]
		g, hasG := done[st.ID]
		body.Items = append(body.Items, stackItemOf(st, l, hasL, g, hasG, drafts[st.ID]))
	}
	return &StackListOutput{CacheControl: "no-store", Body: body}, nil
}

func getStack(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackDetailOutput, error) {
	st, err := d.repo().Stack(ctx, in.EnvID, in.StackID)
	if errors.Is(err, stackops.ErrStackNotFound) {
		return nil, stackNotFound()
	}
	if err != nil {
		return nil, stackInternal(d, "read", in.EnvID, err)
	}
	body, err := stackDetailOf(ctx, d, st)
	if err != nil {
		return nil, stackInternal(d, "detail", in.EnvID, err)
	}
	return &StackDetailOutput{CacheControl: "no-store", Body: body}, nil
}

// stackDetailOf reads the rows a stack's detail is built from.
func stackDetailOf(ctx context.Context, d StacksDeps, st stackops.Stack) (StackDetail, error) {
	repo := d.repo()
	latest, hasLatest, err := repo.LatestDeploy(ctx, st.ID)
	if err != nil {
		return StackDetail{}, err
	}
	done, err := repo.LastDeployed(ctx, st.EnvID)
	if err != nil {
		return StackDetail{}, err
	}
	drafts, err := repo.DraftStates(ctx, st.EnvID)
	if err != nil {
		return StackDetail{}, err
	}
	g, hasG := done[st.ID]
	return StackDetail{
		StackItem: stackItemOf(st, latest, hasLatest, g, hasG, drafts[st.ID]),
		EnvID:     st.EnvID, SourceRef: st.Source.Ref, CreatedBy: st.CreatedBy, CreatedAt: rfc3339(st.CreatedAt), Stopped: st.Stopped,
	}, nil
}

func createStack(ctx context.Context, d StacksDeps, in *StackCreateInput) (*StackDetailOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	name := in.Body.Name
	if !stacks.NameValid(name) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackNameInvalid,
			"Use lowercase letters, numbers, dash or underscore, and start with a letter or number.")
	}
	compose := in.Body.Compose
	if compose == "" {
		switch in.Body.Start {
		case "", stackStartExample:
			compose = stacks.StarterCompose
		case stackStartEmpty:
		default:
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackStartInvalid, "Start from the starter example or an empty file.")
		}
	}
	if len(compose) > stacks.MaxFileBytes {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackFileInvalid, "compose.yaml is over 1 MiB.")
	}
	if d.Clusters == nil {
		d.Log.Error("the stack routes have no cluster reader")
		return nil, huma.Error500InternalServerError("internal error")
	}
	swarm, err := d.Clusters.IsSwarm(ctx, env.ID)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	target := stacks.TargetStandalone
	if swarm {
		target = stacks.TargetSwarm
	}

	// A name the environment already uses for something Bayplan does not manage is refused (D-12):
	// Bayplan never deploys into, takes over or changes such a stack. A name one of Bayplan's own
	// stacks holds is the create's own refusal below, and its objects are never a clash.
	own, err := d.repo().Stacks(ctx, env.ID)
	if err != nil {
		return nil, stackInternal(d, "list", env.ID, err)
	}
	if !slices.ContainsFunc(own, func(s stackops.Stack) bool { return s.Name == name }) {
		foreign, err := d.ops().ForeignStack(ctx, env.ID, target, name, "")
		if err != nil {
			return nil, snapshotProblem(d.Log, env.ID, err)
		}
		if foreign {
			return nil, &huma.ErrorModel{
				Type: problemNameClash, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
				Detail: stackops.NameClashMessage(name),
				Errors: []*huma.ErrorDetail{{Location: "name", Message: name}},
			}
		}
	}

	st, _, err := d.repo().CreateStack(ctx, env.ID, name, target, p.Name, stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}},
	})
	if errors.Is(err, stackops.ErrNameTaken) {
		return nil, &huma.ErrorModel{
			Type: problemStackNameTaken, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "A stack named " + name + " already exists on " + env.Name + ".",
			Errors: []*huma.ErrorDetail{{Location: "name", Message: name}},
		}
	}
	if err != nil {
		return nil, stackInternal(d, "create", env.ID, err)
	}
	recordStack(ctx, d, p, env.ID, in.UserAgent, audit.ActionStackCreated, st.ID, audit.ResultSuccess, "",
		map[string]string{"stack": name, "target": string(target)})

	body, err := stackDetailOf(ctx, d, st)
	if err != nil {
		return nil, stackInternal(d, "detail", env.ID, err)
	}
	return &StackDetailOutput{CacheControl: "no-store", Body: body}, nil
}

// recordStack writes one audit row about a stack. The request's values are kept, its
// cancellation is not: a browser that closes must not lose the record.
func recordStack(ctx context.Context, d StacksDeps, p *rbac.Principal, envID, userAgent, action, stackID string, result audit.Result, reason string, details map[string]string) {
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: stackTargetType, TargetID: stackID, EnvID: envID,
		Result: result, Reason: reason, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a stack action", "action", action, "stack", stackID, "error", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
