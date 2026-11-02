// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/gitconn"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/instance"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/vault"
)

const (
	stackTargetType = "stack"

	problemStackNameInvalid    = "stack_name_invalid"
	problemStackNameTaken      = "stack_name_taken"
	problemStackNotFound       = "stack_not_found"
	problemStackStartInvalid   = "stack_start_invalid"
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
	problemRepoFileReadOnly    = "repo_file_read_only"
	problemTargetEngineAbsent  = "target_engine_missing"
	problemComposeInvalid      = "compose_invalid"
	problemComposeUnsupported  = "compose_unsupported"
	problemTooManyDeploys      = "too_many_deploys"
	problemStackJobNotFound    = "stack_job_not_found"
	problemNameClash           = "stack_name_clash"
	problemTargetChanged       = "target_changed"
	problemJobFinished         = "job_finished"
	problemOperationLocked     = "operation_locked"

	stackStartExample = "example"
	stackStartEmpty   = "empty"
	stackStartUpload  = "upload"

	// maxStackBody is the largest request body of a stack route: a compose file and files beside it.
	maxStackBody = 8 << 20
)

// stackNameInvalidMessage is the sentence for a name the rule of target refuses, on create and on the name
// check. It is built from stacks.NameRuleText, as the not-adoptable reason of a discovered stack is, so the
// two cannot word the rule differently (D-53).
func stackNameInvalidMessage(target stacks.Target) string {
	return "Use only " + stacks.NameRuleText(target) + "."
}

// stackTargetOf is the target of an environment from its stored facts: a swarm or a standalone host.
func (d StacksDeps) stackTargetOf(ctx context.Context, envID string) (stacks.Target, error) {
	if d.Clusters == nil {
		d.Log.Error("the stack routes have no cluster reader")
		return "", huma.Error500InternalServerError("internal error")
	}
	swarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return "", snapshotProblem(d.Log, envID, err)
	}
	if swarm {
		return stacks.TargetSwarm, nil
	}
	return stacks.TargetStandalone, nil
}

// stackNameTaken reports whether one of the environment's managed stacks holds name. Swarm looks object
// names up regardless of case, so on a swarm two names that differ only in case are one (T-08-95).
func stackNameTaken(own []stackops.Stack, target stacks.Target, name string) bool {
	_, taken := stackops.HoldingName(own, target, name)
	return taken
}

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
	// Clock and ConvergeTimeout are what a swarm deploy waits on and for: the clock its polls and
	// its timeout run on (nil is the wall clock), and how long each service gets to settle (zero is
	// ten minutes; TIERPLAN_STACK_CONVERGE_TIMEOUT).
	Clock           stackops.Clock
	ConvergeTimeout time.Duration
	// Platforms is the server's one memo of the platforms its image lookups found, by repository and digest (an answer of
	// none too): a review fills it and the deploy that follows reads it, so that deploy asks no registry again (G-08-4). Nil is no memo: a reviewed deploy asks
	// the registry once for each looked-up digest.
	Platforms *stackops.PlatformMemo
	// CheckDeadline bounds one check of a stack's working copy. Zero is five seconds.
	CheckDeadline time.Duration
	// OnCheck, when set, is called once a check holds its slot and before it runs. Tests use it to
	// hold a check in flight.
	OnCheck func(ctx context.Context)
	// OnChanges, when set, is called once a read of a revision's changes holds its slot and before it runs. Tests
	// use it to hold one in flight.
	OnChanges func(ctx context.Context)
	// OnAdoptCheck, when set, is called once an adopt check holds its slot and before it runs. Tests use it to hold
	// one in flight.
	OnAdoptCheck func(ctx context.Context)
	// OnDrift, when set, is called once a read of a stack for drift holds its slot and before it runs. Tests use it
	// to hold one in flight.
	OnDrift func(ctx context.Context)
	// Drift holds the newest drift check of each stack, in memory. Nil means a store of this deps' own.
	Drift *stackops.DriftStore
	// DriftRecheck, when set, is called with the environment and the stack when an operation on a stack has ended and
	// freed the environment's lock, so the background drift check looks at the stack again (D-22).
	DriftRecheck func(envID, stackID string)
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
	// AddressSpaces reads how full the address pools of the target are, for the compose
	// lint's address hint and the small-subnet assignment of a deploy. Nil means the feature
	// is off: the checks that need a pool are skipped.
	AddressSpaces *envs.AddressSpaces
	// GitConns and GitSync fill the git part of a git stack's list entry and detail: the repository's name and
	// address, and whether a sync check runs now. Nil leaves them empty.
	GitConns *gitconn.Store
	GitSync  *gitsync.Service
	// GitDeployed, when set, is told that a revision of a stack ended good, so that a git stack's settings and sync state
	// follow what was deployed (gitsync.Service.Deployed). It does nothing for a revision not made from a repository.
	GitDeployed func(ctx context.Context, stackID string, rev stackops.Snapshot)
	// GitRolledBack, when set, is told that a person rolled a stack back and the revision of the roll back exists, so that
	// a git stack whose automatic deploys are on pauses them and the repository head is not deployed over the restored
	// revision (gitsync.Service.RolledBack). It does nothing for a stack that is not from a repository.
	GitRolledBack func(ctx context.Context, st stackops.Stack, rev stackops.Snapshot, actor audit.Actor)
	// GitDeployFailed, when set, is told that the deploy of a revision ended failed, with the service that failed, so
	// that a git stack whose revision an automatic deploy made goes to error (gitsync.Service.DeployFailed). It does
	// nothing for a revision a person deployed.
	GitDeployFailed func(ctx context.Context, st stackops.Stack, rev stackops.Snapshot, failedService string)
	// Envs resolves the environment of a deploy that has no request (StackDeployer); the routes get theirs from the
	// router. Nil makes such a deploy refuse with environment_not_found.
	Envs httpx.EnvResolver
	// SelfContainerID is the ID of the container TierPlan runs in and SelfNamespace the swarm namespace of
	// that container; either is empty when TierPlan cannot tell. The lists flag the stack that holds them (D-49).
	SelfContainerID, SelfNamespace string
	// RevisionsKept is how many revisions of each stack are kept (D-01); the server passes the instance
	// setting. Nil means the default of 20.
	RevisionsKept func() int
	// ReadEnv, when set, is called before the stacks of an environment are read for the All stacks list;
	// an error makes that environment a failed one. Tests use it to fail one environment's read.
	ReadEnv func(ctx context.Context, envID string) error
	// Heartbeat is how often an idle deploy stream gets a ping. Zero means 15 seconds.
	Heartbeat time.Duration
	// MaxLifetime is the longest one deploy stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// MaxStreamsPerUser caps concurrent deploy streams per user. Zero means 4.
	MaxStreamsPerUser int

	// Logs tunes the log stream and the download of a stack's merged log; every zero value is the default.
	Logs StackLogsTuning

	// Locks is the per-environment operation lock: one stack operation at a time runs on an environment.
	// The janitor of Phase 11 takes the same one. Nil means a lock of this deps' own.
	Locks stackops.OperationLock
	// BeforeJob, when set, is called in a deploy after it has been judged and recorded and just before its job
	// starts. Tests use it to change the world between the check and the job.
	BeforeJob func(ctx context.Context)
	// Up, when set, replaces the Compose SDK's up in a standalone deploy (stackops.Deps.Up). Tests use it:
	// the SDK needs a real engine and is proved against one.
	Up func(ctx context.Context, c *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error
	// StopProject and StartProject, when set, replace the Compose SDK's stop and start of a standalone stack
	// (stackops.Deps). Tests use them for the same reason as Up.
	StopProject, StartProject func(ctx context.Context, c *dockerx.Client, p *types.Project) error
	// DownProject, when set, replaces the Compose SDK's down in the removal of a standalone stack
	// (stackops.Deps.DownProject), for the same reason.
	DownProject func(ctx context.Context, c *dockerx.Client, project string) error

	// cancels remembers who cancelled which deploy until the deploy ends and is audited. RegisterStacks
	// makes it; every copy of the deps shares it.
	cancels *deployCancels
}

func (d StacksDeps) repo() stackops.Repo { return stackops.Repo{Store: d.Store, Vault: d.Vault} }

// revisionsKept is the retention limit now: the instance's setting, or the default when none is wired.
func (d StacksDeps) revisionsKept() int {
	if d.RevisionsKept != nil {
		if n := d.RevisionsKept(); n >= 1 {
			return n
		}
	}
	return instance.DefaultStackRevisionsKept
}

func (d StacksDeps) ops() stackops.Deps {
	return stackops.Deps{
		Aggregator: d.Aggregator, Clusters: d.Clusters, Resolver: d.Resolver, Checker: d.Checker,
		Clock: d.Clock, ConvergeTimeout: d.ConvergeTimeout, Platforms: d.Platforms, AddressSpaces: d.AddressSpaces, Up: d.Up, StopProject: d.StopProject, StartProject: d.StartProject, DownProject: d.DownProject, Log: d.Log,
	}
}

// StackItem is one stack as the list shows it.
type StackItem struct {
	ID             string `json:"id" doc:"The stack ID"`
	Name           string `json:"name" doc:"The stack name, also its swarm namespace or compose project"`
	Target         string `json:"target" enum:"swarm,standalone" doc:"Where the stack deploys: fixed when it was created, from the kind of the environment"`
	Source         string `json:"source" enum:"inline,adopted,git,unmanaged" doc:"Where the stack's definition lives: inline (made in TierPlan), adopted (taken over from the engine, see the adopt route) or git. unmanaged is a stack found on the engine that TierPlan did not make (see unmanaged)"`
	Managed        bool   `json:"managed" doc:"True for a stack TierPlan manages; false for one found on the engine by its labels that TierPlan did not make. An unmanaged item has an empty id, only the state and the service counts, and no draft or last deploy"`
	State          string `json:"state" enum:"removing,deploying,rolling_back,starting,stopping,stopped,deploy_failed,not_running,degraded,running,not_deployed,unknown" doc:"The one state the stack shows everywhere, in this priority: removing, deploying, rolling_back, starting or stopping while that operation runs; stopped when a person stopped it; deploy_failed when the newest deploy, update or roll back failed, was cancelled or was interrupted (it stays until a later one succeeds, so a failure is never hidden by degraded or running); not_deployed when nothing was ever deployed; unknown when it was deployed and the engine could not be read; then not_running, degraded or running by how many of its services run"`
	LastDeployedAt string `json:"lastDeployedAt" doc:"When the newest successful deploy ended, RFC 3339 UTC; empty when there is none"`
	LastDeployedBy string `json:"lastDeployedBy" doc:"Who started it; empty when there is none"`
	// LastDeployedTrigger says the newest successful deploy was an automatic one.
	LastDeployedTrigger string `json:"lastDeployedTrigger,omitempty" enum:"poll,webhook" doc:"Left out unless set: what started the newest successful deploy when no person did, a poll check or a webhook. Then lastDeployedBy is the system actor and not a person"`
	HasDraft            bool   `json:"hasDraft" doc:"True when the draft holds changes that have not been deployed"`
	// ServicesRunning and ServicesTotal are the engine's own count of the stack's services, read from the
	// cache the lists use. They are left out for a stack that was never deployed and for one the engine could not be read for.
	ServicesRunning *int `json:"servicesRunning,omitempty" doc:"Services of the stack that run now; left out for a stack not deployed and when the engine could not be read"`
	ServicesTotal   *int `json:"servicesTotal,omitempty" doc:"Services the stack has on the engine; left out for a stack not deployed and when the engine could not be read"`
	// Drift is the newest drift check of a stack TierPlan manages.
	Drift *StackDriftSummaryBody `json:"drift,omitempty" doc:"Left out until the stack has been checked for drift since its newest deploy, start or stop (by the background check or by someone opening its drift page), and for an unmanaged stack: the newest check, as the drift route answers it without the services"`
	// Git is where a git stack stands against its repository.
	Git *StackGitSummary `json:"git,omitempty" doc:"Left out unless the stack is from a git repository: what it follows, the commit that is deployed, how far behind the repository's folder is, the status of the last check and why it failed, as the stack's git route answers"`
	// Unmanaged is set on a stack found on the engine that TierPlan did not make (D-07).
	Unmanaged *StackUnmanagedBody `json:"unmanaged,omitempty" doc:"Set on an unmanaged stack only: what was found about it"`
}

// StackUnmanagedBody is what the lists say about a stack TierPlan found on the engine and does not manage.
type StackUnmanagedBody struct {
	Orphan    bool   `json:"orphan" doc:"True when an object of the stack carries a tierplan.stack.id: TierPlan made it once, for a record this instance does not hold (another instance or a lost record). The ID is never linked to a stack"`
	Portainer bool   `json:"portainer" doc:"True when the stack carries a Portainer marker: a label key starting io.portainer., or a Compose working directory under /data/compose/"`
	Owner     string `json:"owner" enum:",tierplan,agent" doc:"tierplan for the stack the TierPlan server itself runs in, agent for a stack that holds a TierPlan agent, empty for any other"`
	Adoptable bool   `json:"adoptable" doc:"False for TierPlan's own stack, an agent's and a stack whose name the stack-name rule of its target refuses"`
	Reason    string `json:"reason" doc:"Why the stack cannot be adopted; empty when it can"`
}

// StackKindCountsBody is how many stacks the Show toggle counts. They are counted before the search, the
// status filter and the kind, so the numbers never shrink with what was typed.
type StackKindCountsBody struct {
	All       int `json:"all" doc:"Managed and unmanaged stacks together"`
	Managed   int `json:"managed" doc:"Stacks TierPlan manages"`
	Unmanaged int `json:"unmanaged" doc:"Stacks found on the engine that TierPlan did not make"`
}

// StackListBody is the stacks of an environment.
type StackListBody struct {
	Items           []StackItem           `json:"items" doc:"At most 500 stacks that match the search and the status filter, in the order asked for (by name unless sort says otherwise)"`
	Total           int                   `json:"total" doc:"How many stacks match the search and the status filter, whatever the cap"`
	UnfilteredTotal int                   `json:"unfilteredTotal" doc:"How many stacks the environment has before the search and the status filter, so a page can say 3 of 600"`
	Capped          bool                  `json:"capped" doc:"True when total is more than the rows returned"`
	StatusCounts    StackStatusCountsBody `json:"statusCounts" doc:"How many stacks each status chip counts, before the search and the status filter, so the numbers never shrink with what was typed"`
	KindCounts      StackKindCountsBody   `json:"kindCounts" doc:"How many stacks the Show toggle counts (all, managed, unmanaged), before the search, the status filter and the kind"`
	DiscoveryFailed bool                  `json:"discoveryFailed" doc:"True when the look for unmanaged stacks failed just now: the managed stacks are listed all the same and the unmanaged ones are missing"`
}

// StackListInput is the path of the stacks list with its search, status filter and sort.
type StackListInput struct {
	EnvironmentPathInput
	StackQueryInput
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
	// DraftSavedAt and DraftSavedBy say who last saved the draft that holds changes not yet deployed.
	DraftSavedAt string `json:"draftSavedAt" doc:"When the draft that holds changes that have not been deployed was last saved, RFC 3339 UTC; empty when there is no such draft"`
	DraftSavedBy string `json:"draftSavedBy" doc:"Who saved it, as a name; empty when there is no such draft"`
	// Lock is set while a stack operation runs on the environment, whichever stack it is on.
	Lock *StackLockBody `json:"lock,omitempty" doc:"Left out unless set: set while a stack operation runs on this environment, on any of its stacks, and names it. Only one runs at a time: a second is refused 409 operation_locked"`
	// Rollback is whether a roll back has a version to restore, and which.
	Rollback StackRollbackBody `json:"rollback" doc:"Whether a roll back has a version to restore: the newest version that ran well, or, when the newest version is the one that runs, the one before it"`
	// LastOperation is the newest operation, left out for a stack never deployed.
	LastOperation *StackOperationBody `json:"lastOperation,omitempty" doc:"Left out for a stack that has had no operation: the newest one, which the state is read from"`
	// Adoption is set on a stack TierPlan adopted.
	Adoption *StackAdoptionBody `json:"adoption,omitempty" doc:"Left out unless the stack was adopted (source adopted): whether its first deploy is still to come"`
	// DeployedRevision is the revision the stack stands at.
	DeployedRevision int64 `json:"deployedRevision,omitempty" doc:"Left out for a stack with no revision that deployed: the number of the newest one that did, the revision the stack stands at"`
}

// StackAdoptionBody is where an adopted stack stands.
type StackAdoptionBody struct {
	Pending     bool   `json:"pending" doc:"True until the stack's first good deploy: its services do not carry TierPlan's ownership labels yet, and that deploy adds them and recreates each service (a brief restart)"`
	AdoptedFrom string `json:"adoptedFrom" doc:"The name of the stack on the engine that was adopted"`
}

// StackLockBody is who holds the environment's stack operation lock.
type StackLockBody struct {
	StackID   string `json:"stackId" doc:"The stack the running operation is on"`
	StackName string `json:"stackName" doc:"Its name"`
	JobID     string `json:"jobId" doc:"The job that runs the operation: open its console at .../stacks/{stackId}/deploys/{jobId}. Empty for the instant between the lock being taken and the job starting"`
	Kind      string `json:"kind" enum:"deploy,update,rollback,start,stop,remove,adopt,janitor" doc:"What the operation is. janitor is a clean-up of the environment, which names no stack"`
	StartedAt string `json:"startedAt" doc:"When it started, RFC 3339 UTC"`
	StartedBy string `json:"startedBy" doc:"Who started it, as a name"`
}

// StackRollbackBody is the version a roll back would restore.
type StackRollbackBody struct {
	Available  bool   `json:"available" doc:"True when there is an earlier version that ran well to roll back to"`
	DeployedAt string `json:"deployedAt" doc:"When that version's deploy was started, RFC 3339 UTC; empty when there is none"`
	DeployedBy string `json:"deployedBy" doc:"Who started it, as a name; empty when there is none"`
	Revision   int64  `json:"revision,omitempty" doc:"Left out when there is none: the number of that revision, the one a roll back with no body would restore"`
}

// StackOperationBody is the newest operation on a stack: the one the state comes from.
type StackOperationBody struct {
	Kind          string `json:"kind" enum:"deploy,update,rollback,start,stop,remove,adopt" doc:"What the operation was"`
	State         string `json:"state" enum:"running,done,failed,cancelled,interrupted" doc:"Where it is. interrupted is a deploy that was running when the server stopped: the swarm may be partly updated"`
	Code          string `json:"code" doc:"A stable word for how a failed operation failed, such as service_rejected, converge_timeout or interrupted; empty otherwise"`
	FailedService string `json:"failedService" doc:"The service, network, secret or config a failed operation stopped at; empty otherwise"`
	JobID         string `json:"jobId" doc:"The job that ran it, open at .../deploys/{jobId} while the hub still has it"`
	StartedBy     string `json:"startedBy" doc:"Who started it, as a name"`
	StartedAt     string `json:"startedAt" doc:"When it started, RFC 3339 UTC"`
	EndedAt       string `json:"endedAt" doc:"When it ended, RFC 3339 UTC; empty while it runs"`
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
	Name    string `json:"name" maxLength:"255" doc:"The stack's name. On a swarm: letters, numbers, dash and underscore, starting with a letter or number, up to 61 characters. On a standalone host: lowercase letters, numbers, dash and underscore, starting with a letter or number, up to 255 characters. Anything else is 422 stack_name_invalid"`
	Start   string `json:"start,omitempty" doc:"What the first draft holds when compose is left out: example (the default) is a starter file with one web service, empty is an empty compose.yaml, upload is the file the person chose (compose, which may be empty). Anything else is 422 stack_start_invalid"`
	Compose string `json:"compose,omitempty" maxLength:"1100000" doc:"The text of the first compose.yaml. When it is given, start is ignored; with start upload it is the uploaded file as it was read, kept even when it does not parse (the editor shows the problems). Over 1 MiB is 422 file_too_large"`
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
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	if d.Locks == nil {
		d.Locks = stackops.NewLocks()
	}
	if d.cancels == nil {
		d.cancels = &deployCancels{by: map[string]string{}}
	}
	if d.Drift == nil {
		d.Drift = stackops.NewDriftStore()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listStacks", Method: http.MethodGet, Path: "/api/env/{envId}/stacks",
		Summary: "The stacks of an environment: those TierPlan manages and those it found",
		Description: "Every stack of the environment by name, with where it deploys, its state and when it was last deployed. " +
			"The state comes from TierPlan's own records of the stack's deploys and the engine's cached rows, never from a read of the engine of its own. " +
			"Stacks TierPlan did not make (a swarm namespace or a Compose project made by docker stack deploy, docker compose up or Portainer) are listed beside them with managed false and an unmanaged body; " +
			"they are found by their labels in the same cached rows and nothing about them is stored. kind (managed or unmanaged) lists one kind only; kindCounts counts both. " +
			"discoveryFailed is true when the look for them failed, and the managed stacks are listed all the same. " +
			"At most 500 stacks come back after the search (q: a stack name or one of its service names), the status filter (status: degraded, stopped, draft, deploy_failed, drifted, behind, sync-error) and the sort, " +
			"with total the number that matched, unfilteredTotal the number before them and statusCounts the chip numbers, counted before them too. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackListInput) (*StackListOutput, error) {
			return listStacks(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "createStack", Method: http.MethodPost, Path: "/api/env/{envId}/stacks",
		Summary: "Create a stack",
		Description: "Creates a stack and its first draft. Nothing is sent to any engine: the stack is Not deployed until it is deployed. " +
			"The target is fixed here from the environment: swarm when the environment is a swarm, standalone otherwise. " +
			"The name must follow the rule of the environment's target, the same rule that decides whether a stack made elsewhere can be adopted: on a swarm letters, numbers, dash and underscore, starting with a letter or number, up to 61 characters; on a standalone host lowercase letters, numbers, dash and underscore, starting with a letter or number, up to 255 characters (422 stack_name_invalid). It must be new on the environment (409 stack_name_taken; on a swarm names that differ only in case are one name). " +
			"A name that something TierPlan does not manage already uses on the environment (a swarm namespace or a Compose project with that name, without this stack's tierplan.stack.id) is 409 stack_name_clash. " +
			"start chooses the first compose.yaml: example (a starter with one service), empty, or upload (the file in compose, kept as it is even when it does not parse; over 1 MiB is 422 file_too_large); compose overrides example and empty. Audited as stack.created. Needs stacks.create (Admin).",
		Tags: []string{"stacks"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxStackBody,
	}, rbac.Requirement{Perm: rbac.PermStacksCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackCreateInput) (*StackDetailOutput, error) {
			return createStack(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getStack", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}",
		Summary:     "One stack",
		Description: "The stack with its state, derived as on the list, and who saved its pending draft. A stack ID of another environment is 404 stack_not_found, as one that does not exist. Needs stacks.read (Viewer).",
		Tags:        []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackDetailOutput, error) {
			return getStack(ctx, d, in)
		})

	registerAllStacks(r, d)
	registerStackNameCheck(r, d)
	registerStackDraft(r, d)
	// A render is a check of the working copy followed by a view of what it loaded, so the two
	// share one limit of checks running per user.
	checkLimiter := &streamLimiter{max: maxChecksPerUser, open: map[string]int{}}
	registerStackCheck(r, d, checkLimiter)
	registerStackRender(r, d, checkLimiter)
	registerStackVariables(r, d)
	registerStackReview(r, d)
	registerStackDeploy(r, d)
	registerStackLifecycle(r, d)
	registerStackRemove(r, d)
	registerStackJobs(r, d)
	registerStackOverview(r, d)
	registerStackResources(r, d)
	registerStackHistory(r, d, newChangesLimiter())
	registerStackDrift(r, d, newDriftLimiter())
	registerUnmanagedStacks(r, d, registerStackLogs(r, d))
	registerStackAdopt(r, d, newAdoptCheckLimiter())
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

// stackReads reads what an environment's engine holds for the stacks of a list or a page, once for each
// kind of target, from the caches the lists are made from. A read that fails leaves the stacks that need it
// without a state read from the engine: they are Unknown, never Running or Not running.
type stackReads struct {
	ctx   context.Context
	d     StacksDeps
	envID string
	held  map[bool]heldRead
}

type heldRead struct {
	held stackops.Held
	err  error
}

func newStackReads(ctx context.Context, d StacksDeps, envID string) *stackReads {
	return &stackReads{ctx: ctx, d: d, envID: envID, held: map[bool]heldRead{}}
}

// heldOf is what the environment holds for one kind of target, read once.
func (r *stackReads) heldOf(swarm bool) heldRead {
	h, ok := r.held[swarm]
	if !ok {
		h.held, h.err = r.d.ops().Cached(r.ctx, r.envID, swarm)
		if h.err != nil {
			r.d.Log.Debug("could not read what an environment runs for its stacks", "environment", r.envID, "error", h.err)
		}
		r.held[swarm] = h
	}
	return h
}

// rows are the services the stack owns, and false when they could not be read.
func (r *stackReads) rows(st stackops.Stack) ([]stackops.ServiceRow, bool) {
	h := r.heldOf(st.Target == stacks.TargetSwarm)
	if h.err != nil {
		return nil, false
	}
	return h.held.RowsOf(st), true
}

// stackItemOf builds a stack's list entry from its stored rows and what the engine runs.
func stackItemOf(reads *stackReads, st stackops.Stack, latest stackops.DeployRow, hasLatest bool, like stackops.DeployRow, hasLike bool, last stackops.DeployRow, hasLast, hasDraft bool) StackItem {
	item, _ := stackItemWithServices(reads, st, latest, hasLatest, like, hasLike, last, hasLast, hasDraft)
	return item
}

// stackItemWithServices is stackItemOf with the services the stack owns on the engine, which a search
// reads. They are nil for a stack that has had no operation and for one the engine could not be read for.
// latest is the newest operation of any kind and like the newest deploy, update or roll back: a stop or a start
// after a deploy that failed must not hide the failure.
func stackItemWithServices(reads *stackReads, st stackops.Stack, latest stackops.DeployRow, hasLatest bool, like stackops.DeployRow, hasLike bool, last stackops.DeployRow, hasLast, hasDraft bool) (StackItem, []stackops.ServiceRow) {
	// A stack TierPlan adopted runs on the engine from the start, with revision 1 as its good revision (D-14): it
	// reads as deployed before any deploy of its own.
	adopted := st.Source.Kind == stacks.SourceAdopted
	in := stackops.StateInput{Stopped: st.Stopped, Deployed: hasLast || adopted}
	if hasLatest {
		in.Latest = &latest
	}
	if hasLike {
		in.LatestDeploy = &like
	}
	// A stack that has had no operation has nothing on the engine to read, unless it was adopted.
	if hasLatest || hasLast || adopted {
		rows, known := reads.rows(st)
		in.Services, in.Unknown = rows, !known
	}
	state, running, total := stackops.DeriveState(in)
	item := StackItem{
		ID: st.ID, Name: st.Name, Target: string(st.Target), Source: string(st.Source.Kind), Managed: true,
		State: string(state), HasDraft: hasDraft,
	}
	if state != stackops.StateNotDeployed && state != stackops.StateUnknown && !in.Unknown {
		item.ServicesRunning, item.ServicesTotal = &running, &total
	}
	if hasLast {
		item.LastDeployedAt, item.LastDeployedBy, item.LastDeployedTrigger = rfc3339(last.EndedAt), last.StartedBy, last.Trigger
	}
	return item, in.Services
}

func listStacks(ctx context.Context, d StacksDeps, in *StackListInput) (*StackListOutput, error) {
	q, err := newStackQuery(in.StackQueryInput)
	if err != nil {
		return nil, err
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, in.EnvID)
	if err != nil {
		return nil, stackInternal(d, "list: read the environment", in.EnvID, err)
	}
	read, err := d.stackRowsOf(ctx, in.EnvID, "", row.Kind == envs.KindLocal)
	if err != nil {
		return nil, stackInternal(d, "list", in.EnvID, err)
	}
	rows := read.rows
	shown, total := q.apply(rows)
	body := StackListBody{
		Items: make([]StackItem, 0, len(shown)), Total: total, UnfilteredTotal: len(rows),
		Capped: total > len(shown), StatusCounts: stackStatusCounts(rows),
		KindCounts: stackKindCounts(rows), DiscoveryFailed: read.discoveryFailed,
	}
	for _, r := range shown {
		body.Items = append(body.Items, r.item)
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
	like, hasLike, err := repo.LatestDeployLike(ctx, st.ID)
	if err != nil {
		return StackDetail{}, err
	}
	done, err := repo.LastDeployed(ctx, st.EnvID)
	if err != nil {
		return StackDetail{}, err
	}
	drafts, err := repo.DraftSaves(ctx, st.EnvID)
	if err != nil {
		return StackDetail{}, err
	}
	g, hasG := done[st.ID]
	draft := drafts[st.ID]
	detail := StackDetail{
		StackItem: stackItemOf(newStackReads(ctx, d, st.EnvID), st, latest, hasLatest, like, hasLike, g, hasG, draft.Pending),
		EnvID:     st.EnvID, SourceRef: st.Source.Ref, CreatedBy: st.CreatedBy, CreatedAt: rfc3339(st.CreatedAt), Stopped: st.Stopped,
	}
	if draft.Pending {
		detail.DraftSavedAt, detail.DraftSavedBy = rfc3339(draft.At), draft.By
	}
	if st.Source.Kind == stacks.SourceGit {
		if detail.Git, err = d.gitSummaryOfStack(ctx, st.ID); err != nil {
			return StackDetail{}, err
		}
	}
	back, hasBack, err := repo.RollbackInfo(ctx, st.ID)
	if err != nil {
		return StackDetail{}, err
	}
	if hasBack {
		detail.Rollback = StackRollbackBody{Available: true, DeployedAt: rfc3339(back.At), DeployedBy: back.By, Revision: back.Seq}
	}
	if hasLatest {
		detail.LastOperation = &StackOperationBody{
			Kind: latest.Kind, State: latest.State, Code: latest.Code, FailedService: latest.FailedService, JobID: latest.JobID,
			StartedBy: latest.StartedBy, StartedAt: rfc3339(latest.StartedAt), EndedAt: rfc3339(latest.EndedAt),
		}
	}
	if h, ok := d.Locks.Held(st.EnvID); ok {
		detail.Lock = lockBody(h)
	}
	if st.Source.Kind == stacks.SourceAdopted {
		detail.Adoption = &StackAdoptionBody{Pending: st.Adoption.Pending, AdoptedFrom: st.Source.Ref}
	}
	deployed, hasDeployed, err := repo.GoodRevisionInfo(ctx, st.ID)
	if err != nil {
		return StackDetail{}, err
	}
	if hasDeployed {
		detail.DeployedRevision = deployed.Seq
	}
	detail.Drift = driftSummaryOf(d, st, detail.DeployedRevision)
	return detail, nil
}

// lockBody is the holder of an environment's operation lock as the answers show it.
func lockBody(h stackops.Holder) *StackLockBody {
	return &StackLockBody{
		StackID: h.StackID, StackName: h.StackName, JobID: h.JobID, Kind: h.Kind,
		StartedAt: rfc3339(h.Started), StartedBy: h.By,
	}
}

// stackNameTakenProblem is the 409 for a name one of the environment's stacks holds.
func stackNameTakenProblem(name, envName string) error {
	return &huma.ErrorModel{
		Type: problemStackNameTaken, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: stackNameTakenMessage(name, envName),
		Errors: []*huma.ErrorDetail{{Location: "name", Message: name}},
	}
}

// stackNameFree refuses a name the environment already uses. A name the environment uses for something TierPlan does
// not manage is refused (D-12): TierPlan never deploys into, takes over or changes such a stack. A name one of TierPlan's
// own stacks holds is 409 stack_name_taken, and its objects are never a clash.
func (d StacksDeps) stackNameFree(ctx context.Context, envID, envName string, target stacks.Target, name string) error {
	own, err := d.repo().Stacks(ctx, envID)
	if err != nil {
		return stackInternal(d, "list", envID, err)
	}
	if stackNameTaken(own, target, name) {
		return stackNameTakenProblem(name, envName)
	}
	if !slices.ContainsFunc(own, func(s stackops.Stack) bool { return s.Name == name }) {
		foreign, err := d.ops().ForeignStack(ctx, envID, target, name, "")
		if err != nil {
			return snapshotProblem(d.Log, envID, err)
		}
		if foreign {
			return &huma.ErrorModel{
				Type: problemNameClash, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
				Detail: stackops.NameClashMessage(name),
				Errors: []*huma.ErrorDetail{{Location: "name", Message: name}},
			}
		}
	}
	return nil
}

func createStack(ctx context.Context, d StacksDeps, in *StackCreateInput) (*StackDetailOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	name := in.Body.Name
	// The target comes from the stored facts of the environment, not the engine; the name is held to the
	// rule of that target before anything else is read (D-53, T-08-37).
	target, err := d.stackTargetOf(ctx, env.ID)
	if err != nil {
		return nil, err
	}
	if !stacks.NameValid(target, name) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackNameInvalid, stackNameInvalidMessage(target))
	}
	compose := in.Body.Compose
	if compose == "" {
		switch in.Body.Start {
		case "", stackStartExample:
			compose = stacks.StarterCompose
		case stackStartEmpty, stackStartUpload:
		default:
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemStackStartInvalid, "Start from the starter example, an empty file or an uploaded file.")
		}
	}
	if len(compose) > stacks.MaxFileBytes {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemFileTooLarge, "A file is at most 1 MiB.")
	}

	if err := d.stackNameFree(ctx, env.ID, env.Name, target, name); err != nil {
		return nil, err
	}

	st, _, err := d.repo().CreateStack(ctx, env.ID, name, target, p.Name, stacks.WorkingCopy{
		Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}},
	})
	if errors.Is(err, stackops.ErrNameTaken) {
		return nil, stackNameTakenProblem(name, env.Name)
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
