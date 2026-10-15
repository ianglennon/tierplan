// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
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

var _ = reflect.TypeFor[SpecJSON]

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

// registerServiceEdit mounts the review and apply routes.
func registerServiceEdit(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "reviewServiceUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/review",
		Summary:     "Review a full service spec",
		Description: "Judges a full service spec without changing Docker.",
		Tags:        []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceEditInput) (*ServiceReviewOutput, error) {
			return nil, huma.Error501NotImplemented("not implemented")
		})

	httpx.Register(r, huma.Operation{
		OperationID: "applyServiceUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/services/{serviceId}/apply",
		Summary:     "Apply a full service spec",
		Description: "Sends a full service spec once.",
		Tags:        []string{"environments"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceEditInput) (*ServiceApplyOutput, error) {
			return nil, huma.Error501NotImplemented("not implemented")
		})
}
