// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	maxMapEntries     = 64
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
			"A network is made on one node, named by nodeId (the nodeId of a row of a list), which may be left out on an environment of one node (else 422 node_required). " +
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

// createNetwork routes the create to its node, sends it once and audits the attempt.
func createNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID string, in *NetworkCreateInput) (*NetworkCreateOutput, error) {
	body := in.Body
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	nodeID := body.NodeID
	if nodeID == "" {
		if len(agg.Nodes) != 1 {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
				"This environment has more than one node. Say which one to create the network on (nodeId).")
		}
		nodeID = agg.Nodes[0].ID
	}

	spec := dockerx.NetworkCreateSpec{
		Name: body.Name, Driver: body.Driver, Attachable: body.Attachable, Internal: body.Internal, IPv6: body.IPv6,
		Labels: body.Labels, Options: body.Options, Subnet: body.Subnet, Gateway: body.Gateway, IPRange: body.IPRange,
	}
	var id string
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		id, _, e = c.NetworkCreate(writeCtx, spec)
		return e
	})
	scope := dockerx.NetworkScope(body.Driver)
	if err != nil {
		code, problem := networkCreateProblem(d.Log, envID, body.Name, err)
		auditNetwork(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkCreated, body.Name, body.Name, nodeID, scope, spec, code)
		return nil, problem
	}
	auditNetwork(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkCreated, id, body.Name, nodeID, scope, spec, codeCreated)
	return &NetworkCreateOutput{CacheControl: "no-store", Body: NetworkCreateBody{ID: id, Name: body.Name}}, nil
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
		"name": name, "node": node, "driver": spec.Driver, "scope": scope, "code": code,
		"internal": strconv.FormatBool(spec.Internal), "attachable": strconv.FormatBool(spec.Attachable),
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
