// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

const (
	problemImageUnresolved      = "image_unresolved"
	problemNoPreviousSpec       = "no_previous_spec"
	problemRollbackObjectMissed = "rollback_object_missing"
	problemRollbackRefused      = "rollback_refused"
	problemForceUpdateFailed    = "force_update_failed"
	problemRollbackFailed       = "rollback_failed"
	problemRemoveFailed         = "remove_failed"
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

// ServiceRemoveInput is the path and query of DELETE .../services/{serviceId}.
type ServiceRemoveInput struct {
	ServiceDetailInput
	Confirm string `query:"confirm" maxLength:"128" doc:"The service's name, typed. Needed only to remove Bayplan's own agent service; ignored for any other service"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// registerServiceActions mounts the force update, rollback and remove routes.
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

	httpx.Register(r, huma.Operation{
		OperationID: "removeService", Method: http.MethodDelete, Path: "/api/env/{envId}/services/{serviceId}",
		Summary: "Remove a service",
		Description: "Removes one service, once and never retried: the swarm stops its tasks and deletes the service. Its images, volumes, networks, secrets and configs stay. " +
			"A service that belongs to a stack (com.docker.stack.namespace) is refused: 409 stack_owned, with the stack in errors as stack, before anything is sent. " +
			"Bayplan's own agent service is removed only when confirm is its name, else 422 confirmation_required and nothing is sent. " +
			"404 service_not_found for a service that is not there; 409 not_a_swarm for a standalone host. " +
			"Needs services.manage (Admin) on the environment. Every attempt on a service that exists is audited as service.removed, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermServicesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceRemoveInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, removeService(ctx, d, p, env.ID, in)
		})
}

// serviceWriteOutcome is what one force update or rollback learned inside its write, beyond
// the error: the service's ID and name for the audit row, and the image it started from.
type serviceWriteOutcome struct {
	id, name, image string
}

// imageUnresolvedError is a tag that could not be looked up, with what the problem needs to
// say about it. It is the reason a force update sends nothing.
type imageUnresolvedError struct {
	ref, host, reason string
	err               error
}

func (e *imageUnresolvedError) Error() string {
	return "could not look up " + e.ref + ": " + e.err.Error()
}
func (e *imageUnresolvedError) Unwrap() error { return e.err }

// forceUpdateService replaces every task of a service again. In one write call on a manager
// it reads the service, compares the version the caller saw, resolves the credential for the
// image, resolves the tag to its digest itself and sends the update with QueryRegistry off.
// A tag that cannot be resolved is the answer, never an update with the unpinned tag.
func forceUpdateService(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceForceUpdateInput) (ServiceForceUpdateBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceForceUpdateBody{}, err
	}
	// The write is sent once and seen through, so a browser that goes away while it runs does
	// not turn an update that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var out ServiceForceUpdateBody
	var wo serviceWriteOutcome
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		detail, _, err := c.ServiceInspect(writeCtx, in.ServiceID)
		if err != nil {
			return err
		}
		wo.id, wo.name, wo.image = detail.ID, detail.Name, detail.Image
		version := in.Body.Version
		if version == 0 {
			version = detail.Version
		}
		if version != detail.Version {
			return dockerx.ErrServiceChanged
		}
		out = ServiceForceUpdateBody{Image: detail.Image, PreviousImage: detail.Image}
		opts := dockerx.ForceUpdateOptions{Version: version}
		if opts.EncodedAuth, err = registryAuthFor(writeCtx, d, envID, detail.Image); err != nil {
			return err
		}
		if detail.Image == "" {
			out.Reason = reasonNoImage
		} else {
			res, err := c.ResolveImage(writeCtx, detail.Image, opts.EncodedAuth)
			switch {
			case errors.Is(err, dockerx.ErrDigestOnly):
				out.Reason = reasonDigestOnly
				if ref, perr := registry.ParseRef(detail.Image); perr == nil {
					out.Digest = ref.Digest
				}
			case err != nil:
				return unresolved(detail.Image, err)
			default:
				opts.Image, opts.Platforms = res.Ref, res.Platforms
				out.Image, out.Digest, out.Pinned = res.Ref, res.Digest, true
			}
		}
		return c.ServiceForceUpdate(writeCtx, detail.ID, opts)
	})
	retry := strconv.FormatBool(in.Body.Retry)
	if err != nil {
		code, problem := serviceActionProblem(d, envID, problemForceUpdateFailed, err)
		if code != codeServiceNotFound {
			details := map[string]string{"name": wo.name, "image": wo.image, "retry": retry, "pinned": "false", "code": code}
			auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceForceUpdated, wo.id, in.ServiceID, details, code)
		}
		return ServiceForceUpdateBody{}, problem
	}
	refreshClusters(ctx, d, envID)
	details := map[string]string{"name": wo.name, "image": out.Image, "digest": out.Digest, "retry": retry, "pinned": strconv.FormatBool(out.Pinned), "code": actionDone}
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceForceUpdated, wo.id, in.ServiceID, details, actionDone)
	return out, nil
}

// unresolved wraps the error of a tag lookup with the reference and the registry it asked.
func unresolved(image string, err error) error {
	e := &imageUnresolvedError{ref: image, reason: dockerx.PullFailure(err), err: err}
	if ref, perr := registry.ParseRef(image); perr == nil {
		e.host = ref.Domain
	}
	return e
}

// rollbackService sends the current spec back to the manager with rollback=previous. In one
// write call it reads the service, compares the version the caller saw (so the rollback is
// of the spec that was shown), judges the previous spec by the host-access policy for the caller
// before anything is read or sent, and resolves the credential for the previous spec's image.
func rollbackService(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceRollbackInput) (ServiceRollbackBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceRollbackBody{}, err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var out ServiceRollbackBody
	var wo serviceWriteOutcome
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		detail, _, err := c.ServiceInspect(writeCtx, in.ServiceID)
		if err != nil {
			return err
		}
		wo.id, wo.name, wo.image = detail.ID, detail.Name, detail.Image
		if detail.Version != in.Body.Version {
			return dockerx.ErrServiceChanged
		}
		if detail.PreviousSpec == nil {
			return dockerx.ErrNoPreviousSpec
		}
		if d.Checker == nil {
			return errors.New("the host-access policy is not configured")
		}
		nets, err := c.Networks(writeCtx)
		if err != nil {
			return err
		}
		dec, err := d.Checker.CheckServiceSpec(writeCtx, p, envID, detail.PreviousSpec, swarmops.NetworkResolverOf(nets))
		if err != nil {
			return err
		}
		if !dec.Allowed {
			return &swarmops.RefusedError{Reason: dec.Reason, Message: dec.Message, Findings: dec.Findings}
		}
		var prev swarm.ServiceSpec
		if err := json.Unmarshal(detail.PreviousSpec, &prev); err != nil {
			return fmt.Errorf("read the previous spec: %w", err)
		}
		if cs := prev.TaskTemplate.ContainerSpec; cs != nil {
			out.Image = cs.Image
		}
		auth, err := registryAuthFor(writeCtx, d, envID, out.Image)
		if err != nil {
			return err
		}
		return c.ServiceRollback(writeCtx, detail.ID, in.Body.Version, auth)
	})
	version := strconv.FormatUint(in.Body.Version, 10)
	if err != nil {
		code, problem := serviceWriteProblem(d, envID, problemRollbackFailed, err)
		if code != codeServiceNotFound {
			details := map[string]string{"name": wo.name, "version": version, "code": code}
			var refused *swarmops.RefusedError
			if errors.As(err, &refused) {
				details["findings"] = strconv.Itoa(len(refused.Findings))
			}
			auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceRolledBack, wo.id, in.ServiceID, details, code)
		}
		return ServiceRollbackBody{}, problem
	}
	refreshClusters(ctx, d, envID)
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceRolledBack, wo.id, in.ServiceID,
		map[string]string{"name": wo.name, "version": version, "code": actionDone}, actionDone)
	return out, nil
}

// serviceRemoveRefusedError is a remove the server turned down after reading the service:
// nothing was sent. code is what the audit row records and problem what the caller is told.
type serviceRemoveRefusedError struct {
	code    string
	problem error
}

func (e *serviceRemoveRefusedError) Error() string { return "remove refused: " + e.code }

// removeService removes one service. In one write call on a manager it reads the service,
// refuses one a stack owns (D-05) and Bayplan's own agent service unless its name was typed,
// and otherwise removes it, once and never retried. Every attempt on a service that exists
// is audited.
func removeService(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceRemoveInput) error {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return err
	}
	// The write is sent once and seen through, so a browser that goes away while it runs does
	// not turn a removal that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var wo serviceWriteOutcome
	var stack string
	var agent, confirmed bool
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		detail, _, err := c.ServiceInspect(writeCtx, in.ServiceID)
		if err != nil {
			return err
		}
		wo.id, wo.name, wo.image = detail.ID, detail.Name, detail.Image
		agent = detail.Agent
		confirmed = detail.Agent && in.Confirm == detail.Name
		if s, owned := stackOwner(detail.Labels); owned {
			stack = s
			return &serviceRemoveRefusedError{code: problemStackOwned, problem: stackOwnedProblem(s)}
		}
		if agent && !confirmed {
			return &serviceRemoveRefusedError{code: codeConfirmationRequired, problem: httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired,
				"This is Bayplan's agent service. Removing it disconnects every node it runs on. Type its name, "+detail.Name+", to confirm.")}
		}
		return c.ServiceRemove(writeCtx, detail.ID)
	})
	details := func(code string) map[string]string {
		return map[string]string{
			"name": wo.name, "stack": stack, "agent": strconv.FormatBool(agent),
			"confirmed": strconv.FormatBool(confirmed), "code": code,
		}
	}
	if err != nil {
		var refused *serviceRemoveRefusedError
		if errors.As(err, &refused) {
			auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceRemoved, wo.id, in.ServiceID, details(refused.code), refused.code)
			return refused.problem
		}
		code, problem := serviceActionProblem(d, envID, problemRemoveFailed, err)
		if code != codeServiceNotFound {
			auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceRemoved, wo.id, in.ServiceID, details(code), code)
		}
		return problem
	}
	refreshClusters(ctx, d, envID)
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceRemoved, wo.id, in.ServiceID, details(actionDone), actionDone)
	return nil
}

// refreshClusters reads the cluster again so the list shows the update that was just sent. A
// failed read leaves the last one.
func refreshClusters(ctx context.Context, d ServicesDeps, envID string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	_ = d.Clusters.Refresh(rctx, envID)
}

// serviceActionProblem turns the error of a force update or a rollback into the code its audit
// row records and the problem the SPA words. failedType is the type of a refusal that is
// none of the known ones: the manager's own words go in the detail.
func serviceActionProblem(d ServicesDeps, envID, failedType string, err error) (string, error) {
	var unresolvedErr *imageUnresolvedError
	var refused *dockerx.RollbackRefusedError
	switch {
	case errors.As(err, &unresolvedErr):
		return problemImageUnresolved, &huma.ErrorModel{
			Type: problemImageUnresolved, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "Could not look up " + unresolvedErr.ref + " to pin it to a digest. Nothing was changed.",
			Errors: []*huma.ErrorDetail{
				{Location: "image.ref", Message: unresolvedErr.ref},
				{Location: "image.host", Message: unresolvedErr.host},
				{Location: "reason", Message: unresolvedErr.reason},
			},
		}
	case errors.Is(err, errRegistryUnresolved):
		return codeRegistryUnresolved, httpx.Problem(http.StatusServiceUnavailable, problemRegistryUnresolved,
			"The registry credential for this service's image could not be read, so nothing was sent.")
	case errors.Is(err, dockerx.ErrNoPreviousSpec):
		return problemNoPreviousSpec, httpx.Problem(http.StatusConflict, problemNoPreviousSpec, "There's no earlier version to roll back to.")
	case errors.As(err, &refused):
		if strings.Contains(strings.ToLower(refused.Message), "not found") {
			m := &huma.ErrorModel{
				Type: problemRollbackObjectMissed, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
				Detail: clipDetail(refused.Message),
			}
			// The kind and the name of what is gone, when the manager's words say, so the page can
			// name it without parsing them.
			if kind, name, ok := missingObject(refused.Message); ok {
				m.Errors = []*huma.ErrorDetail{{Location: "object.kind", Message: kind}, {Location: "object.name", Message: name}}
			}
			return problemRollbackObjectMissed, m
		}
		return problemRollbackRefused, httpx.Problem(http.StatusUnprocessableEntity, problemRollbackRefused, clipDetail(refused.Message))
	}
	switch code := swarmWriteCode(err); code {
	case codeServiceNotFound:
		return code, httpx.Problem(http.StatusNotFound, problemServiceNotFound, "This service does not exist. It may have been removed.")
	case codeServiceChanged, codeHostReadOnly, codeAgentHostPolicy:
		return code, httpx.Problem(http.StatusConflict, code, swarmWriteDetail[code])
	case codeNoManagerConnected:
		return code, httpx.Problem(http.StatusServiceUnavailable, problemNoManager, swarmWriteDetail[code])
	case codeAgentPolicyDeny:
		return code, snapshotProblem(d.Log, envID, err)
	}
	if envs.ReasonFor(err) == envs.ReasonNoQuorum {
		return codeNoQuorum, httpx.Problem(http.StatusServiceUnavailable, problemNoQuorum, "The swarm has lost its quorum.")
	}
	var down envs.ErrNotOnline
	if errors.As(envs.ClassifyDockerError(err), &down) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return codeActionFailed, snapshotProblem(d.Log, envID, err)
	}
	d.Log.Warn("a service update was refused", "environment", envID, "type", failedType, "error", err)
	return failedType, httpx.Problem(http.StatusBadGateway, failedType, clipDetail(err.Error()))
}

// missingObjectPatterns are the two shapes of a manager's refusal that names an object that is
// gone: "secret not found: db_password" and "network front not found".
var missingObjectPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(secret|config|network) not found: ([^\s,;]+)`),
	regexp.MustCompile(`(?i)\b(network) ([^\s,;]+) not found`),
}

// missingObject reads the kind and the name of the object a manager says is gone.
func missingObject(message string) (kind, name string, ok bool) {
	for _, re := range missingObjectPatterns {
		if m := re.FindStringSubmatch(message); m != nil {
			return strings.ToLower(m[1]), m[2], true
		}
	}
	return "", "", false
}

// auditServiceAction writes the row of one force update or rollback attempt: a success when
// code is done, else a failure whose reason is the code. It is written on a context that
// outlives the request: the update has already happened or been refused.
func auditServiceAction(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, userAgent, action, id, asked string, details map[string]string, code string) {
	result, reason := audit.ResultSuccess, ""
	if code != actionDone {
		result, reason = audit.ResultFailure, code
	}
	if id == "" {
		id = asked
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: serviceTargetType, TargetID: id,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		// The update has been made or refused and the caller must still be told which; the
		// lost row is the server's fault and is logged.
		d.Log.Error("could not audit a service action", "action", action, "service", id, "error", err)
	}
}
