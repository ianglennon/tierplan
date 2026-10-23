// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/swarmops"
)

// rotateTimeout bounds one rotate: it makes an object and then updates the services that use the old
// one, one at a time, each a write of its own.
const rotateTimeout = 5 * time.Minute

// RotatePreviewRequest is the body of the two preview routes.
type RotatePreviewRequest struct {
	NewName string `json:"newName,omitempty" maxLength:"128" doc:"The name the new version would have; left out, the next version of the old name ({base}_v{n}: the old name without a trailing _v<digits>, then the lowest unused number above the old version). Not a valid name is 422 name_invalid; a taken one is nameTaken in the answer"`
}

// RotateUserBody is one service that uses the object being rotated.
type RotateUserBody struct {
	ServiceID string         `json:"serviceId" doc:"The service ID"`
	Service   string         `json:"service" doc:"The service name"`
	Stack     string         `json:"stack" doc:"The stack that owns the service; empty for a standalone service"`
	Skipped   bool           `json:"skipped" doc:"True for a service a stack owns: a rotate does not change it, because the stack's next deploy would put the old object back"`
	File      string         `json:"file" doc:"The file name or path the object appears as in the container; kept by the rotate"`
	UID       string         `json:"uid" doc:"The owner of the file; kept"`
	GID       string         `json:"gid" doc:"The group of the file; kept"`
	Mode      string         `json:"mode" doc:"The file mode in octal; kept"`
	Current   map[string]any `json:"current,omitempty" doc:"The service's spec as it is, masked for a caller without secrets.reveal; left out for a skipped service"`
	Next      map[string]any `json:"next,omitempty" doc:"The spec after the rotate, masked in the same way, with the reference to the new object and no ID, which it doesn't have yet; left out for a skipped service"`
}

// RotatePreviewBody is what a rotate would do.
type RotatePreviewBody struct {
	Kind       string            `json:"kind" enum:"secret,config" doc:"secret or config"`
	ID         string            `json:"id" doc:"The ID of the object to replace"`
	Name       string            `json:"name" doc:"Its name"`
	NewName    string            `json:"newName" doc:"The name the new version would have"`
	NameTaken  bool              `json:"nameTaken" doc:"True when the swarm has an object of that name already, so the rotate would answer 409 name_taken"`
	Templating string            `json:"templating" enum:"none,golang," doc:"A config's templating (none or golang); empty for a secret"`
	Labels     map[string]string `json:"labels" doc:"The labels the new version would be given: the old object's"`
	Users      []RotateUserBody  `json:"users" doc:"The services whose spec references the object, in name order"`
	Skipped    int               `json:"skipped" doc:"How many of them a stack owns"`
}

// RotatePreviewOutput wraps RotatePreviewBody for huma.
type RotatePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RotatePreviewBody
}

// SecretRotatePreviewInput is the path and body of the secret preview route.
type SecretRotatePreviewInput struct {
	SecretPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      RotatePreviewRequest
}

// ConfigRotatePreviewInput is the path and body of the config preview route.
type ConfigRotatePreviewInput struct {
	ConfigPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      RotatePreviewRequest
}

// SecretRotateRequest is the body of POST /api/env/{envId}/secrets/{secretId}/rotate.
type SecretRotateRequest struct {
	Name      string `json:"name,omitempty" doc:"The new secret's name, as for a create (else 422 name_invalid); left out, the next version of the old name"`
	Data      string `json:"data,omitempty" doc:"The new value, as standard base64, with the limits of a create: empty is 422 value_required, 512,000 bytes or more 422 value_too_large, text that is not base64 422 value_invalid. It is sent to the swarm once and never kept, logged, audited or returned"`
	RemoveOld bool   `json:"removeOld,omitempty" doc:"Remove the old secret when no service spec names it any more"`
}

// ConfigRotateRequest is the body of POST /api/env/{envId}/configs/{configId}/rotate.
type ConfigRotateRequest struct {
	Name       string `json:"name,omitempty" doc:"The new config's name, as for a create (else 422 name_invalid); left out, the next version of the old name"`
	Data       string `json:"data,omitempty" doc:"The new content, as standard base64, with the limits of a create: empty is 422 value_required, 1,024,000 bytes or more 422 value_too_large, text that is not base64 422 value_invalid"`
	Templating string `json:"templating,omitempty" doc:"none (the default) or golang, as for a create (else 422 templating_invalid). The page prefills the old config's"`
	RemoveOld  bool   `json:"removeOld,omitempty" doc:"Remove the old config when no service spec names it any more"`
}

// SecretRotateInput is the path and body of the secret rotate route.
type SecretRotateInput struct {
	SecretPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      SecretRotateRequest
}

// ConfigRotateInput is the path and body of the config rotate route.
type ConfigRotateInput struct {
	ConfigPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ConfigRotateRequest
}

// RotateServiceBody is what a rotate did to one service that used the object.
type RotateServiceBody struct {
	ServiceID string `json:"serviceId" doc:"The service ID"`
	Service   string `json:"service" doc:"The service name"`
	Stack     string `json:"stack" doc:"The stack that owns a skipped service"`
	State     string `json:"state" enum:"updated,skipped,unchanged" doc:"updated (now uses the new object; its rollout continues in the swarm), skipped (a stack owns it) or unchanged (it no longer used the old object when it was read)"`
	Image     string `json:"image" doc:"The image the service was updated with; empty when it was not updated"`
	Digest    string `json:"digest" doc:"Its digest; empty when it names none"`
}

// RotateBody is what a rotate did.
type RotateBody struct {
	ID         string              `json:"id" doc:"The ID of the new object"`
	Name       string              `json:"name" doc:"Its name"`
	OldName    string              `json:"oldName" doc:"The name of the object it replaces"`
	Services   []RotateServiceBody `json:"services" doc:"The services that used the old object, in the order they were taken"`
	Updated    int                 `json:"updated" doc:"How many services now use the new object"`
	Skipped    int                 `json:"skipped" doc:"How many services a stack owns, which were left alone"`
	OldRemoved bool                `json:"oldRemoved" doc:"True when the old object was removed"`
	RemoveCode string              `json:"removeCode" enum:"object_in_use,remove_failed," doc:"Why the old object stayed although removeOld was asked for: object_in_use (a service spec, a stack's, still names it) or remove_failed; empty otherwise"`
}

// RotateOutput wraps RotateBody for huma.
type RotateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RotateBody
}

// rotateKind is what differs between rotating a secret and a config: the words, the permission, the
// audit action and how a failure of the manager is turned into a problem.
type rotateKind struct {
	kind, noun string
	// action is the audit action of a rotate, targetType the type of the object it is about.
	action, targetType string
	// manage is the permission the route needs; route is how a refusal of the services.control check is
	// recorded.
	previewRoute, rotateRoute string
	// problem turns the error of a read or a write through a manager into the code an audit row records and
	// the problem the caller is answered with.
	problem func(envID, failedType, name string, err error) (string, error)
}

func (d SecretsDeps) rotateKind() rotateKind {
	return rotateKind{
		kind: swarmops.KindSecret, noun: "secret", action: audit.ActionSecretRotated, targetType: secretTargetType,
		previewRoute: "POST /api/env/{envId}/secrets/{secretId}/rotate/preview", rotateRoute: "POST /api/env/{envId}/secrets/{secretId}/rotate",
		problem: func(envID, failedType, name string, err error) (string, error) {
			return secretWriteProblem(d, envID, failedType, name, err)
		},
	}
}

func (d ConfigsDeps) rotateKind() rotateKind {
	return rotateKind{
		kind: swarmops.KindConfig, noun: "config", action: audit.ActionConfigRotated, targetType: configTargetType,
		previewRoute: "POST /api/env/{envId}/configs/{configId}/rotate/preview", rotateRoute: "POST /api/env/{envId}/configs/{configId}/rotate",
		problem: func(envID, failedType, name string, err error) (string, error) {
			return configWriteProblem(d, envID, failedType, name, err)
		},
	}
}

// registerSecretRotate mounts the two secret rotate routes.
func registerSecretRotate(r *httpx.Router, d SecretsDeps) {
	k, sd := d.rotateKind(), d.services()
	httpx.Register(r, huma.Operation{
		OperationID: "previewSecretRotate", Method: http.MethodPost, Path: "/api/env/{envId}/secrets/{secretId}/rotate/preview",
		Summary: "Preview the rotate of a swarm secret",
		Description: "What a new version of the secret would do, without changing anything: the name the new secret would have (the next {base}_v{n}, or the one given in newName, with nameTaken when the swarm has it), the labels it would copy, and every service whose spec references the secret in name order, with how it mounts the secret and, for a standalone service, its spec as it is and as it would be (current and next, masked for a caller without secrets.reveal). Services a stack owns are listed as skipped. " +
			"404 secret_not_found; 409 not_a_swarm. Needs secrets.manage (Admin) and services.control (Operator) on the environment; a caller without the second is refused 403 and audited as permission.denied. A preview is not audited otherwise.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermSecretsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretRotatePreviewInput) (*RotatePreviewOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := previewRotate(ctx, sd, k, p, env.ID, in.UserAgent, in.SecretID, in.Body.NewName)
			if err != nil {
				return nil, err
			}
			return &RotatePreviewOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "rotateSecret", Method: http.MethodPost, Path: "/api/env/{envId}/secrets/{secretId}/rotate",
		Summary: "Rotate a swarm secret to a new version",
		Description: "Creates a new secret with the new value under {base}_v{n} (or the name given), copies the old secret's labels, points every standalone service that uses the old secret at the new one, with the same file name, owner, group and mode, and removes the old secret when removeOld is set and nothing uses it any more. " +
			"Each service is updated through the same steps as an edit (the host-escalation policy on its whole spec for the caller, the registry credential, the version just read), one at a time in name order and never retried. Services a stack owns are skipped. " +
			"The first failure stops the run and leaves the old secret and the services after it as they were: the answer is that failure's problem (for example 409 service_changed for a service somebody edited in between, 422 spec_rejected, 403 privileged_service), with errors listing the new secret (created), the services already moved (updated), the stack's (skipped), the one it stopped at (failedAt) and the reason, and a detail that says Updated k of n services. A failed create of the new secret (409 name_taken, 422 spec_rejected, ...) changes nothing else and says so. " +
			"The value goes to the swarm once and is never kept, logged, audited or returned. A body over 1 MiB is refused. " +
			"Needs secrets.manage (Admin) and services.control (Operator) on the environment; a caller without the second is refused 403 and audited as permission.denied. " +
			"Audited as secret.rotated (from, to, updated, skipped, removed_old, code) with the new secret as secret.created (source rotate), the old one as secret.removed when it was removed and each service attempted as service.updated, never with a value.",
		Tags: []string{"environments"}, MaxBodyBytes: maxSecretBody,
	}, rbac.Requirement{Perm: rbac.PermSecretsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretRotateInput) (*RotateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			data, err := decodeSecretValue(in.Body.Data)
			if err != nil {
				return nil, err
			}
			defer clear(data)
			body, err := rotateObject(ctx, sd, k, p, env.ID, in.UserAgent, rotateParams{
				id: in.SecretID, name: in.Body.Name, data: data, removeOld: in.Body.RemoveOld,
			})
			if err != nil {
				return nil, err
			}
			return &RotateOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// registerConfigRotate mounts the two config rotate routes.
func registerConfigRotate(r *httpx.Router, d ConfigsDeps) {
	k, sd := d.rotateKind(), d.services()
	httpx.Register(r, huma.Operation{
		OperationID: "previewConfigRotate", Method: http.MethodPost, Path: "/api/env/{envId}/configs/{configId}/rotate/preview",
		Summary: "Preview the rotate of a swarm config",
		Description: "What a new version of the config would do, without changing anything: the name the new config would have (the next {base}_v{n}, or the one given in newName, with nameTaken when the swarm has it), its templating and labels, which the new version copies, and every service whose spec references the config in name order, with how it mounts the config and, for a standalone service, its spec as it is and as it would be (current and next, masked for a caller without secrets.reveal). Services a stack owns are listed as skipped. " +
			"404 config_not_found; 409 not_a_swarm. Needs configs.manage (Admin) and services.control (Operator) on the environment; a caller without the second is refused 403 and audited as permission.denied. A preview is not audited otherwise.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermConfigsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigRotatePreviewInput) (*RotatePreviewOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := previewRotate(ctx, sd, k, p, env.ID, in.UserAgent, in.ConfigID, in.Body.NewName)
			if err != nil {
				return nil, err
			}
			return &RotatePreviewOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "rotateConfig", Method: http.MethodPost, Path: "/api/env/{envId}/configs/{configId}/rotate",
		Summary: "Rotate a swarm config to a new version",
		Description: "Creates a new config with the new content under {base}_v{n} (or the name given), with the templating given and the old config's labels, points every standalone service that uses the old config at the new one, with the same file name or path, owner, group and mode, and removes the old config when removeOld is set and nothing uses it any more. " +
			"Each service is updated through the same steps as an edit (the host-escalation policy on its whole spec for the caller, the registry credential, the version just read), one at a time in name order and never retried. Services a stack owns are skipped. " +
			"The first failure stops the run and leaves the old config and the services after it as they were: the answer is that failure's problem (for example 409 service_changed, 422 spec_rejected, 403 privileged_service), with errors listing the new config (created), the services already moved (updated), the stack's (skipped), the one it stopped at (failedAt) and the reason, and a detail that says Updated k of n services. A failed create of the new config (409 name_taken, ...) changes nothing else and says so. " +
			"A body over 2 MiB is refused. " +
			"Needs configs.manage (Admin) and services.control (Operator) on the environment; a caller without the second is refused 403 and audited as permission.denied. " +
			"Audited as config.rotated (from, to, updated, skipped, removed_old, code) with the new config as config.created (source rotate), the old one as config.removed when it was removed and each service attempted as service.updated.",
		Tags: []string{"environments"}, MaxBodyBytes: maxConfigBody,
	}, rbac.Requirement{Perm: rbac.PermConfigsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigRotateInput) (*RotateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			driver, _, err := configTemplating(in.Body.Templating)
			if err != nil {
				return nil, err
			}
			data, err := decodeConfigValue(in.Body.Data)
			if err != nil {
				return nil, err
			}
			defer clear(data)
			body, err := rotateObject(ctx, sd, k, p, env.ID, in.UserAgent, rotateParams{
				id: in.ConfigID, name: in.Body.Name, data: data, templating: driver, removeOld: in.Body.RemoveOld,
			})
			if err != nil {
				return nil, err
			}
			return &RotateOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// requireServicesControl is the second permission check of a rotate, which updates services. A caller
// without it is refused before any call, and the refusal is audited as permission.denied like a route
// refusal (T-06-81).
func requireServicesControl(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, userAgent, route string) error {
	var dec rbac.Decision
	if d.Checker != nil {
		dec = d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermServicesControl, Scope: rbac.ScopeEnvironment}, envID)
	}
	if d.Checker == nil || dec.Err != nil {
		d.Log.Error("could not check services.control", "error", dec.Err)
		return huma.Error500InternalServerError("internal error")
	}
	if dec.Allowed {
		return nil
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
		Reason: rbac.ReasonForbidden, TargetType: "route", TargetID: route, EnvID: envID,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: map[string]string{"permission": string(rbac.PermServicesControl)},
	}); err != nil {
		// A refusal nobody can see must not look like a normal one.
		d.Log.Error("could not audit a refusal", "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	return httpx.Problem(http.StatusForbidden, "forbidden", "Rotating a secret or config changes services, which needs the control services permission")
}

// previewRotate answers what a rotate would do. It only reads, so a read that has to be run again on
// another manager is safe.
func previewRotate(ctx context.Context, d ServicesDeps, k rotateKind, p *rbac.Principal, envID, userAgent, id, newName string) (RotatePreviewBody, error) {
	if newName != "" && !secretNameRe.MatchString(newName) {
		return RotatePreviewBody{}, rotateNameProblem()
	}
	if err := requireSwarm(ctx, d, envID); err != nil {
		return RotatePreviewBody{}, err
	}
	if err := requireServicesControl(ctx, d, p, envID, userAgent, k.previewRoute); err != nil {
		return RotatePreviewBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	readCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var res swarmops.RotatePreviewResult
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.RotatePreview(readCtx, swarmopsDeps(d), swarmops.RotatePreviewInput{
			EnvID: envID, Client: c, Kind: k.kind, OldID: id, NewName: newName, Caller: caller,
		})
		return e
	})
	if err != nil {
		_, problem := k.problem(envID, problemUpdateFailed, id, err)
		return RotatePreviewBody{}, problem
	}
	body := RotatePreviewBody{
		Kind: res.Kind, ID: res.OldID, Name: res.OldName, NewName: res.NewName, NameTaken: res.NameTaken,
		Labels: res.Labels, Users: make([]RotateUserBody, 0, len(res.Users)), Skipped: res.Skipped,
	}
	if res.Kind == swarmops.KindConfig {
		body.Templating = configTemplatingLabel(res.Templating)
	}
	for _, u := range res.Users {
		ub := RotateUserBody{
			ServiceID: u.ServiceID, Service: u.Name, Stack: u.Stack, Skipped: u.Skipped,
			File: u.File, UID: u.UID, GID: u.GID, Mode: fileMode(os.FileMode(u.Mode)),
		}
		if !u.Skipped {
			if ub.Current, err = specMap(u.Current); err != nil {
				d.Log.Error("reading a spec for a rotate preview failed", "environment", envID, "error", err)
				return RotatePreviewBody{}, huma.Error500InternalServerError("internal error")
			}
			if ub.Next, err = specMap(u.Next); err != nil {
				d.Log.Error("reading a spec for a rotate preview failed", "environment", envID, "error", err)
				return RotatePreviewBody{}, huma.Error500InternalServerError("internal error")
			}
		}
		body.Users = append(body.Users, ub)
	}
	return body, nil
}

// specMap turns a spec into the JSON object the page reads, as the review does.
func specMap(spec any) (map[string]any, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return decodeInspect(raw)
}

func rotateNameProblem() error {
	return httpx.Problem(http.StatusUnprocessableEntity, problemNameInvalid,
		"Use letters, numbers, dot, dash or underscore, up to 64 characters, starting and ending with a letter or number.")
}

// rotateParams are the checked parts of a rotate request.
type rotateParams struct {
	id, name   string
	data       []byte
	templating string
	removeOld  bool
}

// rotateObject rotates one object. The write is sent and seen through, so a browser that goes away while
// it runs does not turn a rotate that happened into a failure.
func rotateObject(ctx context.Context, d ServicesDeps, k rotateKind, p *rbac.Principal, envID, userAgent string, rp rotateParams) (RotateBody, error) {
	if rp.name != "" && !secretNameRe.MatchString(rp.name) {
		return RotateBody{}, rotateNameProblem()
	}
	if err := requireSwarm(ctx, d, envID); err != nil {
		return RotateBody{}, err
	}
	if err := requireServicesControl(ctx, d, p, envID, userAgent, k.rotateRoute); err != nil {
		return RotateBody{}, err
	}
	caller := swarmops.Caller{Principal: p, Reveal: redact.CanReveal(ctx, d.Checker, p, envID)}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotateTimeout)
	defer cancel()
	sd := applyDeps(writeCtx, d, p, envID, userAgent)
	var res swarmops.RotateResult
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		res, e = swarmops.Rotate(writeCtx, sd, swarmops.RotateInput{
			EnvID: envID, Client: c, Kind: k.kind, OldID: rp.id, NewName: rp.name, Data: rp.data, Templating: rp.templating,
			RemoveOld: rp.removeOld, Caller: caller,
		})
		return e
	})

	code, problem := actionDone, error(nil)
	if err != nil {
		code, problem = rotateProblem(ctx, d, k, envID, rp.id, res, err)
	}
	if res.Created {
		auditRotatedServices(ctx, d, p, envID, userAgent, res)
		refreshClusters(ctx, d, envID)
	}
	if code != k.notFoundCode() {
		auditRotated(ctx, d, k, p, envID, userAgent, rp.id, res, code)
	}
	if problem != nil {
		return RotateBody{}, problem
	}
	return rotateBody(res), nil
}

// notFoundCode is the code a rotate of an object that is not there is recorded with: it is not audited,
// as a delete of one is not.
func (k rotateKind) notFoundCode() string { return k.noun + "_not_found" }

// rotateProblem turns the error of a rotate into the code its audit row records and the problem the
// caller is answered with. A run that stopped part-way says what moved and that the old object stays; a
// create that failed says nothing else changed.
func rotateProblem(ctx context.Context, d ServicesDeps, k rotateKind, envID, id string, res swarmops.RotateResult, err error) (string, error) {
	var (
		stop *swarmops.RotateStopError
		made *swarmops.ObjectCreateError
	)
	switch {
	case errors.As(err, &stop):
		code, problem := serviceEditProblem(ctx, d, envID, stop.Err)
		return code, rotateStopped(problem, k, res)
	case errors.As(err, &made):
		code, problem := k.problem(envID, problemCreateFailed, made.Name, made.Err)
		return code, rotateNotCreated(problem, res.NewName)
	}
	name := res.OldName
	if name == "" {
		name = id
	}
	return k.problem(envID, problemUpdateFailed, name, err)
}

// rotateNotCreated words the failure to create the new object: nothing else changed.
func rotateNotCreated(problem error, newName string) error {
	m, ok := problem.(*huma.ErrorModel)
	if !ok {
		return problem
	}
	reason := m.Detail
	m.Detail = "Couldn't create " + newName + ". " + reason + " Nothing else changed."
	m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "reason", Message: reason})
	return m
}

// rotateStopped words a run that stopped at a service. The detail says how many of the standalone
// services moved and that the old object stays; the errors carry the new object (created), the services
// that moved (updated), those a stack owns (skipped), the one it stopped at (failedAt) and the reason on
// its own.
func rotateStopped(problem error, k rotateKind, res swarmops.RotateResult) error {
	m, ok := problem.(*huma.ErrorModel)
	if !ok {
		return problem
	}
	reason := m.Detail
	total := len(res.Services) - res.Skipped
	m.Detail = "Updated " + strconv.Itoa(res.Updated) + " of " + strconv.Itoa(total) + " services. " + res.OldName + " stays until the rest use " + res.NewName + ". " + reason
	m.Errors = append(m.Errors,
		&huma.ErrorDetail{Location: "reason", Message: reason},
		&huma.ErrorDetail{Location: "created", Message: res.NewName},
	)
	for _, s := range res.Services {
		switch s.State {
		case swarmops.RotateUpdated:
			m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "updated", Message: s.Name})
		case swarmops.RotateSkipped:
			m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "skipped", Message: s.Name})
		case swarmops.RotateFailed:
			m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "failedAt", Message: s.Name})
		case swarmops.RotatePending:
			m.Errors = append(m.Errors, &huma.ErrorDetail{Location: "pending", Message: s.Name})
		}
	}
	return m
}

func rotateBody(res swarmops.RotateResult) RotateBody {
	body := RotateBody{
		ID: res.NewID, Name: res.NewName, OldName: res.OldName, Services: make([]RotateServiceBody, 0, len(res.Services)),
		Updated: res.Updated, Skipped: res.Skipped, OldRemoved: res.OldRemoved, RemoveCode: res.RemoveCode,
	}
	for _, s := range res.Services {
		body.Services = append(body.Services, RotateServiceBody{
			ServiceID: s.ServiceID, Service: s.Name, Stack: s.Stack, State: s.State, Image: s.Image, Digest: s.Digest,
		})
	}
	return body
}

// auditRotatedServices writes the service.updated row of every service the rotate attempted: a success for
// one that moved, a failure with its code for the one it stopped at. A skipped or untouched service is
// not an attempt.
func auditRotatedServices(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, userAgent string, res swarmops.RotateResult) {
	for _, s := range res.Services {
		if s.State != swarmops.RotateUpdated && s.State != swarmops.RotateFailed {
			continue
		}
		code := actionDone
		if s.State == swarmops.RotateFailed {
			code = s.Code
		}
		details := map[string]string{
			"name": s.Name, "version": strconv.FormatUint(s.Version, 10), "image": s.Image, "digest": s.Digest,
			"changed": "true", "findings": strconv.Itoa(s.Findings), "code": code, "source": swarmops.SourceRotate,
		}
		auditServiceAction(ctx, d, p, envID, userAgent, audit.ActionServiceUpdated, s.ServiceID, s.Name, details, code)
	}
}

// auditRotated writes the row of the rotate itself, about the object it replaced: a success when code is
// done, else a failure whose reason is the code. It is written on a context that outlives the request.
func auditRotated(ctx context.Context, d ServicesDeps, k rotateKind, p *rbac.Principal, envID, userAgent, asked string, res swarmops.RotateResult, code string) {
	result, reason := audit.ResultSuccess, ""
	if code != actionDone {
		result, reason = audit.ResultFailure, code
	}
	id := res.OldID
	if id == "" {
		id = asked
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: k.action, TargetType: k.targetType, TargetID: id,
		EnvID: envID, Result: result, Reason: reason, IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: map[string]string{
			"from": res.OldName, "to": res.NewName, "updated": strconv.Itoa(res.Updated), "skipped": strconv.Itoa(res.Skipped),
			"removed_old": strconv.FormatBool(res.OldRemoved), "code": code,
		},
	}); err != nil {
		// The change has been made or refused and the caller must still be told which; the lost row is
		// the server's fault and is logged.
		d.Log.Error("could not audit a rotate", "action", k.action, k.noun, id, "error", err)
	}
}
