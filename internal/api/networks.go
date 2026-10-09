// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

const (
	networkTargetType = "network"
	problemNameTaken  = "name_taken"
	codeCreated       = "created"

	problemSubnetInvalid         = "subnet_invalid"
	problemGatewayOutsideSubnet  = "gateway_outside_subnet"
	problemIPRangeOutsideSubnet  = "iprange_outside_subnet"
	problemAttachableOverlayOnly = "attachable_overlay_only"
	problemOverlayNeedsSwarm     = "overlay_needs_swarm"
)

// NetworksDeps are the dependencies of the network routes.
type NetworksDeps struct {
	// Aggregator reaches an engine: a write to one node goes through WriteOnNode, a
	// cluster-scoped one through WriteOnManager.
	Aggregator *envs.Aggregator
	Audit      *audit.Writer
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// NetworkCreateRequest is the body of POST /api/env/{envId}/networks.
type NetworkCreateRequest struct {
	Name       string            `json:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The network name"`
	Driver     string            `json:"driver" enum:"bridge,overlay,macvlan,ipvlan" doc:"The network driver. Ingress, config-only and config-from networks cannot be created"`
	NodeID     string            `json:"nodeId,omitempty" maxLength:"128" doc:"The node to create a local-scope network on: the nodeId of a row of a list. Left out on an environment of one node"`
	Attachable bool              `json:"attachable,omitempty" doc:"Let standalone containers join it. Only for an overlay network on a swarm"`
	Internal   bool              `json:"internal,omitempty" doc:"No access to outside networks"`
	IPv6       bool              `json:"ipv6,omitempty" doc:"Enable IPv6"`
	Labels     map[string]string `json:"labels,omitempty" maxProperties:"64" doc:"Labels, at most 64"`
	Options    map[string]string `json:"options,omitempty" maxProperties:"64" doc:"Driver options, at most 64"`
	Subnet     string            `json:"subnet,omitempty" maxLength:"64" doc:"The subnet in CIDR form, for example 10.20.0.0/24. Left out, the engine picks one"`
	Gateway    string            `json:"gateway,omitempty" maxLength:"64" doc:"The gateway address, inside the subnet"`
	IPRange    string            `json:"ipRange,omitempty" maxLength:"64" doc:"The range containers are given addresses from, in CIDR form, inside the subnet"`
}

// NetworkCreateInput is the path and body of the create route.
type NetworkCreateInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      NetworkCreateRequest
}

// NetworkCreateBody is the answer to a create.
type NetworkCreateBody struct {
	ID   string `json:"id" doc:"The ID of the network the engine made"`
	Name string `json:"name" doc:"The network name"`
}

// NetworkCreateOutput wraps NetworkCreateBody for huma.
type NetworkCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NetworkCreateBody
}

// RegisterNetworks registers the network routes. A write is routed through the
// aggregator's write paths and never retried (P2 D-42), and every attempt is audited.
func RegisterNetworks(r *httpx.Router, d NetworksDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "createNetwork", Method: http.MethodPost, Path: "/api/env/{envId}/networks",
		Summary: "Create a network",
		Description: "Creates a bridge, overlay, macvlan or ipvlan network. " +
			"An overlay network on a swarm belongs to the cluster: it is made once, through a manager, and nodeId is ignored. " +
			"Every other network is local to one node, named by nodeId (the nodeId of a row of a list); nodeId may be left out only on an environment of one node that is not a swarm (else 422 node_required). " +
			"An overlay network off a swarm is 422 overlay_needs_swarm, and attachable on anything but an overlay network on a swarm is 422 attachable_overlay_only. " +
			"subnet, gateway and ipRange are checked as CIDR before anything is sent: 422 subnet_invalid, gateway_outside_subnet or iprange_outside_subnet. " +
			"409 name_taken when the engine has the name already, 409 host_read_only on a read-only host, 503 node_unavailable when the node is not a connected node of the environment. " +
			"Needs networks.manage (Admin) on the environment. Every attempt is audited as network.created, with result failure and the code when the engine or the host refused it.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated,
	}, rbac.Requirement{Perm: rbac.PermNetworksManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkCreateInput) (*NetworkCreateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return createNetwork(ctx, d, p, env.ID, in)
		})
}

// createNetwork checks the request, routes the create to where the network belongs,
// sends it once and audits the attempt.
//
// An overlay network on a swarm is cluster scoped: it is made once, through the manager
// the aggregator chooses. Every other network is local to one engine and is made on the
// node the request names, or on the environment's only node when it is not a swarm.
func createNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID string, in *NetworkCreateInput) (*NetworkCreateOutput, error) {
	body := in.Body
	if err := validateAddressing(body.Subnet, body.Gateway, body.IPRange); err != nil {
		return nil, err
	}
	if body.Attachable && body.Driver != dockerx.DriverOverlay {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemAttachableOverlayOnly,
			"Only an overlay network on a swarm can be attachable.")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	swarm := agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker
	if body.Driver == dockerx.DriverOverlay && !swarm {
		code := problemOverlayNeedsSwarm
		if body.Attachable {
			code = problemAttachableOverlayOnly
		}
		return nil, httpx.Problem(http.StatusUnprocessableEntity, code,
			"An overlay network needs a swarm. This environment is not one.")
	}

	spec := dockerx.NetworkCreateSpec{
		Name: body.Name, Driver: body.Driver, Attachable: body.Attachable, Internal: body.Internal, IPv6: body.IPv6,
		Labels: body.Labels, Options: body.Options, Subnet: body.Subnet, Gateway: body.Gateway, IPRange: body.IPRange,
	}
	scope := dockerx.NetworkScope(body.Driver)
	var id, nodeID string
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	create := func(c *dockerx.Client) error {
		var e error
		id, _, e = c.NetworkCreate(writeCtx, spec)
		return e
	}
	if swarm && scope == dockerx.ScopeSwarm {
		spec.Scope = dockerx.ScopeSwarm
		err = d.Aggregator.WriteOnManager(writeCtx, envID, create)
	} else {
		nodeID = body.NodeID
		if nodeID == "" {
			if swarm || len(agg.Nodes) != 1 {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
					"This network is local to one node. Say which one to create it on (nodeId).")
			}
			nodeID = agg.Nodes[0].ID
		}
		err = d.Aggregator.WriteOnNode(writeCtx, envID, nodeID, create)
	}
	if err != nil {
		code, problem := networkCreateProblem(d.Log, envID, body.Name, err)
		auditNetwork(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkCreated, body.Name, body.Name, nodeID, scope, spec, code)
		return nil, problem
	}
	auditNetwork(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkCreated, id, body.Name, nodeID, scope, spec, codeCreated)
	return &NetworkCreateOutput{CacheControl: "no-store", Body: NetworkCreateBody{ID: id, Name: body.Name}}, nil
}

// validateAddressing checks the subnet, gateway and IP range of a create with net/netip,
// before anything is sent: the subnet must be a prefix, the gateway an address inside it
// and the range a prefix inside it. A gateway or range with no subnet has nothing to be
// inside. The problem types are the ones the SPA words (UI-SPEC "Field errors").
func validateAddressing(subnet, gateway, ipRange string) error {
	if subnet == "" {
		if gateway != "" || ipRange != "" {
			return httpx.Problem(http.StatusUnprocessableEntity, problemSubnetInvalid,
				"A gateway or an IP range needs a subnet to be inside of.")
		}
		return nil
	}
	sp, err := netip.ParsePrefix(subnet)
	if err != nil {
		return httpx.Problem(http.StatusUnprocessableEntity, problemSubnetInvalid,
			"The subnet must be in CIDR form, like 10.20.0.0/24.")
	}
	sp = sp.Masked()
	if gateway != "" {
		if a, err := netip.ParseAddr(gateway); err != nil || !sp.Contains(a) {
			return httpx.Problem(http.StatusUnprocessableEntity, problemGatewayOutsideSubnet,
				"The gateway must be an address inside the subnet.")
		}
	}
	if ipRange != "" {
		rp, err := netip.ParsePrefix(ipRange)
		if err != nil || rp.Bits() < sp.Bits() || !sp.Contains(rp.Addr()) {
			return httpx.Problem(http.StatusUnprocessableEntity, problemIPRangeOutsideSubnet,
				"The IP range must be a prefix inside the subnet.")
		}
	}
	return nil
}

// networkCreateProblem turns the error of a create into the audit code and the problem
// the SPA understands.
func networkCreateProblem(log *slog.Logger, envID, name string, err error) (string, error) {
	switch {
	case dockerx.IsConflict(err):
		return problemNameTaken, httpx.Problem(http.StatusConflict, problemNameTaken, "A network named "+name+" already exists.")
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly, httpx.Problem(http.StatusConflict, codeHostReadOnly,
			"This host is read-only. Its owner turned changes off on the host itself, so Bayplan can only read from it.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return problemNodeUnavailable, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
	}
	var denied *envs.AgentPolicyDeniedError
	if errors.As(envs.ClassifyDockerError(err), &denied) {
		return problemAgentPolicyDenied, snapshotProblem(log, envID, err)
	}
	return codeActionFailed, snapshotProblem(log, envID, err)
}

// auditNetwork writes the row of one network attempt. A row that cannot be written is
// logged: the network has been made or refused, and the caller must still be told which.
func auditNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent, action, targetID, name, node, scope string, spec dockerx.NetworkCreateSpec, code string) {
	result, reason := audit.ResultSuccess, ""
	if code != codeCreated {
		result, reason = audit.ResultFailure, code
	}
	details := map[string]string{
		"name": name, "driver": spec.Driver, "scope": scope, "code": code,
		"internal": strconv.FormatBool(spec.Internal), "attachable": strconv.FormatBool(spec.Attachable),
	}
	// A cluster network is made by a manager, not on a node of the caller's choosing.
	if node != "" {
		details["node"] = node
	}
	if spec.Subnet != "" {
		details["subnet"] = spec.Subnet
	}
	if spec.Gateway != "" {
		details["gateway"] = spec.Gateway
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: networkTargetType, TargetID: targetID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a network action", "action", action, "network", name, "error", err)
	}
}
