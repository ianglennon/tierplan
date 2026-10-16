// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// maxStagedObjects is the most objects one apply creates, and maxApplyBody the most bytes its request
// may have: the per-kind size limits of the secrets and configs routes apply to each object.
const (
	maxStagedObjects = 16
	maxApplyBody     = 8 << 20
)

// ServiceStagedObject is an object the editor made that does not exist yet.
type ServiceStagedObject struct {
	Kind       string            `json:"kind" enum:"network,secret,config" doc:"What to create. A network is made as an attachable overlay"`
	Name       string            `json:"name" doc:"The name the spec uses for it: letters, numbers, dot, dash and underscore, starting and ending with a letter or number, at most 64 characters (else 422 name_invalid)"`
	Data       string            `json:"data,omitempty" doc:"A secret's value or a config's content, as standard base64: 1 to 511,999 bytes for a secret and 1 to 1,023,999 for a config (else 422 value_required, value_invalid or value_too_large). It is sent to the swarm once and never kept, logged, audited or returned. Ignored for a network"`
	Templating string            `json:"templating,omitempty" doc:"A config only: none (the default) or golang, to have the swarm fill the content in as a Go template; anything else is 422 templating_invalid"`
	Labels     map[string]string `json:"labels,omitempty" maxProperties:"64" doc:"Labels of a secret or config, at most 64 (422 labels_invalid for a key over 256 or a value over 4096 characters)"`
	Subnet     string            `json:"subnet,omitempty" doc:"A network only: the subnet in CIDR form; left out, the swarm chooses (422 subnet_invalid)"`
}

// ServiceApplyRequest is the body of the apply route: the review's body and the objects the spec
// names that do not exist yet.
type ServiceApplyRequest struct {
	ServiceEditRequest
	Objects []ServiceStagedObject `json:"objects,omitempty" doc:"The networks, secrets and configs the spec names by name that do not exist yet, with the values to make them from. At most 16 (else 422 too_many_objects). Each is created, networks then secrets then configs, before the service is updated, and every one is removed again if anything after the first creation fails. One the spec does not name is ignored. The caller needs networks.manage, secrets.manage or configs.manage (Env Admin) for each kind created, else 403 object_permission before anything is created"`
}

// ServiceApplyInput is the path and body of the apply route.
type ServiceApplyInput struct {
	ServiceDetailInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ServiceApplyRequest
}

// ServiceEditInput is the path and body of the review route.
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

// ServiceApplyStepBody is one step of an apply.
type ServiceApplyStepBody struct {
	Step  string `json:"step" enum:"image,object,service" doc:"image (the tag resolved to a digest), object (one network, secret or config created) or service (the update)"`
	Kind  string `json:"kind" doc:"For an object step: network, secret or config; empty otherwise"`
	Name  string `json:"name" doc:"For an object step the object's name, for the service step the service's; empty for the image"`
	State string `json:"state" enum:"done,failed,skipped" doc:"skipped is a step with nothing to do or one that was not reached"`
	Code  string `json:"code" doc:"Why a step failed; empty otherwise"`
}

// ServiceObjectBody names an object an apply made.
type ServiceObjectBody struct {
	Kind string `json:"kind" enum:"network,secret,config" doc:"network, secret or config"`
	Name string `json:"name" doc:"The object's name"`
	ID   string `json:"id" doc:"The ID the swarm gave it"`
}

// ServiceCleanupBody is what removing the objects of a failed apply did.
type ServiceCleanupBody struct {
	Removed    []ServiceObjectBody `json:"removed" doc:"The objects made by the apply that were removed again, in the order they were removed. Empty after a successful apply"`
	NotRemoved []ServiceObjectBody `json:"notRemoved" doc:"The objects made by the apply that could not be removed and are still there; empty when none"`
}

// ServiceApplyBody is what an apply sent.
type ServiceApplyBody struct {
	Image    string                 `json:"image" doc:"The image the service now names: tag@sha256:digest when the tag was resolved"`
	Digest   string                 `json:"digest" doc:"The digest the image is pinned to; empty when it names none"`
	Pinned   bool                   `json:"pinned" doc:"True when Bayplan resolved the tag and wrote the digest into the spec"`
	Warnings []string               `json:"warnings" doc:"The manager's warnings about the update, scrubbed of masked values; empty when there are none"`
	Steps    []ServiceApplyStepBody `json:"steps" doc:"The steps of the apply in order: the image, each object created, the service"`
	Cleanup  ServiceCleanupBody     `json:"cleanup" doc:"Empty after a successful apply. A failed apply answers a problem whose errors carry the same lists"`
}

// ServiceApplyOutput wraps ServiceApplyBody for huma.
type ServiceApplyOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceApplyBody
}

const (
	problemSpecInvalid          = "spec_invalid"
	problemSpecRejected         = "spec_rejected"
	problemNameImmutable        = "name_immutable"
	problemModeImmutable        = "mode_immutable"
	problemStackLabelReserved   = "stack_label_reserved"
	problemPlaceholderNoMatch   = "placeholder_unmatched"
	problemPlaceholderReserved  = "placeholder_reserved"
	problemObjectsMissing       = "objects_missing"
	problemNothingToApply       = "nothing_to_apply"
	problemPrivilegedService    = rbac.ReasonPrivilegedService
	problemSecretRevealRequired = "secret_reveal_required"
	problemUpdateFailed         = "update_failed"
	problemTooManyObjects       = "too_many_objects"
	problemObjectPermission     = "object_permission"
	problemObjectDuplicate      = "object_duplicate"
	problemImageRequired        = "image_required"
	problemServiceNameTaken     = "service_name_taken"
)

// registerServiceEdit mounts the review and apply routes.
func registerServiceEdit(r *httpx.Router, d ServicesDeps) {
	const problems = "Problems, all before anything is sent: 409 service_changed when the version is not the current one; 409 stack_owned for a service a stack owns; 404 service_not_found; 409 not_a_swarm for a standalone host; " +
		"422 spec_invalid (the decoder's message) for a spec that does not decode strictly into a swarm service spec, whoever sends it; 422 placeholder_unmatched for a <hidden> with no current value behind it; 422 placeholder_reserved when a caller who may reveal secrets sends <hidden>; " +
		"422 name_immutable and 422 mode_immutable for a changed name or kind of mode; 422 stack_label_reserved for a com.docker.stack. label the service did not have; " +
		"403 privileged_service, with each finding in errors, when a caller below env Admin sends host access (a bind outside /srv, /mnt and /media or of a runtime socket, a local volume that mounts a host device, a named pipe, an added capability, SELinux disabled or spc_t, seccomp unconfined or custom, AppArmor disabled, the host network, a plugin runtime). " +
		"403 secret_reveal_required, with each added reference in errors, when a caller without secrets.reveal adds a secret the service does not use; "
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
			"Also 422 nothing_to_apply for a spec equal to the current one, 422 objects_missing for a network, secret or config the spec names that does not exist and was not sent in objects, and 422 spec_rejected, with Docker's words, for a spec Docker refuses. " +
			"The networks, secrets and configs in objects are created first, networks then secrets then configs, their IDs are written into the spec's references, and then the service is updated; each is audited as network.created, secret.created or config.created with source service_apply, never with a value. " +
			"If anything after the first creation fails, every object this apply made is removed again in reverse order (audited with source service_apply_cleanup); the problem's detail says so and its errors carry the reason on its own (location reason) and the objects as removed and notRemoved, each as kind:name. " +
			"Also 422 too_many_objects for more than 16 objects, 422 object_duplicate for the same kind and name twice, the name, value, labels, templating and subnet problems of the secrets, configs and networks routes for a bad object, 409 name_taken when an object's name exists already (nothing is retried), and 403 object_permission, before anything is created, for a caller without networks.manage, secrets.manage or configs.manage (Env Admin) for a kind it would create. " + problems +
			"No answer, error or audit row carries a value of the environment, the command line, the health check or the log options for a caller without secrets.reveal, or the value of a staged secret or config. " +
			"Needs services.control (Operator) on the environment. Every attempt on a service that exists is audited as service.updated, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusAccepted, MaxBodyBytes: maxApplyBody,
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceApplyInput) (*ServiceApplyOutput, error) {
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
	return reviewBody(d, envID, res)
}

// reviewBody is the answer of a review of an edit or of a create.
func reviewBody(d ServicesDeps, envID string, res swarmops.ReviewResult) (ServiceReviewBody, error) {
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

// objectPermissions are the permission each kind of object needs to be created (D-13).
var objectPermissions = map[string]rbac.Perm{
	swarmops.KindNetwork: rbac.PermNetworksManage,
	swarmops.KindSecret:  rbac.PermSecretsManage,
	swarmops.KindConfig:  rbac.PermConfigsManage,
}

// objectNouns are how the kinds are worded in a sentence about several of them.
var objectNouns = map[string]string{swarmops.KindNetwork: "networks", swarmops.KindSecret: "secrets", swarmops.KindConfig: "configs"}

// stagedObjects checks the objects of an apply the way the networks, secrets and configs routes
// check theirs, before anything is sent, and turns them into what swarmops creates from. The caller
// clears the values when it is done.
func stagedObjects(in []ServiceStagedObject) ([]swarmops.StagedObject, error) {
	if len(in) > maxStagedObjects {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemTooManyObjects,
			"One change can create at most "+strconv.Itoa(maxStagedObjects)+" objects.")
	}
	out := make([]swarmops.StagedObject, 0, len(in))
	seen := map[string]bool{}
	fail := func(err error) ([]swarmops.StagedObject, error) {
		for _, o := range out {
			clear(o.Data)
		}
		return nil, err
	}
	for _, o := range in {
		if !secretNameRe.MatchString(o.Name) {
			return fail(httpx.Problem(http.StatusUnprocessableEntity, problemNameInvalid,
				"Use letters, numbers, dot, dash or underscore, up to 64 characters, starting and ending with a letter or number."))
		}
		key := o.Kind + "/" + o.Name
		if seen[key] {
			return fail(httpx.Problem(http.StatusUnprocessableEntity, problemObjectDuplicate, "The "+o.Kind+" "+o.Name+" is listed twice."))
		}
		seen[key] = true
		so := swarmops.StagedObject{Kind: o.Kind, Name: o.Name}
		var err error
		switch o.Kind {
		case swarmops.KindNetwork:
			if err = validateAddressing(o.Subnet, "", ""); err == nil {
				so.Subnet = o.Subnet
			}
		case swarmops.KindSecret:
			if so.Data, err = decodeSecretValue(o.Data); err == nil {
				err = validateSecretLabels(o.Labels)
			}
			so.Labels = o.Labels
		case swarmops.KindConfig:
			if so.Templating, _, err = configTemplating(o.Templating); err == nil {
				if so.Data, err = decodeConfigValue(o.Data); err == nil {
					err = validateSecretLabels(o.Labels)
				}
			}
			so.Labels = o.Labels
		default:
			err = httpx.Problem(http.StatusUnprocessableEntity, problemNameInvalid, "Unknown kind of object "+o.Kind+".")
		}
		out = append(out, so)
		if err != nil {
			return fail(err)
		}
	}
	return out, nil
}

// applyDeps are swarmopsDeps with the two hooks that need the caller: whether they may create a
// kind of object, and the audit row of each object an apply makes or removes.
func applyDeps(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, userAgent string) swarmops.Deps {
	sd := swarmopsDeps(d)
	sd.CanCreate = func(kind string) bool {
		perm, ok := objectPermissions[kind]
		return ok && d.Checker.Check(ctx, p, rbac.Requirement{Perm: perm, Scope: rbac.ScopeEnvironment}, envID).Allowed
	}
	sd.Record = func(e audit.Entry) {
		e.Actor, e.EnvID, e.IP, e.UserAgent = p.AuditActor(), envID, httpx.ClientIP(ctx), userAgent
		wctx, cancel := doneActionAuditContext(ctx)
		defer cancel()
		if err := d.Audit.Record(wctx, e); err != nil {
			// The object has been made or removed and the caller must still be told which; the
			// lost row is the server's fault and is logged.
			d.Log.Error("could not audit an object of a service apply", "action", e.Action, "object", e.Details["name"], "error", err)
		}
	}
	return sd
}

// applyServiceUpdate sends a spec once. The write is sent and seen through, so a browser that goes
// away while it runs does not turn an update that happened into a failure.
func applyServiceUpdate(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID string, in *ServiceApplyInput) (ServiceApplyBody, error) {
	objects, err := stagedObjects(in.Body.Objects)
	if err != nil {
		return ServiceApplyBody{}, err
	}
	defer func() {
		for _, o := range objects {
			clear(o.Data)
		}
	}()
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceApplyBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	sd := applyDeps(writeCtx, d, p, envID, in.UserAgent)
	var res swarmops.ApplyResult
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.Apply(writeCtx, sd, swarmops.ApplyInput{
			EnvID: envID, Client: c, ServiceID: in.ServiceID, Version: in.Body.Version, Spec: json.RawMessage(in.Body.Spec), Caller: caller, Objects: objects,
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
		return ServiceApplyBody{}, withCleanup(problem, "update", res.Name, res.Cleanup)
	}
	refreshClusters(ctx, d, envID)
	auditServiceAction(ctx, d, p, envID, in.UserAgent, audit.ActionServiceUpdated, res.ServiceID, in.ServiceID, details(actionDone), actionDone)
	return ServiceApplyBody{
		Image: res.Image.Ref, Digest: res.Image.Digest, Pinned: res.Image.Pinned, Warnings: nonNil(res.Warnings),
		Steps: stepBodies(res.Steps), Cleanup: ServiceCleanupBody{Removed: objectBodies(res.Cleanup.Removed), NotRemoved: objectBodies(res.Cleanup.NotRemoved)},
	}, nil
}

func stepBodies(steps []swarmops.ApplyStep) []ServiceApplyStepBody {
	out := make([]ServiceApplyStepBody, len(steps))
	for i, s := range steps {
		out[i] = ServiceApplyStepBody{Step: s.Step, Kind: s.Kind, Name: s.Name, State: s.State, Code: s.Code}
	}
	return out
}

func objectBodies(objects []swarmops.CreatedObject) []ServiceObjectBody {
	out := make([]ServiceObjectBody, len(objects))
	for i, o := range objects {
		out[i] = ServiceObjectBody{Kind: o.Kind, Name: o.Name, ID: o.ID}
	}
	return out
}

// withCleanup adds what the cleanup of a failed apply did to its problem: the sentence that says
// the objects made for the change were removed, or names what could not be, and, in the problem's
// errors, the reason on its own and the lists as kind:name. A problem is left as it is when the apply made nothing.
func withCleanup(problem error, verb, service string, rep swarmops.CleanupReport) error {
	m, ok := problem.(*huma.ErrorModel)
	n := len(rep.Removed) + len(rep.NotRemoved)
	if !ok || n == 0 {
		return problem
	}
	tail := fmt.Sprintf("The %d objects created for this change were removed.", n)
	if len(rep.NotRemoved) > 0 {
		left := make([]string, len(rep.NotRemoved))
		for i, o := range rep.NotRemoved {
			left[i] = o.Kind + " " + o.Name
		}
		tail = "Bayplan couldn't remove " + strings.Join(left, ", ") + ". Remove them under Networks, Secrets or Configs."
	}
	reason := m.Detail
	m.Detail = "Couldn't " + verb + " " + service + ". " + reason + " " + tail
	// The reason on its own, so a page can word the whole sentence itself.
	m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "reason", Message: reason})
	for _, o := range rep.Removed {
		m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "removed", Message: o.Kind + ":" + o.Name})
	}
	for _, o := range rep.NotRemoved {
		m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "notRemoved", Message: o.Kind + ":" + o.Name})
	}
	return m
}

// kindsSentence words kinds of object as "networks", "networks and secrets", "networks, secrets and configs".
func kindsSentence(kinds []string) string {
	nouns := make([]string, len(kinds))
	for i, k := range kinds {
		nouns[i] = objectNouns[k]
	}
	if len(nouns) <= 1 {
		return strings.Join(nouns, "")
	}
	return strings.Join(nouns[:len(nouns)-1], ", ") + " and " + nouns[len(nouns)-1]
}

// serviceEditProblem turns the error of a review or an apply into the code its audit row records and
// the problem the SPA words. None of the sentences holds a value of the spec.
func serviceEditProblem(d ServicesDeps, envID string, err error) (string, error) {
	return serviceWriteProblem(d, envID, problemUpdateFailed, err)
}

// serviceWriteProblem is serviceEditProblem with the type of the problem for a refusal that is none of
// the known ones: update_failed for an edit, create_failed for a create.
func serviceWriteProblem(d ServicesDeps, envID, failed string, err error) (string, error) {
	problem := func(status int, typ, detail string, errs ...*huma.ErrorDetail) (string, error) {
		return typ, &huma.ErrorModel{Type: typ, Title: http.StatusText(status), Status: status, Detail: detail, Errors: errs}
	}
	var (
		invalid  *swarmops.SpecInvalidError
		owned    *swarmops.StackOwnedError
		refused  *swarmops.RefusedError
		reveal   *swarmops.RevealRequiredError
		missing  *swarmops.MissingError
		unres    *swarmops.ImageUnresolvedError
		rejected *swarmops.RejectedError
		ph       *redact.PlaceholderError
		perm     *swarmops.ObjectPermissionError
		made     *swarmops.ObjectCreateError
		taken    *swarmops.NameTakenError
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
		// One entry per missing object, located by its kind, and one part of the sentence per kind that
		// has any: a kind with nothing missing is not named.
		var (
			errs  []*huma.ErrorDetail
			parts []string
		)
		for _, k := range []struct {
			kind  string
			names []string
		}{
			{"networks", missing.Missing.Networks},
			{"secrets", missing.Missing.Secrets},
			{"configs", missing.Missing.Configs},
		} {
			if len(k.names) == 0 {
				continue
			}
			for _, n := range k.names {
				errs = append(errs, &huma.ErrorDetail{Location: k.kind, Message: n})
			}
			parts = append(parts, k.kind+" "+strings.Join(k.names, ", "))
		}
		return problem(http.StatusUnprocessableEntity, problemObjectsMissing,
			"The spec names objects that don't exist: "+strings.Join(parts, "; ")+". Create them first.", errs...)
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
	case errors.As(err, &reveal):
		// The names are what the spec used to reach the secret, never a value.
		errs := make([]*huma.ErrorDetail, len(reveal.Secrets))
		names := make([]string, len(reveal.Secrets))
		for i, s := range reveal.Secrets {
			errs[i] = &huma.ErrorDetail{Location: s.Path, Message: s.Name}
			names[i] = s.Name
		}
		return problem(http.StatusForbidden, problemSecretRevealRequired,
			"Adding a secret to a service needs the reveal secrets permission: "+strings.Join(names, ", ")+". Ask an administrator to add it.", errs...)
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
	case errors.As(err, &perm):
		return problem(http.StatusForbidden, problemObjectPermission, "Needs Admin to create "+kindsSentence(perm.Kinds)+".")
	case errors.As(err, &made) && errors.Is(err, dockerx.ErrNameTaken):
		return problem(http.StatusConflict, problemNameTaken, "A "+made.Kind+" named "+made.Name+" already exists.",
			&huma.ErrorDetail{Location: "object", Message: made.Kind + ":" + made.Name})
	case errors.As(err, &made):
		return serviceActionProblem(d, envID, problemCreateFailed, made.Err)
	case errors.Is(err, swarmops.ErrImageRequired):
		return problem(http.StatusUnprocessableEntity, problemImageRequired, "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.")
	case errors.Is(err, swarmops.ErrNameInvalid):
		return problem(http.StatusUnprocessableEntity, problemNameInvalid,
			"Use letters, numbers, dot, dash or underscore, up to 63 characters.")
	case errors.As(err, &taken):
		return problem(http.StatusConflict, problemServiceNameTaken, "A service named "+taken.Name+" already exists.")
	case errors.As(err, &rejected):
		return problem(http.StatusUnprocessableEntity, problemSpecRejected, "Docker rejected these settings: "+clipDetail(rejected.Message))
	case errors.Is(err, swarmops.ErrRegistryUnresolved):
		d.Log.Error("could not resolve the registry credential of a service update", "environment", envID, "error", err)
		err = errRegistryUnresolved
	}
	return serviceActionProblem(d, envID, failed, err)
}
