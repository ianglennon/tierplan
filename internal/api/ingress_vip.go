// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The virtual IP write routes (Phase 26, VIP-01, VIP-05, D-16): an Admin previews what switching the virtual IP on or changing
// it would do, switches it on, changes it and switches it off, each as a job, and every refusal is a stable code with the
// server's sentence. None of them carries the VRRP key (PROH-26-03), and none sets the privileged-services opt-in (PROH-26-01):
// that stays the agent's own setting.

// IngressVIPRequest is the body of the preview, the enable and the change.
type IngressVIPRequest struct {
	Address  string `json:"address" maxLength:"18" doc:"The virtual IP with the prefix length of its network, for example 192.168.1.50/24. The prefix is required and only IPv4 is accepted"`
	RouterID *int   `json:"routerId,omitempty" minimum:"1" maximum:"255" doc:"The VRRP router ID, 1 to 255. Leave it out to let TierPlan derive one from the swarm's cluster ID. A change takes the whole configuration: leaving it out derives it again"`
}

// IngressVIPInput is the path and the body of the preview, the enable and the change.
type IngressVIPInput struct {
	EnvironmentPathInput
	Body IngressVIPRequest
}

// IngressVIPPreviewManagerBody is a manager's privileged-services opt-in as the preview shows it.
type IngressVIPPreviewManagerBody struct {
	IngressVIPManagerBody
	Note string `json:"note,omitempty" doc:"A sentence about this manager to show with it; set for a manager reached through TierPlan's own Docker socket, which counts as allowing"`
}

// IngressVIPPreviewBody is the answer to the preview of an enable or a change of the virtual IP. It holds no key: none exists
// until the job makes it.
type IngressVIPPreviewBody struct {
	Findings        []IngressFindingBody           `json:"findings" doc:"Everything the checks found, errors and warnings, in the order they ran. A finding of severity error refuses the enable or the change"`
	Address         string                         `json:"address" doc:"The address as parsed, with its prefix length; empty when it was refused as typed"`
	RouterID        int                            `json:"routerId" doc:"The router ID the job would use; 0 when it was refused"`
	DerivedRouterID int                            `json:"derivedRouterId" doc:"The router ID the swarm's cluster ID gives, to show beside an override"`
	RouterDerived   bool                           `json:"routerDerived" doc:"True when the router ID is the derived one, false when the request chose it"`
	Managers        []IngressVIPPreviewManagerBody `json:"managers" doc:"The privileged-services opt-in of every manager; empty when it could not be read"`
	Warning         string                         `json:"warning" doc:"The fixed privilege warning, always present and not only on a warning. Show it as sent"`
	Change          bool                           `json:"change" doc:"True when the environment has a virtual IP already, so the preview is of a change; false for an enable"`
	Items           []string                       `json:"items" doc:"The steps the job would run, as their labels"`
	Current         string                         `json:"current,omitempty" doc:"For a change, the address the environment has now, with its prefix length"`
	CurrentRouterID int                            `json:"currentRouterId,omitempty" doc:"For a change, the router ID the environment has now"`
	Strategy        string                         `json:"strategy,omitempty" enum:"roll,replace,none" doc:"For a change that is not refused, how it is made: roll moves one node at a time, replace removes the service and creates it again, none touches no node"`
	Notice          string                         `json:"notice,omitempty" doc:"For a change that is not refused, what it does to the address. Show it as sent"`
}

// IngressVIPPreviewOutput wraps IngressVIPPreviewBody for huma.
type IngressVIPPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressVIPPreviewBody
}

// IngressVIPStartBody is the answer to the enable, the change and the switch-off: the job that does it.
type IngressVIPStartBody struct {
	JobID string `json:"jobId" doc:"The job: read it at GET /api/env/{envId}/ingress/jobs/{jobId} or follow its stream"`
}

// IngressVIPStartOutput wraps IngressVIPStartBody for huma.
type IngressVIPStartOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressVIPStartBody
}

// IngressVIPDisablePreviewBody is the answer to the preview of a switch-off of the virtual IP. It holds no key.
type IngressVIPDisablePreviewBody struct {
	Address string   `json:"address" doc:"The environment's virtual IP with the prefix length of its network"`
	Service string   `json:"service" doc:"The keepalived service's name; empty when the swarm was read and does not hold one of TierPlan's"`
	Tasks   int      `json:"tasks" doc:"How many keepalived tasks run now; 0 when none do or the swarm could not be read"`
	Secrets []string `json:"secrets" doc:"The names of the swarm secrets that hold the virtual IP's key, never the key; when the swarm could not be read, the name the stored key's generation gives"`
	Items   []string `json:"items" doc:"The steps the switch-off job runs, as their labels"`
}

// IngressVIPDisablePreviewOutput wraps IngressVIPDisablePreviewBody for huma.
type IngressVIPDisablePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressVIPDisablePreviewBody
}

// ingressVIPPreviewBody maps the deployer's preview to the body.
func ingressVIPPreviewBody(pv ingressdeploy.VIPPreview) IngressVIPPreviewBody {
	body := IngressVIPPreviewBody{
		Findings: ingressFindings(pv.Findings), RouterID: pv.RouterID, DerivedRouterID: pv.DerivedRouterID, RouterDerived: pv.RouterDerived,
		Managers: make([]IngressVIPPreviewManagerBody, 0, len(pv.Managers)), Warning: pv.Warning, Items: append([]string{}, pv.Items...),
	}
	if pv.Address.IsValid() {
		body.Address = pv.Address.String()
	}
	for _, m := range pv.Managers {
		mb := IngressVIPPreviewManagerBody{IngressVIPManagerBody: IngressVIPManagerBody{
			NodeID: m.NodeID, Hostname: m.Hostname, State: m.State, LocalSocket: m.LocalSocket,
		}}
		if m.LocalSocket {
			name := m.Hostname
			if name == "" {
				name = m.NodeID
			}
			mb.Note = ingressdeploy.VIPLocalSocketManagerText(name)
		}
		body.Managers = append(body.Managers, mb)
	}
	return body
}

// vipRefusalProblem is the problem for a request the checks refused. A value that is not an address, or not a router ID, is
// 422 with that finding's code as the type: the request is malformed. Any other refusal is 409 with the code of the first
// refusing finding. Either way the detail is the sentences of the refusing findings, which the server words, and each finding is
// listed with its code as the location, its sentence as the message and its items (the managers and where each stands, the nodes
// outside the network) as the value. Nothing in it is built from an engine's answer.
func vipRefusalProblem(fs []ingressdeploy.Finding) error {
	var (
		details []*huma.ErrorDetail
		msgs    []string
		first   *ingressdeploy.Finding
		invalid *ingressdeploy.Finding
	)
	for i := range fs {
		f := fs[i]
		if f.Severity != ingressdeploy.SeverityError {
			continue
		}
		if first == nil {
			first = &fs[i]
		}
		if invalid == nil && (f.Code == ingressdeploy.CodeVIPAddressInvalid || f.Code == ingressdeploy.CodeVIPRouterIDInvalid) {
			invalid = &fs[i]
		}
		items := f.Items
		if items == nil {
			items = []string{}
		}
		details = append(details, &huma.ErrorDetail{Location: f.Code, Message: f.Message, Value: items})
		msgs = append(msgs, f.Message)
	}
	status, typ := http.StatusConflict, ""
	switch {
	case invalid != nil:
		status, typ = http.StatusUnprocessableEntity, invalid.Code
	case first != nil:
		typ = first.Code
	}
	return &huma.ErrorModel{Type: typ, Title: http.StatusText(status), Status: status, Detail: strings.Join(msgs, " "), Errors: details}
}

// registerIngressVIPWrites registers the virtual IP's preview, enable, change, switch-off preview and switch-off. All of them need
// ingress.manage on the environment (its Admin), like the status and the move history beside them.
func registerIngressVIPWrites(r *httpx.Router, d IngressDeployDeps) {
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress is not available on this server.")
	}
	request := func(b IngressVIPRequest) ingressdeploy.VIPRequest {
		return ingressdeploy.VIPRequest{Address: strings.TrimSpace(b.Address), RouterID: b.RouterID}
	}
	fail := func(ctx context.Context, what string, err error) error {
		var pre *ingressdeploy.PreflightError
		var locked *ingressdeploy.LockedError
		switch {
		case errors.As(err, &pre):
			return vipRefusalProblem(pre.Findings)
		case errors.As(err, &locked):
			name := ""
			if env := httpx.EnvFrom(ctx); env != nil {
				name = env.Name
			}
			return operationLocked(name, locked.Holder)
		case errors.Is(err, ingressdeploy.ErrVIPNotEnabled):
			return httpx.Problem(http.StatusNotFound, problemVIPNotEnabled, vipNotEnabledText)
		case errors.Is(err, envs.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		d.Log.Error("virtual IP request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressVIP",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/vip/preview",
		Summary:     "Preview switching the virtual IP on or changing it",
		Description: "Runs every check of the virtual IP on the address and the router ID asked for and writes nothing: no key is made and no secret created. " +
			"On an environment with no virtual IP it previews switching it on; on one that has a virtual IP it previews changing it, and change is true, with current, currentRouterId, " +
			"the strategy of the change (roll, replace or none) and a notice that says what it does to the address. The answer is 200 whether or not the checks refuse: " +
			"each finding has a code, the server's sentence, the items it is about and a severity, where error refuses the job and warning does not. " +
			"The fixed privilege warning is always in warning, and managers lists each manager's privileged-services setting. " +
			"The checks are the job's own: an address with no prefix length is the finding vip_address_invalid, and the others are vip_swarm_only, vip_ingress_off, ingress_paused, vip_already_enabled, " +
			"vip_privileged_not_allowed (naming TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true, with each manager and its state), vip_ipv6_unsupported, vip_not_unicast, vip_is_network_or_broadcast, " +
			"vip_outside_node_subnet, vip_is_node_address, vip_in_use_by_environment, vip_overlaps_docker, vip_router_id_in_use and vip_name_taken, and, for a change of a virtual IP whose switch-off failed part way, vip_removal_pending, with the warnings vip_inside_pool and vip_unverified. " +
			"It never carries the VRRP key. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressVIPInput) (*IngressVIPPreviewOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		// An environment that has a virtual IP previews a change; one that has none answers ErrVIPNotEnabled to the change's
		// checks before anything is read of the swarm, and previews an enable.
		cp, err := d.Service.PreviewVIPChange(ctx, in.EnvID, request(in.Body))
		switch {
		case err == nil:
			body := ingressVIPPreviewBody(cp.VIPPreview)
			body.Change, body.Strategy, body.Notice = true, cp.Strategy, cp.Notice
			if cp.Current.IsValid() {
				body.Current = cp.Current.String()
			}
			body.CurrentRouterID = cp.CurrentRouterID
			return &IngressVIPPreviewOutput{CacheControl: "no-store", Body: body}, nil
		case !errors.Is(err, ingressdeploy.ErrVIPNotEnabled):
			return nil, fail(ctx, "preview the virtual IP change", err)
		}
		pv, err := d.Service.PreviewVIP(ctx, in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "preview the virtual IP", err)
		}
		return &IngressVIPPreviewOutput{CacheControl: "no-store", Body: ingressVIPPreviewBody(pv)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "enableIngressVIP",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/vip",
		Summary:     "Switch the virtual IP on for a swarm",
		Description: "Takes the environment's operation lock, runs every check of the preview again and answers 202 with a job's ID at once. The job makes the VRRP key, stores it as a swarm secret, " +
			"creates the global keepalived service and stores the address with its audit entry (ingress.vip_enabled). Follow it at GET /api/env/{envId}/ingress/jobs/{jobId} or its stream. " +
			"A refusal is 409 with the finding's code as the type and the server's sentence as the detail, and each refusing finding in errors: vip_swarm_only, vip_ingress_off, ingress_paused, vip_already_enabled, " +
			"vip_privileged_not_allowed (naming TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true, with each manager and its state in errors), vip_not_unicast, vip_is_network_or_broadcast, vip_outside_node_subnet, " +
			"vip_is_node_address, vip_in_use_by_environment, vip_overlaps_docker, vip_router_id_in_use, vip_ipv6_unsupported and vip_name_taken. An address that is not an address with its prefix length, from /8 to /32, is 422 vip_address_invalid. " +
			"409 " + problemOperationLocked + " while another operation holds the environment, naming it. Nothing is written when it is refused. TierPlan never sets the privileged-services setting itself: " +
			"every manager's agent must already allow it. The answer and the job never carry the VRRP key. Needs ingress.manage on the environment (its Admin).",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressVIPInput) (*IngressVIPStartOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		id, err := d.Service.EnableVIP(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "switch the virtual IP on", err)
		}
		return &IngressVIPStartOutput{CacheControl: "no-store", Body: IngressVIPStartBody{JobID: id}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "changeIngressVIP",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/ingress/vip",
		Summary:     "Change the address or the router ID of a swarm's virtual IP",
		Description: "The body is the whole configuration, as the preview takes it: an address, and a router ID or none to derive it from the cluster ID again. Takes the environment's operation lock, " +
			"runs every check of the preview again on the new request and answers 202 with a job's ID at once. The job updates the keepalived service and stores the new address with its audit entry " +
			"(ingress.vip_changed). The key and its secret are kept: a change never rotates them. An address change rolls one node at a time and a router ID change removes the service and creates it again, " +
			"which the preview says first. Follow the job at GET /api/env/{envId}/ingress/jobs/{jobId} or its stream. " +
			"404 " + problemVIPNotEnabled + " when the environment has no virtual IP. A refusal is 409 with the finding's code as the type and the server's sentence as the detail, as for the enable, " +
			"and an address that is not an address with its prefix length (from /8 to /32) is 422 vip_address_invalid; a virtual IP whose switch-off failed part way is 409 vip_removal_pending, " +
			"and switching it off again finishes the removal; 409 " + problemOperationLocked + " while another operation holds the environment, naming it. " +
			"Nothing is written when it is refused. Needs ingress.manage on the environment (its Admin).",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressVIPInput) (*IngressVIPStartOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		id, err := d.Service.ChangeVIP(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "change the virtual IP", err)
		}
		return &IngressVIPStartOutput{CacheControl: "no-store", Body: IngressVIPStartBody{JobID: id}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressVIPDisable",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/vip/disable/preview",
		Summary:     "Preview switching a swarm's virtual IP off",
		Description: "Says what switching the virtual IP off would remove: the keepalived service, with how many of its tasks run now, and the swarm secret that holds the key, by name and never by content. " +
			"It writes nothing and needs no manager check, since removing a service is not an escalation. items lists the steps of the job. " +
			"404 " + problemVIPNotEnabled + " when the environment has no virtual IP. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressVIPDisablePreviewOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		pv, err := d.Service.PreviewVIPDisable(ctx, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "preview the virtual IP switch-off", err)
		}
		body := IngressVIPDisablePreviewBody{
			Address: pv.Address.String(), Service: pv.Service, Tasks: pv.Tasks,
			Secrets: append([]string{}, pv.Secrets...), Items: append([]string{}, pv.Items...),
		}
		return &IngressVIPDisablePreviewOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "disableIngressVIP",
		Method:      http.MethodDelete,
		Path:        "/api/env/{envId}/ingress/vip",
		Summary:     "Switch a swarm's virtual IP off",
		Description: "Takes the environment's operation lock and answers 202 with a job's ID at once. The job removes the keepalived service, waits for every node to release the address, removes the key's swarm secret " +
			"and deletes the stored row with its audit entry (ingress.vip_disabled). A step that fails leaves the row switched off and names what is left in the swarm; switching off again finishes it. " +
			"Follow the job at GET /api/env/{envId}/ingress/jobs/{jobId} or its stream. 404 " + problemVIPNotEnabled + " when the environment has no virtual IP; " +
			"409 " + problemOperationLocked + " while another operation holds the environment, naming it. Nothing is written when it is refused. Needs ingress.manage on the environment (its Admin).",
		DefaultStatus: http.StatusAccepted,
		Tags:          []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressVIPStartOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		id, err := d.Service.DisableVIP(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID)
		if err != nil {
			return nil, fail(ctx, "switch the virtual IP off", err)
		}
		return &IngressVIPStartOutput{CacheControl: "no-store", Body: IngressVIPStartBody{JobID: id}}, nil
	})
}
