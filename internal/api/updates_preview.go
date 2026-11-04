// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// What the compact confirm shows before an update runs (plan 12-16, D-05, IMG-06): the digests that would change and the things
// worth knowing first. It reads the check that is stored, never the registry.

// previewContainerNeedsRead is the 403 for someone who may read services but not containers.
const previewContainerNeedsRead = "Describing the update of a container needs the permission to read containers here."

// UpdatePreviewInput is the path and query of the preview route.
type UpdatePreviewInput struct {
	EnvironmentPathInput
	Kind string `query:"kind" enum:"service,container,stack" doc:"service for a swarm service (a service of a stack TierPlan manages is described through its stack), container for a plain container (needs containers.read as well; one of a stack TierPlan manages is described through its stack), stack for a stack TierPlan manages"`
	Name string `query:"name" minLength:"1" maxLength:"255" doc:"The service's or container's name, or the stack's name for kind stack"`
}

// UpdatePreviewItem is one workload the update would change.
type UpdatePreviewItem struct {
	Kind             string   `json:"kind" enum:"service,container" doc:"What it is"`
	Name             string   `json:"name" doc:"The service's or container's name; for a service of a stack on a standalone host, its name in the compose file"`
	Stack            string   `json:"stack" doc:"The stack it belongs to; empty for a service made on its own"`
	Image            string   `json:"image" doc:"The image it follows, without a digest (repository:tag)"`
	FromDigest       string   `json:"fromDigest" doc:"The digest it runs"`
	ToDigest         string   `json:"toDigest" doc:"The digest the stored check found the tag at"`
	Platforms        []string `json:"platforms" doc:"The platforms it runs on, for a multi-platform image; empty when none is known"`
	ChangedPlatforms []string `json:"changedPlatforms" doc:"The platforms it runs on whose image changed; empty when none is named"`
}

// UpdateWarningBody is one thing to know before the update runs.
type UpdateWarningBody struct {
	Workload string `json:"workload" doc:"The service the warning is about"`
	Code     string `json:"code" enum:"downtime,container_downtime,no_healthcheck,host_ports,volumes,previous_failure" doc:"downtime (the only replica stops before its replacement starts), container_downtime (a container is recreated), no_healthcheck (neither the service nor its image defines a health check), host_ports (a port is published in host mode), volumes (the service mounts a volume), previous_failure (an update to this version failed before)"`
}

// UpdateLeftOutBody is a service of a stack that an update of the stack leaves out.
type UpdateLeftOutBody struct {
	Name  string `json:"name" doc:"The service's name, as in items"`
	State string `json:"state" enum:"skipped,ignored" doc:"Why it is left out: skipped (this version was skipped) or ignored (its image is ignored)"`
}

// UpdatePreviewBody is what the confirm shows.
type UpdatePreviewBody struct {
	Items    []UpdatePreviewItem `json:"items" doc:"The workloads the update would change, one for a service; empty when offered is false"`
	Warnings []UpdateWarningBody `json:"warnings" doc:"Pre-flight warnings, worked out on the server; empty when offered is false"`
	LeftOut  []UpdateLeftOutBody `json:"leftOut" doc:"For a stack: the services the update leaves out because they are skipped or ignored; empty otherwise"`
	Lock     *UpdateHolderBody   `json:"lock,omitempty" doc:"Left out unless set: who holds the environment's operation lock. While it is held the update is refused with 409 operation_locked"`
	Offered  bool                `json:"offered" doc:"True when an update would be offered for this service"`
	Refusal  string              `json:"refusal" enum:",agent,job_mode,managed_by_stack,nothing_to_update,not_checked,check_failed,pinned_in_file,not_from_registry,offline,adopt_first,compose_unmanaged,task_container" doc:"Why no update is offered; empty when one is. compose_unmanaged: the container belongs to a Compose project TierPlan does not manage, so it is adopted first; task_container: a task container of a swarm service is updated through its service"`
}

// UpdatePreviewOutput wraps UpdatePreviewBody for huma.
type UpdatePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdatePreviewBody
}

func registerImageUpdatePreview(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "previewImageUpdate", Method: http.MethodGet, Path: "/api/env/{envId}/updates/preview",
		Summary: "What updating a service would change",
		Description: "The digests that would change, the platforms they cover, and the warnings worth reading first: a replicated service with one replica that stops before its replacement starts (downtime), no health check in the service or its image (no_healthcheck), a port published in host mode (host_ports) and a mounted volume (volumes). " +
			"Read from the check that is stored and the service as it is now; the registry is not asked, so what it shows is what the badge shows. " +
			"A service of a stack TierPlan manages, and kind stack, are described as the update of the stack: the services it would change and, in leftOut, those it leaves out because they are skipped or ignored. " +
			"offered is false, with a refusal, when the service cannot or need not be updated (the TierPlan agent, a job, nothing newer, a stack that must be adopted first (adopt_first), no check yet, a failed check, a pinned or local image) or when the environment is offline. " +
			"For kind container the warnings are container_downtime (always: it is stopped and recreated), no_healthcheck and volumes, and a container of a Compose project TierPlan does not manage, or a task container, is refused with compose_unmanaged or task_container. Needs containers.read (Viewer) as well. " +
			"lock names who holds the environment's operation lock, if anyone does. 404 not_found when the service is not there. Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdatePreviewInput) (*UpdatePreviewOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := previewImageUpdate(ctx, d, httpx.PrincipalFrom(ctx), env.ID, in)
			if err != nil {
				return nil, err
			}
			return &UpdatePreviewOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// previewImageUpdate describes the update of a service.
func previewImageUpdate(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdatePreviewInput) (UpdatePreviewBody, error) {
	if d.Applier == nil {
		return UpdatePreviewBody{}, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
	}
	if in.Kind == "container" {
		// A container's preview reads the container, which the route's services.read does not cover.
		if p == nil || d.Checker == nil {
			return UpdatePreviewBody{}, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Updates are not available right now.")
		}
		dec := d.Checker.Check(ctx, p, rbac.Require(rbac.PermContainersRead), envID)
		if dec.Err != nil {
			return UpdatePreviewBody{}, snapshotProblem(d.log(), envID, dec.Err)
		}
		if !dec.Allowed {
			return UpdatePreviewBody{}, httpx.Problem(http.StatusForbidden, "", previewContainerNeedsRead)
		}
	}
	if in.Kind == "stack" && d.Stacks.Store != nil {
		// A stack that is only discovered has no revision to update: it is adopted first (D-09).
		_, err := managedStackByName(ctx, d.Stacks.repo(), envID, in.Name)
		switch {
		case errors.Is(err, stackops.ErrStackNotFound):
			return UpdatePreviewBody{Items: []UpdatePreviewItem{}, Warnings: []UpdateWarningBody{}, LeftOut: []UpdateLeftOutBody{}, Refusal: problemAdoptFirst}, nil
		case err != nil:
			return UpdatePreviewBody{}, snapshotProblem(d.log(), envID, err)
		}
	}
	pv, err := d.Applier.Preview(ctx, envID, in.Kind, in.Name)
	if err != nil {
		var down envs.ErrNotOnline
		switch {
		case errors.Is(err, updates.ErrWorkloadGone):
			return UpdatePreviewBody{}, workloadGone(in.Kind)
		case errors.Is(err, updates.ErrUnsupportedKind):
			return UpdatePreviewBody{}, httpx.Problem(http.StatusUnprocessableEntity, "", "That kind of workload can't be updated yet.")
		case errors.As(envs.ClassifyDockerError(err), &down):
			return UpdatePreviewBody{Items: []UpdatePreviewItem{}, Warnings: []UpdateWarningBody{}, LeftOut: []UpdateLeftOutBody{}, Refusal: "offline"}, nil
		}
		return UpdatePreviewBody{}, snapshotProblem(d.log(), envID, err)
	}
	b := UpdatePreviewBody{
		Items: make([]UpdatePreviewItem, 0, len(pv.Items)), Warnings: make([]UpdateWarningBody, 0, len(pv.Warnings)),
		LeftOut: make([]UpdateLeftOutBody, 0, len(pv.LeftOut)), Offered: pv.Offered, Refusal: pv.Refusal,
	}
	for _, l := range pv.LeftOut {
		b.LeftOut = append(b.LeftOut, UpdateLeftOutBody{Name: l.Name, State: string(l.State)})
	}
	for _, it := range pv.Items {
		b.Items = append(b.Items, UpdatePreviewItem{
			Kind: it.Kind, Name: it.Name, Stack: it.Stack, Image: it.Image, FromDigest: it.FromDigest, ToDigest: it.ToDigest,
			Platforms: nonNil(it.Platforms), ChangedPlatforms: nonNil(it.ChangedPlatforms),
		})
	}
	for _, w := range pv.Warnings {
		b.Warnings = append(b.Warnings, UpdateWarningBody{Workload: w.Workload, Code: w.Code})
	}
	if pv.Lock != nil {
		h := holderBodyOf(*pv.Lock)
		b.Lock = &h
	}
	return b, nil
}
