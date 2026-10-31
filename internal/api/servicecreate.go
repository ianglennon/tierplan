// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/redact"
	"github.com/ianglennon/coaming/internal/swarmops"
)

// ServiceCreateReviewRequest is the body of the create review route.
type ServiceCreateReviewRequest struct {
	Spec SpecJSON `json:"spec" doc:"The full service spec to create, as Docker states it (Name, Labels, TaskTemplate, Mode, ...), decoded strictly: an unknown field is 422 spec_invalid. A <hidden> placeholder stands for nothing in a new service and is 422 placeholder_unmatched"`
}

// ServiceCreateRequest is the body of the create route: the spec and the objects it names that do
// not exist yet.
type ServiceCreateRequest struct {
	ServiceCreateReviewRequest
	Objects []ServiceStagedObject `json:"objects,omitempty" doc:"The networks, secrets and configs the spec names by name that do not exist yet, with the values to make them from, exactly as for an apply. At most 16 (else 422 too_many_objects). Each is created before the service, and every one is removed again if the service cannot be created"`
}

// ServiceCreateReviewInput is the path and body of the create review route.
type ServiceCreateReviewInput struct {
	EnvironmentPathInput
	Body ServiceCreateReviewRequest
}

// ServiceCreateInput is the path and body of the create route.
type ServiceCreateInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceCreateRequest
}

// ServiceCreateBody is what a create made.
type ServiceCreateBody struct {
	ID       string                 `json:"id" doc:"The ID the swarm gave the new service"`
	Name     string                 `json:"name" doc:"The service's name"`
	Image    string                 `json:"image" doc:"The image the service was created with: tag@sha256:digest when the tag was resolved"`
	Digest   string                 `json:"digest" doc:"The digest the image is pinned to; empty when it names none"`
	Pinned   bool                   `json:"pinned" doc:"True when Coaming resolved the tag and wrote the digest into the spec"`
	Warnings []string               `json:"warnings" doc:"The manager's warnings about the creation, scrubbed of masked values; empty when there are none"`
	Steps    []ServiceApplyStepBody `json:"steps" doc:"The steps of the create in order: the image, each object created, the service"`
	Cleanup  ServiceCleanupBody     `json:"cleanup" doc:"Empty after a successful create. A failed create answers a problem whose errors carry the same lists"`
}

// ServiceCreateOutput wraps ServiceCreateBody for huma.
type ServiceCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceCreateBody
}

// registerServiceCreate mounts the create review and create routes.
func registerServiceCreate(r *httpx.Router, d ServicesDeps) {
	const problems = "Problems, all before anything is created: 409 service_name_taken for a name a service has; 422 image_required for no image; 422 name_invalid for a name that is not 1 to 63 letters, numbers, dot, dash or underscore; " +
		"422 spec_invalid (the decoder's message) for a spec that does not decode strictly into a swarm service spec, or for a secret reference whose SecretName is not the name of the secret its SecretID points at; 422 placeholder_unmatched for a <hidden> in the spec; 422 stack_label_reserved for a com.docker.stack. label; " +
		"422 image_unresolved when the tag cannot be resolved to a digest (a new service is never created unpinned); 409 not_a_swarm for a standalone host; " +
		"403 privileged_service, with each finding in errors, when the caller is below env Admin and the spec asks for host access. "
	httpx.Register(r, huma.Operation{
		OperationID: "reviewServiceCreate", Method: http.MethodPost, Path: "/api/env/{envId}/services/review",
		Summary: "Review a new service",
		Description: "Judges the full spec of a service that does not exist yet and answers what a create would do, without changing Docker. " +
			"The image is required and is resolved to its digest through the manager with the stored registry credential (a failed lookup is imageError, and the spec stays unpinned), and the host-escalation policy runs on the decoded spec a create would forward. " +
			"The answer is the normalised spec (masked for a caller without secrets.reveal), the image, the findings, whether the caller may have them (allowed) and the networks, secrets and configs the spec names that do not exist (missing). " +
			"version is 0 and changed is true. " + problems +
			"Needs services.manage (Env Admin) on the environment. A review is not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceCreateReviewInput) (*ServiceReviewOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := reviewServiceCreate(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceReviewOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "createService", Method: http.MethodPost, Path: "/api/env/{envId}/services",
		Summary: "Create a service",
		Description: "Creates one standalone service from a full spec, once and never retried; the tasks start in the swarm, so the answer is 201. " +
			"The spec is decoded strictly, the host-escalation policy runs on the whole of it, the image is required and resolved to its digest (a failed lookup is 422 image_unresolved and nothing is created), and one create is sent with the registry credential and without the engine's own registry lookup. " +
			"The networks, secrets and configs in objects are created first, networks then secrets then configs, their IDs are written into the spec's references, and then the service is created; each is audited as network.created, secret.created or config.created with source service_apply, never with a value. " +
			"If the service cannot be created, every object this request made is removed again in reverse order (audited with source service_apply_cleanup); the problem's detail says so and its errors carry the reason on its own (location reason) and the objects as removed and notRemoved, each as kind:name. " +
			"Also 422 objects_missing for a network, secret or config the spec names that does not exist and was not sent in objects, 422 spec_rejected, with Docker's words, for a spec Docker refuses, 422 too_many_objects for more than 16 objects, 422 object_duplicate for the same kind and name twice, the name, value, labels, templating and subnet problems of the secrets, configs and networks routes for a bad object, 409 name_taken when an object's name exists already, and 403 object_permission, before anything is created, for a caller without networks.manage, secrets.manage or configs.manage for a kind it would create. " + problems +
			"No answer, error or audit row carries a value of the environment, the command line, the health check or the log options for a caller without secrets.reveal, or the value of a staged secret or config. " +
			"Needs services.manage (Env Admin) on the environment. Every attempt is audited as service.created, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxApplyBody,
	}, rbac.Requirement{Perm: rbac.PermServicesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceCreateInput) (*ServiceCreateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := createService(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceCreateOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// reviewServiceCreate judges a new service's spec inside one cluster read: it only reads, so a read
// that has to be run again on another manager is safe.
func reviewServiceCreate(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceCreateReviewInput) (ServiceReviewBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceReviewBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	hp := swarmops.HostPolicy{ManagerAllowsPrivileged: true}
	if mp, err := d.Clusters.ManagerPolicy(ctx, envID); err == nil && mp.Known {
		hp = swarmops.HostPolicy{ManagerAllowsPrivileged: mp.PrivilegedServices, ReadOnly: mp.ReadOnly}
	}
	readCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var res swarmops.ReviewResult
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.ReviewCreate(readCtx, swarmopsDeps(d), swarmops.CreateInput{
			EnvID: envID, Client: c, Spec: json.RawMessage(in.Body.Spec), Caller: caller, HostPolicy: hp,
		})
		return e
	})
	if err != nil {
		_, problem := serviceWriteProblem(ctx, d, envID, problemCreateFailed, err)
		return ServiceReviewBody{}, problem
	}
	return reviewBody(d, envID, res)
}

// createService makes one service. The write is sent and seen through, so a browser that goes away
// while it runs does not turn a service that was made into a failure.
func createService(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceCreateInput) (ServiceCreateBody, error) {
	objects, err := stagedObjects(in.Body.Objects)
	if err != nil {
		return ServiceCreateBody{}, err
	}
	defer func() {
		for _, o := range objects {
			clear(o.Data)
		}
	}()
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceCreateBody{}, err
	}
	defer lockStagedSubnets(d, envID, objects)()
	if err := stagedOverlapGate(ctx, d, envID, objects); err != nil {
		return ServiceCreateBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	sd := applyDeps(writeCtx, d, p, envID, in.UserAgent)
	var res swarmops.CreateResult
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.Create(writeCtx, sd, swarmops.CreateInput{
			EnvID: envID, Client: c, Spec: json.RawMessage(in.Body.Spec), Caller: caller, Objects: objects,
		})
		return e
	})
	made := 0
	for _, s := range res.Steps {
		if s.Step == swarmops.StepObject && s.State == swarmops.StepDone {
			made++
		}
	}
	details := func(code string) map[string]string {
		return map[string]string{
			"name": res.Name, "image": res.Image.Ref, "digest": res.Image.Digest, "objects": strconv.Itoa(made), "code": code,
		}
	}
	// A failed create has no ID: its row is about the name that was asked for.
	asked := res.Name
	if asked == "" {
		asked = "(unnamed)"
	}
	if err != nil {
		code, problem := serviceWriteProblem(ctx, d, envID, problemCreateFailed, err)
		auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceCreated, res.ServiceID, asked, details(code), code)
		return ServiceCreateBody{}, withCleanup(problem, "create", res.Name, res.Cleanup)
	}
	refreshClusters(ctx, d, envID)
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceCreated, res.ServiceID, asked, details(actionDone), actionDone)
	return ServiceCreateBody{
		ID: res.ServiceID, Name: res.Name, Image: res.Image.Ref, Digest: res.Image.Digest, Pinned: res.Image.Pinned, Warnings: nonNil(res.Warnings),
		Steps: stepBodies(res.Steps), Cleanup: ServiceCleanupBody{Removed: objectBodies(res.Cleanup.Removed), NotRemoved: objectBodies(res.Cleanup.NotRemoved)},
	}, nil
}
