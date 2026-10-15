// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/rbac/compose"
	"github.com/ianglennon/bayplan/internal/rbac/swarmspec"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// SpecJSON is a service spec carried as the JSON object the caller sent, byte for byte, so a
// number above 2^53 or a field the typed spec would drop is judged by the strict decoder inside
// swarmops, not lost by this layer.
type SpecJSON json.RawMessage

// UnmarshalJSON keeps the bytes.
func (s *SpecJSON) UnmarshalJSON(b []byte) error {
	*s = append((*s)[:0], bytes.TrimSpace(b)...)
	return nil
}

// MarshalJSON writes the bytes, or null for none.
func (s SpecJSON) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}
	return s, nil
}

// Schema implements huma.SchemaProvider: any JSON object.
func (SpecJSON) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type: "object", AdditionalProperties: true,
		Description: "A full swarm service spec as Docker states it (Name, Labels, TaskTemplate, Mode, ...), decoded strictly: an unknown field is 422 spec_invalid",
	}
}

// ServiceEditRequest is the body of the review and apply routes.
type ServiceEditRequest struct {
	Version uint64   `json:"version" minimum:"1" doc:"The version of the service the caller read (what GET .../spec answered). When it is not the current one nothing is changed and the answer is 409 service_changed"`
	Spec    SpecJSON `json:"spec" doc:"The full service spec to review or apply. Without secrets.reveal it is the masked spec the caller read, changed: a value left as <hidden> keeps its current value, a value typed replaces it, a removed entry is removed"`
}

// ServiceEditInput is the path and body of the review and apply routes.
type ServiceEditInput struct {
	ServiceDetailInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceEditRequest
}

// ServiceReviewImageBody is how the image of the reviewed spec resolved.
type ServiceReviewImageBody struct {
	Ref        string `json:"ref" doc:"The image the spec will carry: tag@sha256:digest when the tag was resolved, else as written"`
	Tag        string `json:"tag" doc:"The tag; empty for a reference by digest alone"`
	Digest     string `json:"digest" doc:"The digest the image is pinned to; empty when it is not pinned"`
	Host       string `json:"host" doc:"The registry the image is on"`
	Credential string `json:"credential" doc:"The name of the stored credential the lookup used; empty for none. Never its secret"`
	Changed    bool   `json:"changed" doc:"True when the spec's image differs from the service's current one"`
}

// ServiceReviewImageErrorBody is a digest lookup that failed.
type ServiceReviewImageErrorBody struct {
	Ref    string `json:"ref" doc:"The image that could not be looked up"`
	Host   string `json:"host" doc:"The registry asked"`
	Reason string `json:"reason" doc:"Why, as a pull failure code (unauthorized, not_found, rate_limited, unreachable, ...)"`
}

// ServiceFindingBody is one host-escalation setting of the spec.
type ServiceFindingBody struct {
	Path  string `json:"path" doc:"Where it is in the spec, for example TaskTemplate.ContainerSpec.Mounts[1].Source"`
	Value string `json:"value" doc:"The offending value"`
	Rule  string `json:"rule" enum:"bind_path,volume_device,npipe,cap_add,selinux,seccomp,apparmor,host_network,plugin_runtime" doc:"The rule that fired"`
	Why   string `json:"why" doc:"Why the setting needs env Admin"`
}

// ServiceMissingBody lists what the spec names that does not exist.
type ServiceMissingBody struct {
	Networks []string `json:"networks" doc:"Networks the spec names, by the name or ID it used, that the swarm does not have"`
	Secrets  []string `json:"secrets" doc:"Secrets the spec names that the swarm does not have"`
	Configs  []string `json:"configs" doc:"Configs the spec names that the swarm does not have"`
}

// ServiceHostPolicyBody is what the manager's agent will forward.
type ServiceHostPolicyBody struct {
	ManagerAllowsPrivileged bool `json:"managerAllowsPrivileged" doc:"False when the manager's agent refuses services with host access because its owner did not set BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true; true when it allows them or there is no agent in the way"`
	ReadOnly                bool `json:"readOnly" doc:"True when the manager's host is read-only (BAYPLAN_AGENT_READ_ONLY): nothing can be applied"`
}

// ServiceReviewBody is the review of a spec.
type ServiceReviewBody struct {
	Spec       map[string]any               `json:"spec" doc:"The merged, normalised spec the apply would send, with the image pinned when it could be, as the caller may see it: the values of the environment, the command line, the health check command and the log options are <hidden> unless the caller holds secrets.reveal"`
	Version    uint64                       `json:"version" doc:"The service's current version"`
	Changed    bool                         `json:"changed" doc:"False when the spec equals the current one: an apply would answer 422 nothing_to_apply"`
	Image      ServiceReviewImageBody       `json:"image" doc:"How the image resolved"`
	ImageError *ServiceReviewImageErrorBody `json:"imageError,omitempty" doc:"Present when the digest of a changed tag could not be looked up; the spec is then unpinned, and an apply would answer 422 image_unresolved"`
	Findings   []ServiceFindingBody         `json:"findings" doc:"The host-escalation settings of the spec, returned whether or not the caller may have them"`
	Allowed    bool                         `json:"allowed" doc:"False when the caller is below env Admin and findings is not empty"`
	Refusal    string                       `json:"refusal" doc:"When not allowed, one finding per line as {path}: {value}"`
	Missing    ServiceMissingBody           `json:"missing" doc:"Objects the spec names that do not exist; an apply would answer 422 objects_missing"`
	HostPolicy ServiceHostPolicyBody        `json:"hostPolicy" doc:"What the manager's agent will forward"`
	Warnings   []string                     `json:"warnings" doc:"Notes about the review; empty when there are none"`
}

// ServiceReviewOutput wraps ServiceReviewBody for huma.
type ServiceReviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceReviewBody
}

// ServiceApplyBody is what an apply sent.
type ServiceApplyBody struct {
	Image    string   `json:"image" doc:"The image the service now names: tag@sha256:digest when the tag was resolved"`
	Digest   string   `json:"digest" doc:"The digest the image is pinned to; empty when it names none"`
	Pinned   bool     `json:"pinned" doc:"True when Bayplan resolved the tag and wrote the digest into the spec"`
	Warnings []string `json:"warnings" doc:"The manager's warnings about the update, scrubbed of masked values; empty when there are none"`
}

// ServiceApplyOutput wraps ServiceApplyBody for huma.
type ServiceApplyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceApplyBody
}

const (
	problemSpecInvalid         = "spec_invalid"
	problemSpecRejected        = "spec_rejected"
	problemNameImmutable       = "name_immutable"
	problemModeImmutable       = "mode_immutable"
	problemStackLabelReserved  = "stack_label_reserved"
	problemPlaceholderNoMatch  = "placeholder_unmatched"
	problemPlaceholderReserved = "placeholder_reserved"
	problemObjectsMissing      = "objects_missing"
	problemNothingToApply      = "nothing_to_apply"
	problemPrivilegedService   = rbac.ReasonPrivilegedService
	problemUpdateFailed        = "update_failed"
)

// registerServiceEdit mounts the review and apply routes.
func registerServiceEdit(r *httpx.Router, d ServicesDeps) {
	const problems = "Problems, all before anything is sent: 409 service_changed when the version is not the current one; 409 stack_owned for a service a stack owns; 404 service_not_found; 409 not_a_swarm for a standalone host; " +
		"422 spec_invalid (the decoder's message) for a spec that does not decode strictly into a swarm service spec, whoever sends it; 422 placeholder_unmatched for a <hidden> with no current value behind it; 422 placeholder_reserved when a caller who may reveal secrets sends <hidden>; " +
		"422 name_immutable and 422 mode_immutable for a changed name or kind of mode; 422 stack_label_reserved for a com.docker.stack. label the service did not have; " +
		"403 privileged_service, with each finding in errors, when a caller below env Admin sends host access (a bind outside /srv, /mnt and /media or of a runtime socket, a local volume that mounts a host device, a named pipe, an added capability, SELinux disabled or spc_t, seccomp unconfined or custom, AppArmor disabled, the host network, a plugin runtime). "
	httpx.Register(r, huma.Operation{
		OperationID: "reviewServiceUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/review",
		Summary: "Review a full service spec",
		Description: "Judges a full service spec and answers what an apply would do, without changing Docker. " +
			"The spec is read fresh against the service: the values a caller without secrets.reveal could not see (<hidden>) are merged back in, a changed tag is resolved to its digest through the manager with the stored registry credential (a failed lookup is imageError, and the spec stays unpinned), and the host-escalation policy runs on the decoded spec an apply would forward. " +
			"The answer is the merged, normalised spec (masked again for a caller without secrets.reveal), the image, the findings, whether the caller may have them (allowed) and the networks the spec names that do not exist (missing). " +
			"A refusal by the policy is not an error here: allowed is false and refusal lists the findings. " + problems +
			"Needs services.control (Operator) on the environment. A review is not audited.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceEditInput) (*ServiceReviewOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := reviewServiceUpdate(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceReviewOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "applyServiceUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/apply",
		Summary: "Apply a full service spec",
		Description: "Sends a full service spec once and never retried; the rollout continues in the swarm, so the answer is 202. " +
			"In one write on a manager it inspects the service fresh, refuses a version that is not the one sent, merges the masked values back, runs the host-escalation policy on the merged spec that will be forwarded, resolves the digest when the image changed (a failed lookup is 422 image_unresolved and nothing is sent) and sends one update with the version and the registry credential. " +
			"Two applies racing on one service: Docker's version check lets one through and the other gets 409 service_changed; neither is retried, and sending the same version twice makes the second 409. " +
			"Also 422 nothing_to_apply for a spec equal to the current one, 422 objects_missing for a network the spec names that does not exist, and 422 spec_rejected, with Docker's words, for a spec Docker refuses (a missing secret or config, for one). " + problems +
			"No answer, error or audit row carries a value of the environment, the command line, the health check or the log options for a caller without secrets.reveal. " +
			"Needs services.control (Operator) on the environment. Every attempt on a service that exists is audited as service.updated, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceEditInput) (*ServiceApplyOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := applyServiceUpdate(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ServiceApplyOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// swarmopsDeps is what the orchestrator needs from the routes' dependencies.
func swarmopsDeps(d ServicesDeps) swarmops.Deps {
	sd := swarmops.Deps{Checker: d.Checker, StackOwner: stackOwner}
	if d.Resolver != nil {
		sd.Resolver = d.Resolver
	}
	return sd
}

// reviewServiceUpdate judges a spec inside one cluster read: it only reads, so a read that has to be
// run again on another manager is safe.
func reviewServiceUpdate(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceEditInput) (ServiceReviewBody, error) {
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
		res, e = swarmops.Review(readCtx, swarmopsDeps(d), swarmops.ReviewInput{
			EnvID: envID, Client: c, ServiceID: in.ServiceID, Version: in.Body.Version, Spec: json.RawMessage(in.Body.Spec), Caller: caller, HostPolicy: hp,
		})
		return e
	})
	if err != nil {
		_, problem := serviceEditProblem(d, envID, err)
		return ServiceReviewBody{}, problem
	}
	raw, err := json.Marshal(res.Spec)
	if err != nil {
		d.Log.Error("encoding a reviewed spec failed", "environment", envID, "error", err)
		return ServiceReviewBody{}, huma.Error500InternalServerError("internal error")
	}
	spec, err := decodeInspect(raw)
	if err != nil {
		d.Log.Error("reading a reviewed spec failed", "environment", envID, "error", err)
		return ServiceReviewBody{}, huma.Error500InternalServerError("internal error")
	}
	body := ServiceReviewBody{
		Spec: spec, Version: res.Version, Changed: res.Changed,
		Image: ServiceReviewImageBody{
			Ref: res.Image.Ref, Tag: res.Image.Tag, Digest: res.Image.Digest, Host: res.Image.Host,
			Credential: res.Image.Credential, Changed: res.Image.Changed,
		},
		Findings: findingBodies(res.Findings), Allowed: res.Allowed, Refusal: res.Refusal,
		Missing:    ServiceMissingBody{Networks: nonNil(res.Missing.Networks), Secrets: nonNil(res.Missing.Secrets), Configs: nonNil(res.Missing.Configs)},
		HostPolicy: ServiceHostPolicyBody{ManagerAllowsPrivileged: res.HostPolicy.ManagerAllowsPrivileged, ReadOnly: res.HostPolicy.ReadOnly},
		Warnings:   nonNil(res.Warnings),
	}
	if res.ImageError != nil {
		body.ImageError = &ServiceReviewImageErrorBody{Ref: res.ImageError.Ref, Host: res.ImageError.Host, Reason: res.ImageError.Reason}
	}
	return body, nil
}

func findingBodies(fs []compose.Finding) []ServiceFindingBody {
	out := make([]ServiceFindingBody, len(fs))
	for i, f := range fs {
		out[i] = ServiceFindingBody{Path: f.Key, Value: f.Value, Rule: f.Rule, Why: f.Why}
	}
	return out
}

// applyServiceUpdate sends a spec once. The write is sent and seen through, so a browser that goes
// away while it runs does not turn an update that happened into a failure.
func applyServiceUpdate(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceEditInput) (ServiceApplyBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceApplyBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var res swarmops.ApplyResult
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.Apply(writeCtx, swarmopsDeps(d), swarmops.ApplyInput{
			EnvID: envID, Client: c, ServiceID: in.ServiceID, Version: in.Body.Version, Spec: json.RawMessage(in.Body.Spec), Caller: caller,
		})
		return e
	})
	details := func(code string) map[string]string {
		return map[string]string{
			"name": res.Name, "version": strconv.FormatUint(in.Body.Version, 10), "image": res.Image.Ref, "digest": res.Image.Digest,
			"changed": strconv.FormatBool(res.Changed), "findings": strconv.Itoa(len(res.Findings)), "code": code,
		}
	}
	if err != nil {
		code, problem := serviceEditProblem(d, envID, err)
		if code != codeServiceNotFound {
			auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceUpdated, res.ServiceID, in.ServiceID, details(code), code)
		}
		return ServiceApplyBody{}, problem
	}
	refreshClusters(ctx, d, envID)
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceUpdated, res.ServiceID, in.ServiceID, details(actionDone), actionDone)
	return ServiceApplyBody{Image: res.Image.Ref, Digest: res.Image.Digest, Pinned: res.Image.Pinned, Warnings: nonNil(res.Warnings)}, nil
}

// serviceEditProblem turns the error of a review or an apply into the code its audit row records and
// the problem the SPA words. None of the sentences holds a value of the spec.
func serviceEditProblem(d ServicesDeps, envID string, err error) (string, error) {
	problem := func(status int, typ, detail string, errs ...*huma.ErrorDetail) (string, error) {
		return typ, &huma.ErrorModel{Type: typ, Title: http.StatusText(status), Status: status, Detail: detail, Errors: errs}
	}
	var (
		invalid  *swarmops.SpecInvalidError
		owned    *swarmops.StackOwnedError
		refused  *swarmops.RefusedError
		missing  *swarmops.MissingError
		unres    *swarmops.ImageUnresolvedError
		rejected *swarmops.RejectedError
		ph       *redact.PlaceholderError
	)
	switch {
	case errors.As(err, &invalid):
		return problem(http.StatusUnprocessableEntity, problemSpecInvalid, "The service spec can't be read: "+invalid.Message+".",
			&huma.ErrorDetail{Location: "spec", Message: invalid.Message})
	case errors.As(err, &owned):
		return problemStackOwned, stackOwnedProblem(owned.Stack)
	case errors.As(err, &ph) && errors.Is(err, redact.ErrPlaceholderReserved):
		return problem(http.StatusUnprocessableEntity, problemPlaceholderReserved,
			redact.Placeholder+" is what Bayplan shows for a value it withholds, so it can't be a value. Type the real value or remove it.",
			&huma.ErrorDetail{Location: ph.Path, Message: "reserved"})
	case errors.As(err, &ph):
		return problem(http.StatusUnprocessableEntity, problemPlaceholderNoMatch,
			"A hidden value in the spec has nothing behind it: "+ph.Path+" isn't in the service. Type its value or remove it.",
			&huma.ErrorDetail{Location: ph.Path, Message: "unmatched"})
	case errors.Is(err, swarmops.ErrNameImmutable):
		return problem(http.StatusUnprocessableEntity, problemNameImmutable, "A service's name can't be changed. Create a new service instead.")
	case errors.Is(err, swarmops.ErrModeImmutable):
		return problem(http.StatusUnprocessableEntity, problemModeImmutable, "A service's mode (replicated, global or a job) can't be changed. Create a new service instead.")
	case errors.Is(err, swarmops.ErrStackLabel):
		return problem(http.StatusUnprocessableEntity, problemStackLabelReserved, "Labels starting with com.docker.stack. belong to stacks. Remove them from the spec.")
	case errors.Is(err, swarmops.ErrNothingToApply):
		return problem(http.StatusUnprocessableEntity, problemNothingToApply, "There's nothing to apply.")
	case errors.As(err, &missing):
		errs := make([]*huma.ErrorDetail, 0, len(missing.Missing.Networks))
		for _, n := range missing.Missing.Networks {
			errs = append(errs, &huma.ErrorDetail{Location: "networks", Message: n})
		}
		return problem(http.StatusUnprocessableEntity, problemObjectsMissing,
			"The spec names networks that don't exist: "+strings.Join(missing.Missing.Networks, ", ")+". Create them first.", errs...)
	case errors.As(err, &refused):
		if refused.Reason == rbac.ReasonServiceSpecUnreadable {
			return problem(http.StatusUnprocessableEntity, problemSpecInvalid, clipDetail(refused.Message))
		}
		errs := make([]*huma.ErrorDetail, len(refused.Findings))
		lines := make([]string, len(refused.Findings))
		for i, f := range refused.Findings {
			errs[i] = &huma.ErrorDetail{Location: f.Key, Message: f.Value}
			lines[i] = swarmspec.FormatSpec(f)
		}
		return problem(http.StatusForbidden, problemPrivilegedService,
			"This service uses settings that need Admin: "+strings.Join(lines, "; ")+". Ask an administrator to apply it.", errs...)
	case errors.As(err, &unres):
		return problemImageUnresolved, &huma.ErrorModel{
			Type: problemImageUnresolved, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity,
			Detail: "Could not look up " + unres.Ref + " to pin it to a digest. Nothing was changed.",
			Errors: []*huma.ErrorDetail{
				{Location: "image.ref", Message: unres.Ref},
				{Location: "image.host", Message: unres.Host},
				{Location: "reason", Message: unres.Reason},
			},
		}
	case errors.As(err, &rejected):
		return problem(http.StatusUnprocessableEntity, problemSpecRejected, "Docker rejected these settings: "+clipDetail(rejected.Message))
	case errors.Is(err, swarmops.ErrRegistryUnresolved):
		d.Log.Error("could not resolve the registry credential of a service update", "environment", envID, "error", err)
		err = errRegistryUnresolved
	}
	return serviceActionProblem(d, envID, problemUpdateFailed, err)
}
