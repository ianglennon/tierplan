// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// TierPlan's own route over the typed API (Phase 28, SELF-01, D-17, D-11): an instance Admin previews switching it on in an
// environment, switches it on, previews switching it off and switches it off, the two switches as jobs. Every operation needs
// ingress.manage on the environment and, past that gate, an instance Admin, because the route changes which origins and proxy
// addresses TierPlan trusts install-wide. Switching it off is never refused because of the route's state or the environment's
// health (D-11).

// The problem codes of the self route operations.
const (
	problemInstanceAdminRequired = "instance_admin_required"
	problemSelfRouteNotOn        = "self_route_not_on"
)

const (
	instanceAdminRequiredText = "Only an instance admin can switch TierPlan's own route, because it changes which origins and proxy addresses TierPlan trusts."
	selfRouteNotOnText        = "TierPlan's own route is not on."
	selfRouteGateText         = "Needs ingress.manage on the environment and an instance admin."
)

// The paths of the self route operations.
const (
	selfRoutePath           = "/api/env/{envId}/ingress/self"
	selfRoutePreviewPath    = "/api/env/{envId}/ingress/self/preview"
	selfRouteOffPreviewPath = "/api/env/{envId}/ingress/self/off/preview"
)

// IngressSelfRouteRequest is the body of the preview and the switch-on.
type IngressSelfRouteRequest struct {
	Hostname string `json:"hostname" maxLength:"253" doc:"The hostname TierPlan is to be served on, under one of the environment's registered domains, for example tierplan.apps.example.org"`
}

// IngressSelfRouteInput is the path and the body of the preview and the switch-on.
type IngressSelfRouteInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      IngressSelfRouteRequest
}

// IngressSelfRoutePathInput is the path of the switch-off preview and the switch-off.
type IngressSelfRoutePathInput struct {
	EnvironmentPathInput
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// IngressSelfSnippetBody is the next step that puts TierPlan's swarm service on the self network.
type IngressSelfSnippetBody struct {
	Kind string `json:"kind" enum:"stack_file,service_update" doc:"stack_file is lines to add to TierPlan's stack file; service_update is a docker service update command"`
	Text string `json:"text" doc:"The lines or the command, with TierPlan's service name filled in. Show it as sent"`
}

// IngressSelfRouteItemBody is one item of a self route preview.
type IngressSelfRouteItemBody struct {
	Code    string                  `json:"code" doc:"The item: self_hostname, self_certificate, self_network, self_origin and self_carve_out for a switch-on; self_origin_removed and self_network_removed for a switch-off"`
	State   string                  `json:"state" enum:"ok,warning,refused" doc:"ok passes; warning tells you something and lets the switch go on; refused stops the switch-on"`
	Message string                  `json:"message" doc:"The server's sentence. Show it as sent"`
	Reason  string                  `json:"reason,omitempty" doc:"The stable code of a warning or a refusal, for example host_taken, vip_ingress_off, self_route_not_here or self_not_attached; absent when the item passed"`
	Snippet *IngressSelfSnippetBody `json:"snippet,omitempty" doc:"On the self_network item of a swarm service that is not on the tierplan-self network: the next step that puts it there"`
}

// IngressSelfRoutePreviewBody is the answer to a preview of switching TierPlan's own route on or off.
type IngressSelfRoutePreviewBody struct {
	Items         []IngressSelfRouteItemBody `json:"items" doc:"The items in order. Any item whose state is refused stops the switch-on; the switch-off has none"`
	Host          string                     `json:"host" doc:"The hostname as judged, lower-cased; for a switch-off, the hostname of the route that is on"`
	DirectAddress string                     `json:"directAddress" doc:"The address that keeps reaching TierPlan whatever the route does; empty when that is the page's own origin"`
}

// IngressSelfRoutePreviewOutput wraps IngressSelfRoutePreviewBody for huma.
type IngressSelfRoutePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressSelfRoutePreviewBody
}

// IngressSelfRouteStartBody is the answer to the switch-on and the switch-off: the job that does it.
type IngressSelfRouteStartBody struct {
	JobID string `json:"jobId" doc:"The job: read it at GET /api/env/{envId}/ingress/jobs/{jobId} or follow its stream"`
}

// IngressSelfRouteStartOutput wraps IngressSelfRouteStartBody for huma.
type IngressSelfRouteStartOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressSelfRouteStartBody
}

// ingressSelfRoutePreviewBody maps the deployer's preview to the body.
func ingressSelfRoutePreviewBody(pv ingressdeploy.SelfRoutePreview) IngressSelfRoutePreviewBody {
	body := IngressSelfRoutePreviewBody{Items: make([]IngressSelfRouteItemBody, 0, len(pv.Items)), Host: pv.Host, DirectAddress: pv.Direct}
	for _, it := range pv.Items {
		ib := IngressSelfRouteItemBody{Code: it.Code, State: it.State, Message: it.Message, Reason: it.Reason}
		if it.Snippet != nil {
			ib.Snippet = &IngressSelfSnippetBody{Kind: it.Snippet.Kind, Text: it.Snippet.Text}
		}
		body.Items = append(body.Items, ib)
	}
	return body
}

// requireInstanceAdmin is the second gate of every self route operation, after rbac.Require(rbac.PermIngressManage): the caller
// must be an instance Admin. A caller who is not is refused 403 instance_admin_required, and the refusal is audited as
// permission.denied the way the RBAC gate audits its own (D-49): when the row cannot be written the answer is 500, since a
// refusal nobody can see must not look like a normal one.
func requireInstanceAdmin(ctx context.Context, d IngressDeployDeps, p *rbac.Principal, method, path, envID, userAgent string) error {
	if d.Checker == nil {
		d.Log.Error("no permission checker for TierPlan's own route")
		return huma.Error500InternalServerError("internal error")
	}
	a, err := d.Checker.Access(ctx, p)
	if err != nil {
		d.Log.Error("could not read the caller's access", "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	if a.Active && a.InstanceAdmin {
		return nil
	}
	if d.Audit != nil {
		wctx, cancel := doneActionAuditContext(ctx)
		defer cancel()
		if err := d.Audit.Record(wctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
			Reason: problemInstanceAdminRequired, TargetType: "route", TargetID: method + " " + path, EnvID: envID,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		}); err != nil {
			d.Log.Error("could not audit a refusal; refusing with 500", "error", err)
			return huma.Error500InternalServerError("internal error")
		}
	}
	return httpx.Problem(http.StatusForbidden, problemInstanceAdminRequired, instanceAdminRequiredText)
}

// RegisterIngressSelfRoute registers the preview, the switch-on, the switch-off preview and the switch-off of TierPlan's own
// route. Each needs ingress.manage on the environment and an instance Admin.
func RegisterIngressSelfRoute(r *httpx.Router, d IngressDeployDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress is not available on this server.")
	}
	// gate answers 401 to a caller who is not signed in, refuses one who is not an instance Admin, and answers 503 when the
	// deployer is not wired. It returns the principal.
	gate := func(ctx context.Context, method, path, envID, userAgent string) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if err := requireInstanceAdmin(ctx, d, p, method, path, envID, userAgent); err != nil {
			return nil, err
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		return p, nil
	}
	fail := func(ctx context.Context, what string, err error) error {
		var locked *ingressdeploy.LockedError
		switch {
		case errors.As(err, &locked):
			name := ""
			if env := httpx.EnvFrom(ctx); env != nil {
				name = env.Name
			}
			return operationLocked(name, locked.Holder)
		case errors.Is(err, ingressdeploy.ErrSelfRouteNotEnabled):
			return httpx.Problem(http.StatusConflict, problemSelfRouteNotOn, selfRouteNotOnText)
		case errors.Is(err, envs.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		d.Log.Error("TierPlan's own route request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	request := func(b IngressSelfRouteRequest) ingressdeploy.SelfRouteRequest {
		return ingressdeploy.SelfRouteRequest{Hostname: strings.TrimSpace(b.Hostname)}
	}

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressSelfRoute",
		Method:      http.MethodPost,
		Path:        selfRoutePreviewPath,
		Summary:     "Preview switching TierPlan's own route on",
		Description: "Says what serving TierPlan itself on the hostname through this environment's ingress would do, and what would stop it, and writes nothing: no claim, no network, no certificate. " +
			"The answer is 200 whether or not anything refuses. items are, in order, self_hostname (the name, judged by the rules every claim is judged by), self_certificate, " +
			"self_network (whether TierPlan is, or will be, on the tierplan-self network, with the next step as snippet for a swarm service that is not), self_origin (the origin TierPlan will also accept) " +
			"and, when TierPlan's own certificate renews over HTTP-01, self_carve_out. Each item has a state (ok, warning or refused), the server's sentence and, for a warning or a refusal, its stable reason. " +
			"Any refused item stops the switch-on. directAddress is the address that keeps reaching TierPlan whatever the route does, empty for the page's own origin. " +
			"Answered with Cache-Control no-store. " + selfRouteGateText + " A caller who holds ingress.manage but is not an instance admin is refused 403 " + problemInstanceAdminRequired +
			" and the refusal is audited as permission.denied.",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressSelfRouteInput) (*IngressSelfRoutePreviewOutput, error) {
		if _, err := gate(ctx, http.MethodPost, selfRoutePreviewPath, in.EnvID, in.UserAgent); err != nil {
			return nil, err
		}
		pv, err := d.Service.PreviewSelfRoute(ctx, in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "preview TierPlan's own route", err)
		}
		return &IngressSelfRoutePreviewOutput{CacheControl: "no-store", Body: ingressSelfRoutePreviewBody(pv)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "enableIngressSelfRoute",
		Method:      http.MethodPost,
		Path:        selfRoutePath,
		Summary:     "Switch TierPlan's own route on",
		Description: "Takes the environment's operation lock and answers 202 with a job's ID at once. The job (ingress_self_route_enable) runs the preview's checks again as its first item, self_check, " +
			"and a refusal ends it there with the refusing item's reason as the code (for example host_taken, vip_ingress_off, self_route_already_on or self_route_not_here) before anything is written. " +
			"Otherwise self_claim stores the hostname for TierPlan with its audit entry (ingress.self_route_enabled), and self_network waits for the ingress to report the tierplan-self network. " +
			"The job ends done once the claim is stored, even when the route is not served yet. Follow it at GET /api/env/{envId}/ingress/jobs/{jobId} or its stream. " +
			"409 " + problemOperationLocked + " while another operation holds the environment, naming it. " + selfRouteGateText +
			" A caller who holds ingress.manage but is not an instance admin is refused 403 " + problemInstanceAdminRequired + " and the refusal is audited as permission.denied.",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressSelfRouteInput) (*IngressSelfRouteStartOutput, error) {
		p, err := gate(ctx, http.MethodPost, selfRoutePath, in.EnvID, in.UserAgent)
		if err != nil {
			return nil, err
		}
		id, err := d.Service.EnableSelfRoute(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "switch TierPlan's own route on", err)
		}
		return &IngressSelfRouteStartOutput{CacheControl: "no-store", Body: IngressSelfRouteStartBody{JobID: id}}, nil
	})
}
