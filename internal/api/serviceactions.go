// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

const (
	problemImageUnresolved      = "image_unresolved"
	problemNoPreviousSpec       = "no_previous_spec"
	problemRollbackObjectMissed = "rollback_object_missing"
	problemRollbackRefused      = "rollback_refused"
	problemForceUpdateFailed    = "force_update_failed"
	problemRollbackFailed       = "rollback_failed"
	problemRegistryUnresolved   = codeRegistryUnresolved

	// reasonDigestOnly and reasonNoImage say why a force update did not pin an image.
	reasonDigestOnly = "digest_only"
	reasonNoImage    = "no_image"
)

// ServiceSpecBody is the spec of one service and the one before its last update, for the
// editor and the rollback diff.
type ServiceSpecBody struct {
	Version      uint64         `json:"version" doc:"The service's version: what an update or a rollback must send back to be accepted"`
	Name         string         `json:"name" doc:"The service name"`
	Mode         string         `json:"mode" enum:"replicated,global,replicated-job,global-job" doc:"How the swarm runs it"`
	Stack        string         `json:"stack" doc:"The stack the service belongs to (com.docker.stack.namespace); empty for a service made on its own"`
	Agent        bool           `json:"agent" doc:"True for a Bayplan agent service"`
	Masked       bool           `json:"masked" doc:"True when a value was replaced by the placeholder <hidden> because the caller may not reveal secrets on this environment"`
	Spec         map[string]any `json:"spec" doc:"The service's spec as the manager states it (Name, Labels, TaskTemplate, Mode, ...), with the values of the environment variables, the command line, the health check command and the log driver's options replaced by <hidden> unless the caller holds secrets.reveal"`
	PreviousSpec map[string]any `json:"previousSpec,omitempty" doc:"The spec before the last update, masked the same way; absent when the service was never updated, so there is nothing to roll back to"`
}

// ServiceSpecOutput wraps ServiceSpecBody for huma.
type ServiceSpecOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceSpecBody
}

// ServiceForceUpdateRequest is the body of POST .../services/{serviceId}/force-update.
type ServiceForceUpdateRequest struct {
	Version uint64 `json:"version,omitempty" doc:"The version of the service the caller saw. When it is not the current one nothing is changed and the answer is 409 service_changed. Left out (0), the version read at the start of the update is used, so a repeated force update always goes through"`
	Retry   bool   `json:"retry,omitempty" doc:"True when this repeats a stalled or failed update. It changes nothing but the audit row"`
}

// ServiceForceUpdateInput is the path and body of the force update route.
type ServiceForceUpdateInput struct {
	ServiceDetailInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceForceUpdateRequest
}

// ServiceForceUpdateBody is what a force update left the service with.
type ServiceForceUpdateBody struct {
	Image         string `json:"image" doc:"The image the service now names: tag@sha256:digest when the tag was resolved, else what it named"`
	Digest        string `json:"digest" doc:"The digest the image is pinned to; empty when it names none"`
	Pinned        bool   `json:"pinned" doc:"True when Bayplan resolved the tag and wrote the digest into the spec"`
	PreviousImage string `json:"previousImage" doc:"The image the service named before"`
	Reason        string `json:"reason,omitempty" enum:"digest_only,no_image" doc:"Why nothing was pinned. digest_only: the image names a digest and no tag, so it already is the exact image. no_image: the spec has no container image"`
}

// ServiceForceUpdateOutput wraps ServiceForceUpdateBody for huma.
type ServiceForceUpdateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceForceUpdateBody
}

// ServiceRollbackRequest is the body of POST .../services/{serviceId}/rollback.
type ServiceRollbackRequest struct {
	Version uint64 `json:"version" minimum:"1" doc:"The version of the service the caller saw (what GET .../spec answered). When it is not the current one nothing is changed and the answer is 409 service_changed, so a rollback only ever goes back to the spec that was shown"`
}

// ServiceRollbackInput is the path and body of the rollback route.
type ServiceRollbackInput struct {
	ServiceDetailInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceRollbackRequest
}

// ServiceRollbackBody is the answer of a rollback that was started.
type ServiceRollbackBody struct {
	Image string `json:"image" doc:"The image of the spec the service is going back to; empty when that spec names none"`
}

// ServiceRollbackOutput wraps ServiceRollbackBody for huma.
type ServiceRollbackOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceRollbackBody
}

// registerServiceActions mounts the force update and rollback routes.
func registerServiceActions(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "forceUpdateService", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/force-update",
		Summary: "Force a service update",
		Description: "Replaces every task of a service again with the settings it has now, once and never retried. " +
			"Bayplan strips any digest from the spec's image, resolves the tag to its current digest through the manager with the stored registry credential, writes tag@sha256 into the spec, raises the spec's ForceUpdate counter and sends the update with the version just read, so a tag that moved is picked up. " +
			"A lookup that fails is 422 image_unresolved (errors name image.ref, image.host and reason) and nothing is sent: a force update never falls back to the unpinned tag. " +
			"An image that names a digest and no tag is not re-pinned: the answer says pinned false with reason digest_only. " +
			"A version that is not the current one is 409 service_changed; 409 not_a_swarm for a standalone host. Works on a service that belongs to a stack. " +
			"Repeating it replaces every task again: each call is its own rollout. " +
			"Needs services.control (Operator) on the environment. Every attempt on a service that exists is audited as service.force_updated.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceForceUpdateInput) (*ServiceForceUpdateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := forceUpdateService(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceForceUpdateOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "rollbackService", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/rollback",
		Summary: "Roll a service back to its previous spec",
		Description: "Sends the service's current spec with rollback=previous and the version, once and never retried; the manager swaps the current and the previous spec, so a second rollback goes back to the spec just replaced. " +
			"The registry credential for the previous spec's image is sent with it. The answer is 202: the rollout continues in the swarm. " +
			"A service that has no previous spec is 409 no_previous_spec. A manager that refuses because the previous spec names a secret, a config or a network that no longer exists is 409 rollback_object_missing, with its words in detail. " +
			"A version that is not the current one is 409 service_changed; 409 not_a_swarm for a standalone host. Works on a service that belongs to a stack. " +
			"Needs services.control (Operator) on the environment. Every attempt on a service that exists is audited as service.rolled_back.",
		Tags:          []string{"environments"},
		DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceRollbackInput) (*ServiceRollbackOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := rollbackService(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceRollbackOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// forceUpdateService is not written yet.
func forceUpdateService(_ context.Context, _ ServicesDeps, _ *rbac.Principal, _ string, _ *ServiceForceUpdateInput) (ServiceForceUpdateBody, error) {
	return ServiceForceUpdateBody{}, httpx.Problem(http.StatusNotImplemented, "not_implemented", "not implemented")
}

// rollbackService is not written yet.
func rollbackService(_ context.Context, _ ServicesDeps, _ *rbac.Principal, _ string, _ *ServiceRollbackInput) (ServiceRollbackBody, error) {
	return ServiceRollbackBody{}, httpx.Problem(http.StatusNotImplemented, "not_implemented", "not implemented")
}
