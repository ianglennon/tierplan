// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/rebuild"
)

// The read-only page of a stack TierPlan did not make (plan 08-09, STK-12, D-09, D-10). The stack is a name on
// an environment, found by its labels each time it is asked for: nothing about it is stored, it has no ID, and
// none of these routes writes to an engine.

// problemUnmanagedNotFound is the type of the 404 for a name no labelled service or container carries, or one
// a stack of TierPlan's holds now.
const problemUnmanagedNotFound = "unmanaged_stack_not_found"

// problemRebuildFailed is the type of the 422 for a stack whose running services cannot be written as a compose
// file that passes the check every stack passes.
const problemRebuildFailed = "rebuild_failed"

// UnmanagedStackPathInput is the path of an unmanaged stack route: the environment and the stack's name, as the
// router encoded it.
type UnmanagedStackPathInput struct {
	EnvID string `path:"envId" doc:"The environment ID"`
	Name  string `path:"name" maxLength:"255" doc:"The swarm namespace or the Compose project that names the stack"`
}

// UnmanagedFoundByBody is the label a stack was found by.
type UnmanagedFoundByBody struct {
	Label string `json:"label" doc:"com.docker.stack.namespace on a swarm, com.docker.compose.project on a standalone host"`
	Value string `json:"value" doc:"The stack's name"`
}

// UnmanagedStackBody is a stack found on an environment that TierPlan did not make, read now.
type UnmanagedStackBody struct {
	Name            string               `json:"name" doc:"The swarm namespace or the Compose project"`
	Target          string               `json:"target" enum:"swarm,standalone" doc:"Where the stack runs: a swarm or a standalone host"`
	State           string               `json:"state" enum:"not_running,degraded,running" doc:"How many of its services run: running, degraded (some do) or not_running (none do)"`
	ServicesRunning int                  `json:"servicesRunning" minimum:"0" doc:"Services that run now"`
	ServicesTotal   int                  `json:"servicesTotal" minimum:"0" doc:"Services the stack has on the engine"`
	Services        []StackServiceBody   `json:"services" doc:"The services found by the label, by name. Their environment values, commands and log options are never part of the answer"`
	FoundBy         UnmanagedFoundByBody `json:"foundBy" doc:"The label the stack was found by"`
	Orphan          bool                 `json:"orphan" doc:"True when an object of the stack carries a tierplan.stack.id: TierPlan made it once, for a record this instance does not hold (another instance or a lost record). The ID is never sent or linked"`
	Portainer       bool                 `json:"portainer" doc:"True when the stack carries a Portainer marker: a label key starting io.portainer., or a Compose working directory under /data/compose/"`
	Owner           string               `json:"owner" enum:",tierplan,agent" doc:"tierplan for the stack the TierPlan server itself runs in, agent for a stack that holds a TierPlan agent, empty for any other"`
	Adoptable       bool                 `json:"adoptable" doc:"False for TierPlan's own stack, an agent's and a stack whose name the stack-name rule of its target refuses"`
	Reason          string               `json:"reason" doc:"Why the stack cannot be adopted; empty when it can"`
	Fingerprint     string               `json:"fingerprint" doc:"A digest of what runs: each service's ID and version on a swarm, each container's ID, config hash and image ID on a standalone host. The adopt request sends it back, and when what runs has changed since, the adoption is refused 409 adopt_stale and stores nothing. Empty when the stack cannot be adopted"`
}

// UnmanagedStackOutput wraps UnmanagedStackBody for huma.
type UnmanagedStackOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UnmanagedStackBody
}

// GeneratedNoteBody is something a person should know about the rebuilt file.
type GeneratedNoteBody struct {
	Service string `json:"service" doc:"The compose service the note is about; empty when it concerns the whole file"`
	Code    string `json:"code" doc:"A stable name for the kind of note: service_name_prefix, network_not_overlay, image_unpinned or cpus_inexact"`
	Message string `json:"message" doc:"The note as a sentence"`
}

// GeneratedComposeBody is the compose file TierPlan would start from if the stack were adopted (plan 08-11).
type GeneratedComposeBody struct {
	YAML            string              `json:"yaml" doc:"The compose file rebuilt from the running services. For a caller without secrets.reveal every environment value, command, health check command and log option reads <hidden>"`
	Notes           []GeneratedNoteBody `json:"notes" doc:"What the rebuild could not say plainly, by service then code"`
	ExternalSecrets int                 `json:"externalSecrets" minimum:"0" doc:"Secrets the file references as external. TierPlan reads their names, never their values"`
	ExternalConfigs int                 `json:"externalConfigs" minimum:"0" doc:"Configs the file references as external. TierPlan reads their names, never their contents"`
}

// GeneratedComposeOutput wraps GeneratedComposeBody for huma.
type GeneratedComposeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         GeneratedComposeBody
}

// maxUnmanagedName is the longest name the routes take, as the path parameter's maxLength says.
const maxUnmanagedName = 255

// registerUnmanagedStacks registers the routes of the read-only page of an unmanaged stack: the stack, its
// resources, and its log stream and download, which share the limits of the log routes of a managed stack.
func registerUnmanagedStacks(r *httpx.Router, d StacksDeps, logs *stackLogMount) {
	httpx.Register(r, huma.Operation{
		OperationID: "getUnmanagedStack", Method: http.MethodGet, Path: "/api/env/{envId}/unmanaged-stacks/{name}",
		Summary: "A stack TierPlan did not make",
		Description: "The services of a swarm namespace or a Compose project that TierPlan does not manage, found by that label and read now, with what its labels say about it: " +
			"an orphan TierPlan ID (it carries a tierplan.stack.id, which is never sent), a Portainer marker, whether it is TierPlan's own stack or its agent's. " +
			"Only names, states, images and published ports are part of the answer: no environment value, command or log option. Nothing is stored and no engine is written to. " +
			"A name no labelled service or container carries is 404 unmanaged_stack_not_found; so is a name one of TierPlan's stacks holds now, with that stack's ID in errors as managedStackId. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UnmanagedStackPathInput) (*UnmanagedStackOutput, error) {
			return getUnmanagedStack(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getGeneratedCompose", Method: http.MethodGet, Path: "/api/env/{envId}/unmanaged-stacks/{name}/compose",
		Summary: "The compose file TierPlan would use if it adopted the stack",
		Description: "The compose file rebuilt from the running services of a swarm stack, or from the containers of a docker compose project, that TierPlan does not manage, read now. It is a preview: nothing is stored and no engine is written to. " +
			"Environment values are written as literals, with every $ doubled so the file loads back as it reads. Existing swarm secrets and configs are external, by name: their values are never read. " +
			"Networks and volumes that carry the stack's label are the stack's own, written under their existing names; the others are external. " +
			"The services pass through the same mask as the service detail and the container inspect: without secrets.reveal on the environment every environment value, command, argument, health check command and log option reads <hidden>. " +
			"A stack whose services cannot be written as a compose file that passes the check is 422 rebuild_failed with the reason. " +
			"A name no labelled service or container carries, or one a stack of TierPlan's holds, is 404 unmanaged_stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UnmanagedStackPathInput) (*GeneratedComposeOutput, error) {
			return getGeneratedCompose(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "listUnmanagedStackResources", Method: http.MethodGet, Path: "/api/env/{envId}/unmanaged-stacks/{name}/resources",
		Summary: "What an unmanaged stack uses, and what adopting it would own",
		Description: "The tasks (a swarm) or containers (a standalone host) of the services found by the label, and the networks, volumes, secrets and configs they use. " +
			"The ownership of each is a preview of adopting: a network or a volume that carries the stack's namespace or project label reads stack, any other external; a secret or a config is always external. " +
			"Each card is read on its own and carries its own error (a code, never the engine's text). No secret value, config content or label is part of the answer, and no engine is written to. " +
			"A name no labelled service or container carries, or one a stack of TierPlan's holds, is 404 unmanaged_stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UnmanagedStackPathInput) (*StackResourcesOutput, error) {
			return listUnmanagedStackResources(ctx, d, in)
		})

	// The log routes are mounted through Router.Guarded, as a managed stack's are, because the contract cannot
	// describe a stream (D-47). The stack is the name in the address; its log carries no known secret to mask.
	d = logs.d
	r.Guarded(http.MethodGet, "/api/env/{envId}/unmanaged-stacks/{name}/logs/stream", logs.need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveStackLogs(w, req, d, logs.streams, logs.mem, unmanagedLogSubject)
	}))
	r.Guarded(http.MethodGet, "/api/env/{envId}/unmanaged-stacks/{name}/logs/download", logs.need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		downloadStackLogs(w, req, d, logs.user, logs.server, unmanagedLogSubject)
	}))
}

func listUnmanagedStackResources(ctx context.Context, d StacksDeps, in *UnmanagedStackPathInput) (*StackResourcesOutput, error) {
	f, err := d.findUnmanaged(ctx, in.EnvID, in.Name)
	if err != nil {
		return nil, err
	}
	// Nothing declares what the stack uses, so the zero Declared goes in and the services' own references stand in.
	res := d.ops().Resources(ctx, in.EnvID, f.stack, stackops.Declared{})
	return &StackResourcesOutput{CacheControl: "no-store", Body: stackResourcesBody(ctx, d, in.EnvID, f.stack, res)}, nil
}

// unmanagedLogSubject is the stack the address names by its name. A stack TierPlan did not make has no secret TierPlan
// knows of, so its log is masked with nothing: what a person may read there they may read on the Services and
// Containers pages already (P4 permissions).
func unmanagedLogSubject(ctx context.Context, d StacksDeps, envID string, req *http.Request) (stackops.Stack, stacks.Masker, error) {
	name, err := url.PathUnescape(chi.URLParam(req, "name"))
	if err != nil || name == "" || len(name) > maxUnmanagedName {
		return stackops.Stack{}, stacks.Masker{}, unmanagedNotFound("")
	}
	f, err := d.findUnmanaged(ctx, envID, name)
	if err != nil {
		return stackops.Stack{}, stacks.Masker{}, err
	}
	return f.stack, stacks.Masker{}, nil
}

// unmanagedNotFound is the 404 for a name that is not an unmanaged stack of the environment (now). managedID is
// the ID of the stack of TierPlan's that holds the name, empty when none does.
func unmanagedNotFound(managedID string) error {
	m := &huma.ErrorModel{
		Type: problemUnmanagedNotFound, Title: http.StatusText(http.StatusNotFound), Status: http.StatusNotFound,
		Detail: "No stack of this name is running here that TierPlan does not manage. It was removed outside TierPlan, or it is managed now.",
	}
	if managedID != "" {
		m.Errors = []*huma.ErrorDetail{{Location: "managedStackId", Message: managedID}}
	}
	return m
}

// selfMarks says which objects of an environment of that kind are TierPlan's own: the container the server runs in,
// and, for the server's own Docker host only, its swarm namespace.
func (d StacksDeps) selfMarks(envKind string) stackops.SelfMarks {
	self := stackops.SelfMarks{ContainerID: d.SelfContainerID}
	if envKind == envs.KindLocal {
		self.Namespace = d.SelfNamespace
	}
	return self
}

// foundUnmanaged is a stack found on an environment that TierPlan did not make: the stack to read it as and
// what the labels say about it.
type foundUnmanaged struct {
	stack stackops.Stack
	found stackops.Discovered
}

// findUnmanaged looks for the unmanaged stack of that name on the environment, live. A name one of TierPlan's
// stacks holds is that stack's (404 with its ID), and a name nothing carries is 404. The errors are answers.
func (d StacksDeps) findUnmanaged(ctx context.Context, envID, name string) (foundUnmanaged, error) {
	own, err := d.repo().Stacks(ctx, envID)
	if err != nil {
		return foundUnmanaged{}, stackInternal(d, "list", envID, err)
	}
	if i := slices.IndexFunc(own, func(s stackops.Stack) bool { return s.Name == name }); i >= 0 {
		return foundUnmanaged{}, unmanagedNotFound(own[i].ID)
	}
	if d.Clusters == nil || d.Store == nil {
		d.Log.Error("the stack routes have no cluster reader")
		return foundUnmanaged{}, huma.Error500InternalServerError("internal error")
	}
	row, err := d.Store.RQ.GetEnvironment(ctx, envID)
	if err != nil {
		return foundUnmanaged{}, stackInternal(d, "unmanaged: read the environment", envID, err)
	}
	swarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return foundUnmanaged{}, snapshotProblem(d.Log, envID, err)
	}
	target := stacks.TargetStandalone
	if swarm {
		target = stacks.TargetSwarm
	}
	found, ok, err := d.ops().DiscoverNamed(ctx, envID, target, name, own, d.selfMarks(row.Kind))
	if err != nil {
		return foundUnmanaged{}, snapshotProblem(d.Log, envID, err)
	}
	if !ok {
		return foundUnmanaged{}, unmanagedNotFound("")
	}
	return foundUnmanaged{stack: stackops.DiscoveredStack(envID, name, target), found: found}, nil
}

func getUnmanagedStack(ctx context.Context, d StacksDeps, in *UnmanagedStackPathInput) (*UnmanagedStackOutput, error) {
	f, err := d.findUnmanaged(ctx, in.EnvID, in.Name)
	if err != nil {
		return nil, err
	}
	u := f.found
	body := UnmanagedStackBody{
		Name: u.Name, Target: string(u.Target), State: string(u.State),
		ServicesRunning: u.Running, ServicesTotal: u.Total, Services: stackServiceBodies(u.Rows),
		FoundBy: UnmanagedFoundByBody{Label: stacks.LabelComposeProject, Value: u.Name},
		Orphan:  u.OrphanID != "", Portainer: u.Portainer, Owner: u.Owner, Adoptable: u.Adoptable(), Reason: notAdoptableReason(u),
	}
	if u.Target == stacks.TargetSwarm {
		body.FoundBy.Label = stacks.LabelSwarmNamespace
	}
	if u.Adoptable() {
		var err error
		if body.Fingerprint, err = d.ops().AdoptFingerprint(ctx, in.EnvID, u.Target, u.Name); err != nil {
			return nil, snapshotProblem(d.Log, in.EnvID, err)
		}
	}
	return &UnmanagedStackOutput{CacheControl: "no-store", Body: body}, nil
}

func getGeneratedCompose(ctx context.Context, d StacksDeps, in *UnmanagedStackPathInput) (*GeneratedComposeOutput, error) {
	f, err := d.findUnmanaged(ctx, in.EnvID, in.Name)
	if err != nil {
		return nil, err
	}
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	res, err := d.ops().RebuildLive(ctx, in.EnvID, f.stack, stackops.RebuildOptions{Reveal: redact.CanReveal(ctx, d.Checker, p, in.EnvID)})
	var failed *stackops.RebuildError
	switch {
	case errors.As(err, &failed):
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, failed.Reason)
	case errors.Is(err, stackops.ErrRebuildTarget):
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRebuildFailed, "TierPlan can't rebuild this kind of stack.")
	case err != nil:
		return nil, snapshotProblem(d.Log, in.EnvID, err)
	}
	body := GeneratedComposeBody{YAML: string(res.YAML), Notes: make([]GeneratedNoteBody, 0, len(res.Notes))}
	for _, n := range res.Notes {
		body.Notes = append(body.Notes, GeneratedNoteBody{Service: n.Service, Code: n.Code, Message: n.Message})
	}
	for _, o := range res.Objects {
		switch {
		case o.Kind == rebuild.KindSecret && o.Treatment == rebuild.TreatmentExternal:
			body.ExternalSecrets++
		case o.Kind == rebuild.KindConfig && o.Treatment == rebuild.TreatmentExternal:
			body.ExternalConfigs++
		}
	}
	return &GeneratedComposeOutput{CacheControl: "no-store", Body: body}, nil
}
