// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
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

	problemNetworkBuiltin  = "network_builtin"
	problemNetworkInUse    = "network_in_use"
	problemNetworkNotFound = "network_not_found"
	problemNodeUnreachable = codeNodeUnreachable
	codeRemoved            = "removed"

	problemNodeMismatch             = "node_mismatch"
	problemNetworkNotAttachable     = "network_not_attachable"
	problemAddressInvalid           = "address_invalid"
	problemAlreadyConnected         = "already_connected"
	problemNetworkUnavailableOnNode = "network_unavailable_on_node"
	codeConnected                   = "connected"
	codeDisconnected                = "disconnected"
	problemNotConnected             = "not_connected"
)

// NetworksDeps are the dependencies of the network routes.
type NetworksDeps struct {
	// Aggregator reaches an engine: a write to one node goes through WriteOnNode, a
	// cluster-scoped one through WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters reads the services of a swarm live before a delete.
	Clusters *envs.Clusters
	// AddressSpaces reads how full the address pools are, for the gate of a create that
	// would use the last blocks of a pool. Nil means the feature is off: a create is not
	// checked against a pool.
	AddressSpaces *envs.AddressSpaces
	Audit         *audit.Writer
	// SelfContainerID is Coaming's own container ID; empty when it cannot tell. A
	// connect or disconnect of that container, or of an agent's, needs its name typed.
	SelfContainerID string
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

// NetworkDeleteInput is the path and query of the delete route.
type NetworkDeleteInput struct {
	EnvironmentPathInput
	NetworkID string `path:"networkId" maxLength:"128" doc:"The network ID: the id of a row of the networks list"`
	NodeID    string `query:"nodeId" maxLength:"128" doc:"The node a local-scope network is on: the nodeId of its row. Ignored for a swarm-scoped network, which belongs to the cluster"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// NetworkConnectRequest is the body of POST /api/env/{envId}/networks/{networkId}/connect.
type NetworkConnectRequest struct {
	NodeID      string `json:"nodeId,omitempty" maxLength:"128" doc:"The node the container is on: the nodeId of its attachment on the network page or of its row in the containers list. Left out only on an environment of one node"`
	ContainerID string `json:"containerId" minLength:"1" maxLength:"128" doc:"The container ID"`
	Alias       string `json:"alias,omitempty" maxLength:"253" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]*$" doc:"Another name other containers on the network can use to reach this one"`
	IPv4        string `json:"ipv4,omitempty" maxLength:"45" doc:"An IPv4 address to give the container on the network, inside one of its subnets"`
	IPv6        string `json:"ipv6,omitempty" maxLength:"45" doc:"An IPv6 address to give the container on the network, inside one of its subnets"`
	Confirm     string `json:"confirm,omitempty" maxLength:"256" doc:"The container's name, typed. Needed to connect Coaming's own server or an agent container (else 422 confirmation_required, and nothing is sent)"`
}

// NetworkConnectInput is the path and body of the connect route.
type NetworkConnectInput struct {
	EnvironmentPathInput
	NetworkID string `path:"networkId" maxLength:"128" doc:"The network ID: the id of a row of the networks list"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      NetworkConnectRequest
}

// NetworkDisconnectRequest is the body of POST /api/env/{envId}/networks/{networkId}/disconnect.
// There is no force: a disconnect the engine refuses is refused.
type NetworkDisconnectRequest struct {
	NodeID      string `json:"nodeId,omitempty" maxLength:"128" doc:"The node the container is on: the nodeId of its attachment on the network page or of its row in the containers list. Left out only on an environment of one node"`
	ContainerID string `json:"containerId" minLength:"1" maxLength:"128" doc:"The container ID"`
	Confirm     string `json:"confirm,omitempty" maxLength:"256" doc:"The container's name, typed. Needed to disconnect Coaming's own server or an agent container (else 422 confirmation_required, and nothing is sent)"`
}

// NetworkDisconnectInput is the path and body of the disconnect route.
type NetworkDisconnectInput struct {
	EnvironmentPathInput
	NetworkID string `path:"networkId" maxLength:"128" doc:"The network ID: the id of a row of the networks list"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      NetworkDisconnectRequest
}

// NetworkDetailInput is the path and query of the network detail and inspect routes.
type NetworkDetailInput struct {
	EnvironmentPathInput
	NetworkID string `path:"networkId" maxLength:"128" doc:"The network ID: the id of a row of the networks list"`
	NodeID    string `query:"nodeId" maxLength:"128" doc:"The node a local-scope network is on: the nodeId of its row. Ignored for a swarm-scoped network, which belongs to the cluster"`
}

// NetworkIPAMBody is one address pool of a network.
type NetworkIPAMBody struct {
	Subnet  string `json:"subnet" doc:"The subnet in CIDR notation"`
	Gateway string `json:"gateway" doc:"The gateway address; empty when the network sets none"`
	IPRange string `json:"ipRange" doc:"The range containers are given addresses from, in CIDR notation; empty when the network sets none"`
}

// NetworkAttachmentBody is one thing attached to a network: a container on a node, or a
// swarm service whose spec mentions the network.
type NetworkAttachmentBody struct {
	Kind    string   `json:"kind" enum:"container,service" doc:"container, or service for a swarm service whose spec mentions the network"`
	ID      string   `json:"id" doc:"The container ID; for a service, its ID when the cluster read has it, else empty"`
	Name    string   `json:"name" doc:"The container or service name"`
	Node    string   `json:"node" doc:"The display name of the node a container is on; empty for a service"`
	NodeID  string   `json:"nodeId" doc:"The ID of the node a container is on: what a write is routed by; empty for a service"`
	State   string   `json:"state" doc:"The container state; empty for a service"`
	IPv4    string   `json:"ipv4" doc:"The container's IPv4 address on this network; empty when it has none"`
	IPv6    string   `json:"ipv6" doc:"The container's IPv6 address on this network; empty when it has none"`
	MAC     string   `json:"mac" doc:"The container's MAC address on this network; empty when it has none"`
	Aliases []string `json:"aliases" nullable:"true" doc:"The DNS names the container answers to on this network, from an inspect of the container. Null for a service, for a container whose inspect failed, and for a container past the 50th (see aliasesTruncated)"`
	Tasks   int      `json:"tasks" doc:"For a service, how many of its tasks have an endpoint on the network, as the manager reports it; 0 for a container and when not known"`
}

// NetworkDetailBody is one network as its detail page shows it.
type NetworkDetailBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Scope      string `json:"scope" doc:"local, or swarm for a cluster-wide network"`
	Node       string `json:"node" doc:"The node the network is local to, or Cluster for a swarm-wide network"`
	NodeID     string `json:"nodeId" doc:"The ID of the node the network was read from; for a swarm-wide network, the manager in use"`
	Created    string `json:"created,omitempty" format:"date-time"`
	Internal   bool   `json:"internal" doc:"No access to outside networks"`
	Attachable bool   `json:"attachable" doc:"Standalone containers can join it"`
	IPv6       bool   `json:"ipv6" doc:"IPv6 is enabled"`
	BuiltIn    bool   `json:"builtIn" doc:"True for bridge, host, none, ingress, docker_gwbridge and any ingress network: the engine's own, which can never be deleted"`

	Owner     string            `json:"owner" doc:"The stack (com.docker.stack.namespace) or compose project (com.docker.compose.project) label value; empty when the network has neither"`
	OwnerKind string            `json:"ownerKind" enum:"stack,project," doc:"stack, project, or empty when the network has no owner"`
	Labels    map[string]string `json:"labels" doc:"Every label of the network"`
	Options   map[string]string `json:"options" doc:"The driver options the network was created with"`
	IPAM      []NetworkIPAMBody `json:"ipam" doc:"The address pools, in the engine's order; empty when the network has none"`

	Attachments        []NetworkAttachmentBody `json:"attachments" doc:"Every container on every reporting node that is attached to the network (a local network: its own node's) and, on a swarm, every service whose spec mentions it. Sorted by name, then node"`
	AttachedContainers int                     `json:"attachedContainers" doc:"How many attachments are containers"`
	AttachedServices   int                     `json:"attachedServices" doc:"How many attachments are services"`
	AliasesTruncated   bool                    `json:"aliasesTruncated" doc:"True when more than 50 containers are attached, so only the first 50 (in the order above) were inspected for their aliases and the rest have aliases null"`
	NotReporting       []string                `json:"notReporting" doc:"Nodes that should have answered and did not, so the attachments may be incomplete"`
}

// NetworkDetailOutput wraps NetworkDetailBody for huma.
type NetworkDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NetworkDetailBody
}

// NetworkInspectBody is the raw inspect document of one network.
type NetworkInspectBody struct {
	Inspect map[string]any `json:"inspect" doc:"The engine's inspect document of the network as it is; verbose, with the services, for a swarm-scoped network read on a manager"`
}

// NetworkInspectOutput wraps NetworkInspectBody for huma.
type NetworkInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NetworkInspectBody
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

	httpx.Register(r, huma.Operation{
		OperationID: "getNetwork", Method: http.MethodGet, Path: "/api/env/{envId}/networks/{networkId}",
		Summary: "One network: its configuration and what is attached to it",
		Description: "Everything the network detail page shows: driver, scope, addressing, flags, owner, options, labels, and every attachment. " +
			"Attachments are the containers, running or not, on every node that answered (a local network: its own node, named by nodeId), each with its node, addresses and aliases, " +
			"and on a swarm every service whose spec mentions the network, even with no running task. Sorted by name, then node. " +
			"Aliases come from an inspect of the attached containers, at most 50 of them (4 at once per node); the rest have aliases null and aliasesTruncated is true. " +
			"A local-scope network on a swarm is named by nodeId (422 node_required when two nodes have one with that ID). " +
			"404 network_not_found when the network is not there; 409 node_unreachable when it is missing and a node did not answer; 503 node_unavailable when its node is not connected. " +
			"Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkDetailInput) (*NetworkDetailOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getNetwork(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &NetworkDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectNetwork", Method: http.MethodGet, Path: "/api/env/{envId}/networks/{networkId}/inspect",
		Summary: "One network's raw inspect document",
		Description: "The engine's inspect document of the network as it is, for the Inspect tab: verbose for a swarm-scoped network read on a manager, so the services are in it. " +
			"The network is named as the detail route names it, with the same rules and problems (422 node_required, 404 network_not_found, 409 node_unreachable, 503 node_unavailable). " +
			"Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkDetailInput) (*NetworkInspectOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			_, _, _, raw, err := d.readNetwork(ctx, env.ID, in)
			if err != nil {
				return nil, err
			}
			doc, err := decodeInspect(raw)
			if err != nil {
				d.Log.Error("reading a network inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &NetworkInspectOutput{CacheControl: "no-store", Body: NetworkInspectBody{Inspect: doc}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "connectContainer", Method: http.MethodPost, Path: "/api/env/{envId}/networks/{networkId}/connect",
		Summary: "Connect a container to a network",
		Description: "Connects one container to one network, once and never retried, with an optional alias and fixed addresses. " +
			"The call goes to the node the container is on, named by nodeId. " +
			"A local-scope network takes only containers of its own node (422 node_mismatch); a swarm overlay must be attachable (422 network_not_attachable); " +
			"an address that is not an IPv4 or IPv6 address, or that the network does not accept, is 422 address_invalid; a container already on the network is 409 already_connected. " +
			"A worker that has not learned an overlay network yet answers 409 network_unavailable_on_node with the engine's own words. " +
			"Coaming's own server container and any agent container are protected, as they are for stop and remove: send confirm, the container's name, or the request is 422 confirmation_required and nothing is sent; such a connect is audited before the call (stage initiated) and after it (stage completed). " +
			"404 network_not_found or container_not_found, 409 host_read_only on a read-only host, 503 node_unavailable when the node is not connected. " +
			"Needs networks.connect (Operator) on the environment. Every attempt on a network and container that exist is audited as network.connected, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermNetworksConnect, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkConnectInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, connectContainer(ctx, d, p, env.ID, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "disconnectContainer", Method: http.MethodPost, Path: "/api/env/{envId}/networks/{networkId}/disconnect",
		Summary: "Disconnect a container from a network",
		Description: "Disconnects one container from one network, once, never retried and never forced. " +
			"The call goes to the node the container is on, named by nodeId. " +
			"A container that is not on the network is 409 not_connected; 404 network_not_found or container_not_found, " +
			"Coaming's own server container and any agent container are protected, as they are for stop and remove: send confirm, the container's name, or the request is 422 confirmation_required and nothing is sent. Such a disconnect is audited before the call (stage initiated) and after it (stage completed), and a connection that drops during the call, which is what cutting Coaming off its own network does, is answered 204 and audited as stage connection_lost with result success. " +
			"409 host_read_only on a read-only host, 503 node_unavailable when the node is not connected. " +
			"Needs networks.connect (Operator) on the environment. Every attempt on a network and container that exist is audited as network.disconnected, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermNetworksConnect, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkDisconnectInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, disconnectContainer(ctx, d, p, env.ID, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteNetwork", Method: http.MethodDelete, Path: "/api/env/{envId}/networks/{networkId}",
		Summary: "Delete a network",
		Description: "Deletes one network, once and never retried. " +
			"404 network_not_found when the network is not there (already gone). " +
			"409 network_builtin for bridge, host, none, ingress, docker_gwbridge and any network with the ingress flag. " +
			"A local-scope network on a swarm is named by nodeId, the nodeId of its row (422 node_required when two nodes have one with that ID). " +
			"Needs networks.manage (Admin) on the environment. Every attempt on a network that exists is audited as network.removed, " +
			"with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermNetworksManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *NetworkDeleteInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, deleteNetwork(ctx, d, p, env.ID, in)
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
	if dockerx.IsConflict(err) {
		return problemNameTaken, httpx.Problem(http.StatusConflict, problemNameTaken, "A network named "+name+" already exists.")
	}
	return networkWriteProblem(log, envID, err)
}

// networkWriteProblem is the part of a network write's failure that every write shares:
// a read-only host, a node that is not connected, an agent's policy refusal, and
// anything else as a failed action.
func networkWriteProblem(log *slog.Logger, envID string, err error) (string, error) {
	switch {
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly, httpx.Problem(http.StatusConflict, codeHostReadOnly,
			"This host is read-only. Its owner turned changes off on the host itself, so Coaming can only read from it.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return problemNodeUnavailable, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Coaming.")
	}
	var denied *envs.AgentPolicyDeniedError
	if errors.As(envs.ClassifyDockerError(err), &denied) {
		return problemAgentPolicyDenied, snapshotProblem(log, envID, err)
	}
	return codeActionFailed, snapshotProblem(log, envID, err)
}

// auditNetwork writes the row of one network create attempt. A row that cannot be written
// is logged: the network has been made or refused, and the caller must still be told which.
func auditNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent, action, targetID, name, node, scope string, spec dockerx.NetworkCreateSpec, code string) {
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
	recordNetwork(ctx, d, p, envID, userAgent, action, targetID, name, code, details)
}

// recordNetwork writes one audit row of a network action: a success when code is
// codeCreated or codeRemoved, else a failure whose reason is the code.
func recordNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent, action, targetID, name, code string, details map[string]string) {
	result, reason := audit.ResultSuccess, ""
	if code != codeCreated && code != codeRemoved {
		result, reason = audit.ResultFailure, code
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: networkTargetType, TargetID: targetID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a network action", "action", action, "network", name, "error", err)
	}
}

// errNoSuchNetwork is findNetwork's answer when no listed network has the ID.
var errNoSuchNetwork = errors.New("api: no such network")

// findNetwork is the row of the network networkID in an aggregate. A swarm-scoped network
// is listed once, so it is found by its ID. A local one is on one node, named by nodeID
// when given; with no nodeID and the same ID on two nodes the caller must say which.
func findNetwork(agg envs.Aggregate, networkID, nodeID string) (envs.NetworkRow, error) {
	var found []envs.NetworkRow
	for _, n := range agg.Networks {
		if n.ID != networkID {
			continue
		}
		if n.Scope != dockerx.ScopeSwarm && nodeID != "" && n.NodeID != nodeID {
			continue
		}
		found = append(found, n)
	}
	switch len(found) {
	case 0:
		return envs.NetworkRow{}, errNoSuchNetwork
	case 1:
		return found[0], nil
	}
	return envs.NetworkRow{}, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
		"More than one node has a network with this ID. Say which one (nodeId).")
}

// deleteNetwork removes one network. The built-in networks are refused before anything is
// sent. The removal goes where the network belongs: a swarm-scoped one through a manager,
// a local one to its own node, once and never retried (P2 D-42). Every attempt on a
// network that exists is audited.
func deleteNetwork(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID string, in *NetworkDeleteInput) error {
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return snapshotProblem(d.Log, envID, err)
	}
	row, err := findNetwork(agg, in.NetworkID, in.NodeID)
	if err != nil {
		// A node that did not answer may be the one that holds it: it is not gone then,
		// it is out of reach.
		if !errors.Is(err, errNoSuchNetwork) {
			return err
		}
		if len(agg.NotReporting) > 0 {
			return httpx.Problem(http.StatusConflict, problemNodeUnreachable,
				"Can't find this network: "+joinMore(agg.NotReporting, 3)+" didn't answer. Try again when it's back.")
		}
		return httpx.Problem(http.StatusNotFound, problemNetworkNotFound, "This network does not exist. It may have been removed.")
	}
	node := ""
	if row.Scope != dockerx.ScopeSwarm {
		node = row.NodeID
	}
	refuse := func(status int, code, detail string) error {
		recordRemoved(ctx, d, p, envID, in.UserAgent, row, node, code)
		return httpx.Problem(status, code, detail)
	}
	if dockerx.IsBuiltinNetwork(row.Name, row.Ingress) {
		return refuse(http.StatusConflict, problemNetworkBuiltin, row.Name+" is built in and can't be deleted.")
	}
	code, status, detail, err := d.usedBy(ctx, envID, row)
	if err != nil {
		// The environment itself could not be read: say it as every read does.
		recordRemoved(ctx, d, p, envID, in.UserAgent, row, node, codeActionFailed)
		return snapshotProblem(d.Log, envID, err)
	}
	if code != "" {
		return refuse(status, code, detail)
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	remove := func(c *dockerx.Client) error { return c.NetworkRemove(writeCtx, row.ID) }
	if row.Scope == dockerx.ScopeSwarm {
		err = d.Aggregator.WriteOnManager(writeCtx, envID, remove)
	} else {
		err = d.Aggregator.WriteOnNode(writeCtx, envID, row.NodeID, remove)
	}
	if err != nil {
		code, problem := networkRemoveProblem(d.Log, envID, row.Name, err)
		recordRemoved(ctx, d, p, envID, in.UserAgent, row, node, code)
		return problem
	}
	recordRemoved(ctx, d, p, envID, in.UserAgent, row, node, codeRemoved)
	return nil
}

// networkRemoveProblem turns the error of a remove into the audit code and the problem the
// SPA understands. An engine that says the network is gone is a 404, and one that says
// something is attached, which can happen when a container joins between the check and
// the call, is network_in_use and never a generic failure.
func networkRemoveProblem(log *slog.Logger, envID, name string, err error) (string, error) {
	switch {
	case dockerx.IsNotFound(err):
		return problemNetworkNotFound, httpx.Problem(http.StatusNotFound, problemNetworkNotFound,
			name+" is already gone.")
	case dockerx.IsNetworkInUse(err):
		return problemNetworkInUse, httpx.Problem(http.StatusConflict, problemNetworkInUse,
			name+" is still in use. Disconnect or remove what uses it, then try again.")
	}
	return networkWriteProblem(log, envID, err)
}

// recordRemoved audits one removal attempt of a network that exists: name, driver, scope
// and, for a local network, the node.
func recordRemoved(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent string, row envs.NetworkRow, node, code string) {
	details := map[string]string{"name": row.Name, "driver": row.Driver, "scope": row.Scope, "code": code}
	if node != "" {
		details["node"] = node
	}
	recordNetwork(ctx, d, p, envID, userAgent, audit.ActionNetworkRemoved, row.ID, row.Name, code, details)
}

// networkOwner is the stack or compose project a network belongs to, from its labels: a
// stack wins when both are set. Both empty is no owner.
func networkOwner(labels map[string]string) (owner, kind string) {
	if v := labels[labelStack]; v != "" {
		return v, "stack"
	}
	if v := labels[labelProject]; v != "" {
		return v, "project"
	}
	return "", ""
}

// filterNetworks keeps the networks whose name, driver, any subnet or owner contains q,
// ignoring case. An empty q keeps them all.
func filterNetworks(rows []envs.NetworkRow, q string) []envs.NetworkRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	out := make([]envs.NetworkRow, 0, len(rows))
	for _, n := range rows {
		owner, _ := networkOwner(n.Labels)
		if has(n.Name) || has(n.Driver) || has(owner) || slices.ContainsFunc(n.Subnets, has) {
			out = append(out, n)
		}
	}
	return out
}

// networkUsage is what is attached to the networks of an environment: containers per
// node for a local network, per network over every node for a swarm-scoped one, and the
// services whose specs mention a network.
type networkUsage struct {
	local    map[[2]string]int
	wide     map[string]int
	services map[string]int
	// firstService is, per network ID, the name of the first service by name whose spec
	// names the network: the owner the subnet map shows for a network with no label.
	firstService map[string]string
}

// networkUsageOf counts the attachments in an aggregate. Services come from the cached
// cluster read of a swarm, whatever age it has: a delete reads them live and never
// trusts this.
func networkUsageOf(ctx context.Context, clusters *envs.Clusters, envID string, agg envs.Aggregate) networkUsage {
	u := networkUsage{local: map[[2]string]int{}, wide: map[string]int{}, services: map[string]int{}, firstService: map[string]string{}}
	for _, c := range agg.Containers {
		for _, cn := range c.Networks {
			u.local[[2]string{c.NodeID, cn.NetworkID}]++
			u.wide[cn.NetworkID]++
		}
	}
	if clusters != nil && agg.Target.Probe.Type == dockerx.TypeSwarmManager {
		// A failed read leaves what the last good one held.
		st, _ := clusters.State(ctx, envID)
		for _, ref := range st.ServiceRefs {
			for _, id := range ref.Networks {
				u.services[id]++
				if first, ok := u.firstService[id]; !ok || ref.Name < first {
					u.firstService[id] = ref.Name
				}
			}
		}
	}
	return u
}

// of is the containers and services attached to one row. A swarm-scoped network counts
// the containers of every node; a local one only its own node's, since another node's
// network with the same ID is a different network.
func (u networkUsage) of(n envs.NetworkRow) (containers, services int) {
	if n.Scope == dockerx.ScopeSwarm {
		return u.wide[n.ID], u.services[n.ID]
	}
	return u.local[[2]string{n.NodeID, n.ID}], 0
}

// maxNamedUsers is how many users a refusal names before it says how many more there are.
const maxNamedUsers = 5

// usedBy decides whether a network may be deleted now, from what is true now and not from
// the list the caller saw: envs.ResolveUsage reads every node's containers again and, on a
// swarm, the services (their current and previous specs and virtual IPs) and tasks from a
// manager. It returns an empty code when nothing uses the network. Otherwise it returns the
// problem code, its status and its words:
//
//   - node_unreachable when a node that could hold an attachment did not answer, or the
//     manager could not be asked for the services, so the picture is incomplete;
//   - network_not_found when the network is no longer there;
//   - network_in_use naming up to maxNamedUsers users.
//
// A swarm-scoped network can be attached on any node, so every expected node must answer;
// a local network only on its own node, which must answer. A service spec counts even
// when it has no running task (Pitfall 7).
func (d NetworksDeps) usedBy(ctx context.Context, envID string, row envs.NetworkRow) (code string, status int, detail string, err error) {
	usage, fresh, err := envs.ResolveUsage(ctx, d.Aggregator, d.Clusters, envID)
	if err != nil {
		return "", 0, "", err
	}
	swarmScoped := row.Scope == dockerx.ScopeSwarm
	switch {
	case swarmScoped && len(usage.Missing) > 0:
		return problemNodeUnreachable, http.StatusConflict,
			"Can't tell whether " + row.Name + " is in use: " + joinMore(usage.Missing, 3) + " didn't answer. Try again when it's back.", nil
	case !swarmScoped && !usage.Known(row):
		return problemNodeUnreachable, http.StatusConflict,
			"Can't tell whether " + row.Name + " is in use: " + row.Node + " didn't answer. Try again when it's back.", nil
	}
	if !slices.ContainsFunc(fresh.Networks, func(n envs.NetworkRow) bool {
		return n.ID == row.ID && (swarmScoped || n.NodeID == row.NodeID)
	}) {
		return problemNetworkNotFound, http.StatusNotFound, row.Name + " is already gone.", nil
	}
	if !usage.Known(row) {
		return problemNodeUnreachable, http.StatusConflict, "Can't tell whether " + row.Name + " is in use: no manager answered. Try again when one is back.", nil
	}
	users := usage.UsersOf(row)
	if len(users) == 0 {
		return "", 0, "", nil
	}
	return problemNetworkInUse, http.StatusConflict,
		row.Name + " is still in use by " + joinMore(users, maxNamedUsers) + ". Disconnect or remove what uses it, then try again.", nil
}

// joinMore names up to limit of items, then how many more there are.
func joinMore(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:limit], ", ") + " and " + strconv.Itoa(len(items)-limit) + " more"
}

// maxAliasInspects is how many attached containers are inspected for their aliases; the
// list names the rest without them (T-04-51).
const maxAliasInspects = 50

// readNetwork finds the network in the environment and reads it live from the engine it
// belongs to: a local network from its own node, a swarm-scoped one from the manager the
// environment's read used, verbose so the services on it are in the answer. It returns the
// typed detail and the raw document. The errors are already problems.
func (d NetworksDeps) readNetwork(ctx context.Context, envID string, in *NetworkDetailInput) (agg envs.Aggregate, row envs.NetworkRow, detail dockerx.NetworkDetail, raw []byte, err error) {
	agg, _, err = d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return agg, row, detail, nil, snapshotProblem(d.Log, envID, err)
	}
	row, err = findNetwork(agg, in.NetworkID, in.NodeID)
	if err != nil {
		if !errors.Is(err, errNoSuchNetwork) {
			return agg, row, detail, nil, err
		}
		// A node that did not answer may be the one that holds it: it is not gone then,
		// it is out of reach.
		if len(agg.NotReporting) > 0 {
			return agg, row, detail, nil, httpx.Problem(http.StatusConflict, problemNodeUnreachable,
				"Can't find this network: "+joinMore(agg.NotReporting, 3)+" didn't answer. Try again when it's back.")
		}
		return agg, row, detail, nil, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, "This network does not exist. It may have been removed.")
	}
	verbose := row.Scope == dockerx.ScopeSwarm && agg.Target.Probe.Type == dockerx.TypeSwarmManager
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err = d.Aggregator.OnNode(readCtx, envID, row.NodeID, func(c *dockerx.Client) error {
		var e error
		detail, raw, e = c.NetworkDetail(readCtx, row.ID, verbose)
		return e
	})
	switch {
	case err == nil:
		return agg, row, detail, raw, nil
	case dockerx.IsNotFound(err):
		return agg, row, detail, nil, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, "This network does not exist. It may have been removed.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return agg, row, detail, nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Coaming.")
	}
	return agg, row, detail, nil, snapshotProblem(d.Log, envID, err)
}

// getNetwork builds the detail page's answer: the network read live, the attachments from
// the lists of every node that answered, the services from the cluster read, and the
// aliases from an inspect of the first maxAliasInspects attached containers.
func getNetwork(ctx context.Context, d NetworksDeps, envID string, in *NetworkDetailInput) (NetworkDetailBody, error) {
	agg, row, detail, _, err := d.readNetwork(ctx, envID, in)
	if err != nil {
		return NetworkDetailBody{}, err
	}
	owner, kind := networkOwner(detail.Labels)
	body := NetworkDetailBody{
		ID: row.ID, Name: row.Name, Driver: row.Driver, Scope: row.Scope, Node: row.Node, NodeID: row.NodeID,
		Created: rfc3339(detail.Created), Internal: detail.Internal, Attachable: detail.Attachable, IPv6: detail.IPv6,
		BuiltIn: dockerx.IsBuiltinNetwork(row.Name, detail.Ingress || row.Ingress),
		Owner:   owner, OwnerKind: kind, Labels: detail.Labels, Options: detail.Options,
		IPAM:         make([]NetworkIPAMBody, 0, len(detail.IPAM)),
		Attachments:  []NetworkAttachmentBody{},
		NotReporting: agg.NotReporting,
	}
	if body.Created == "" {
		body.Created = rfc3339(row.Created)
	}
	for _, p := range detail.IPAM {
		body.IPAM = append(body.IPAM, NetworkIPAMBody{Subnet: p.Subnet, Gateway: p.Gateway, IPRange: p.IPRange})
	}

	containers := attachedContainers(agg, row)
	body.AttachedContainers = len(containers)
	body.AliasesTruncated = len(containers) > maxAliasInspects
	d.fillAliases(ctx, envID, row.ID, containers)
	body.Attachments = append(body.Attachments, containers...)

	services := d.attachedServices(ctx, envID, agg, row, detail)
	body.AttachedServices = len(services)
	body.Attachments = append(body.Attachments, services...)
	slices.SortStableFunc(body.Attachments, func(a, b NetworkAttachmentBody) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Node, b.Node), strings.Compare(a.NodeID, b.NodeID), strings.Compare(a.Kind, b.Kind))
	})
	return body, nil
}

// attachedContainers are the containers attached to the network, from the lists of the
// nodes that answered, sorted by name then node. A swarm-scoped network counts the
// containers of every node; a local one only its own node's, since another node's
// network with the same ID is a different network.
func attachedContainers(agg envs.Aggregate, row envs.NetworkRow) []NetworkAttachmentBody {
	out := []NetworkAttachmentBody{}
	for _, c := range agg.Containers {
		if row.Scope != dockerx.ScopeSwarm && c.NodeID != row.NodeID {
			continue
		}
		for _, cn := range c.Networks {
			if cn.NetworkID != row.ID {
				continue
			}
			out = append(out, NetworkAttachmentBody{
				Kind: "container", ID: c.ID, Name: c.Name, Node: c.Node, NodeID: c.NodeID, State: c.State,
				IPv4: cn.IPv4, IPv6: cn.IPv6, MAC: cn.MAC,
			})
			break
		}
	}
	slices.SortFunc(out, func(a, b NetworkAttachmentBody) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Node, b.Node), strings.Compare(a.NodeID, b.NodeID))
	})
	return out
}

// fillAliases inspects the first maxAliasInspects of the containers for the aliases they
// answer to on the network: at most perNodeCalls at once on one node and bulkParallel in
// all, each under the inspect timeout. A container whose inspect fails keeps aliases null
// and nothing else is lost.
func (d NetworksDeps) fillAliases(ctx context.Context, envID, networkID string, containers []NetworkAttachmentBody) {
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	for i := range min(len(containers), maxAliasInspects) {
		g.Go(func() error {
			release, err := lim.acquire(ctx, containers[i].NodeID)
			if err != nil {
				return nil
			}
			defer release()
			inspectCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
			defer cancel()
			var detail dockerx.ContainerDetail
			err = d.Aggregator.OnNode(inspectCtx, envID, containers[i].NodeID, func(c *dockerx.Client) error {
				var e error
				detail, _, e = c.ContainerDetail(inspectCtx, containers[i].ID)
				return e
			})
			if err != nil {
				d.Log.Debug("could not inspect a container for its aliases", "environment", envID, "container", containers[i].Name, "error", err)
				return nil
			}
			for _, n := range detail.Networks {
				if n.NetworkID == networkID {
					containers[i].Aliases = append([]string{}, n.Aliases...)
					break
				}
			}
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: a failure is a container without aliases
}

// attachedServices are the services whose specs mention a swarm-scoped network, even
// with no running task, from the cached cluster read, with the task counts the manager
// gave for the services on it. Services the manager reports on the network that the
// cluster read does not have are listed too. Off a swarm and for a local network there
// are none.
func (d NetworksDeps) attachedServices(ctx context.Context, envID string, agg envs.Aggregate, row envs.NetworkRow, detail dockerx.NetworkDetail) []NetworkAttachmentBody {
	out := []NetworkAttachmentBody{}
	if row.Scope != dockerx.ScopeSwarm || agg.Target.Probe.Type != dockerx.TypeSwarmManager {
		return out
	}
	tasks := map[string]int{}
	for _, s := range detail.Services {
		tasks[s.Name] = s.Tasks
	}
	seen := map[string]bool{}
	if d.Clusters != nil {
		// A failed read leaves what the last good one held.
		st, _ := d.Clusters.State(ctx, envID)
		for _, ref := range st.ServiceRefs {
			if slices.Contains(ref.Networks, row.ID) && !seen[ref.Name] {
				seen[ref.Name] = true
				out = append(out, NetworkAttachmentBody{Kind: "service", ID: ref.ID, Name: ref.Name, Tasks: tasks[ref.Name]})
			}
		}
	}
	for _, s := range detail.Services {
		if !seen[s.Name] {
			seen[s.Name] = true
			out = append(out, NetworkAttachmentBody{Kind: "service", Name: s.Name, Tasks: s.Tasks})
		}
	}
	return out
}

// endpointTarget finds the container and the network of a connect or a disconnect in the
// environment's aggregate. The container is named by its ID and node (the node may be
// left out only when the ID is on one node); a swarm-scoped network is found by its ID, a
// local one on the container's node first and, failing that, anywhere, so a network of
// another node is told apart from one that does not exist. The errors are already
// problems.
func endpointTarget(agg envs.Aggregate, networkID, nodeID, containerID string) (ct envs.ContainerRow, row envs.NetworkRow, err error) {
	var found []envs.ContainerRow
	for _, c := range agg.Containers {
		if c.ID == containerID && (nodeID == "" || c.NodeID == nodeID) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 0:
		return ct, row, missingProblem(agg, problemContainerNotFound, "This container does not exist on that node. It may have been removed.")
	case 1:
		ct = found[0]
	default:
		return ct, row, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
			"More than one node has a container with this ID. Say which one (nodeId).")
	}
	row, err = findNetwork(agg, networkID, ct.NodeID)
	if errors.Is(err, errNoSuchNetwork) {
		row, err = findNetwork(agg, networkID, "")
		if errors.Is(err, errNoSuchNetwork) {
			return ct, row, missingProblem(agg, problemNetworkNotFound, "This network does not exist. It may have been removed.")
		}
	}
	return ct, row, err
}

// missingProblem is the answer for something the lists do not have: a 404, or when a
// node did not answer and may hold it, a 409 node_unreachable.
func missingProblem(agg envs.Aggregate, code, detail string) error {
	if len(agg.NotReporting) > 0 {
		return httpx.Problem(http.StatusConflict, problemNodeUnreachable,
			"Can't find it: "+joinMore(agg.NotReporting, 3)+" didn't answer. Try again when it's back.")
	}
	return httpx.Problem(http.StatusNotFound, code, detail)
}

// recordEndpoint audits one connect or disconnect attempt: the network, the container,
// the container's node and, for a connect, the alias and addresses that were asked for.
func recordEndpoint(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent, action string, row envs.NetworkRow, ct envs.ContainerRow, ep dockerx.EndpointSpec, code string, own ownedAttempt) {
	details := map[string]string{"network": row.Name, "container": ct.Name, "node": ct.NodeID, "code": code}
	own.addTo(details)
	for k, v := range map[string]string{"alias": ep.Alias, "ipv4": ep.IPv4, "ipv6": ep.IPv6} {
		if v != "" {
			details[k] = v
		}
	}
	rec := code
	if code == codeConnected || code == codeDisconnected {
		rec = codeCreated
	}
	recordNetwork(ctx, d, p, envID, userAgent, action, row.ID, row.Name, rec, details)
}

// ownedAttempt says that a connect or disconnect is of one of Coaming's own containers,
// and at which stage of its audit trail the row is. The zero value is an ordinary
// container, whose rows carry neither.
type ownedAttempt struct {
	// kind is ownServer or ownAgent; empty for any other container.
	kind string
	// stage is stageCompleted or stageConnectionLost once the call was made.
	stage string
}

// addTo puts the own-container fields into the details of an audit row.
func (o ownedAttempt) addTo(details map[string]string) {
	if o.kind != "" {
		details["own"] = o.kind
	}
	if o.stage != "" {
		details["stage"] = o.stage
	}
}

// isSelfContainer says whether id is Coaming's own container: the ID, or a prefix of
// it of at least minSelfIDLen characters, starts with the one Coaming found for itself.
func isSelfContainer(selfID, id string) bool {
	return len(selfID) >= minSelfIDLen && strings.HasPrefix(id, selfID)
}

// ownContainerGate decides whether a connect or disconnect of ct is of one of Coaming's
// own containers, from a fresh inspect of it: the list can be seconds old, and neither
// its flag nor anything the client sent is trusted (D-11). For the server's own container
// or an agent's, it requires the container's name typed (422 confirmation_required, so
// nothing is sent) and writes the initiated row, which is why it returns the attempt to
// stamp on the final row. A failed inspect is mapped by problemFor, as the engine's own
// answer to the write would be.
func ownContainerGate(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID, userAgent, action string, row envs.NetworkRow, ct envs.ContainerRow, ep dockerx.EndpointSpec, confirm string, problemFor func(error) (string, error)) (ownedAttempt, string, error) {
	var own dockerx.ContainerOwnership
	inspectCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Aggregator.OnNode(inspectCtx, envID, ct.NodeID, func(c *dockerx.Client) error {
		var e error
		own, e = c.ContainerOwnership(inspectCtx, ct.ID)
		return e
	})
	if err != nil {
		code, problem := problemFor(err)
		return ownedAttempt{}, code, problem
	}
	var attempt ownedAttempt
	switch {
	case isSelfContainer(d.SelfContainerID, own.ID):
		attempt.kind = ownServer
	case own.Labels[ownerLabelAgent] == ownerLabelAgentValue:
		attempt.kind = ownAgent
	default:
		return attempt, "", nil
	}
	if own.Name == "" || confirm != own.Name {
		return attempt, codeConfirmationRequired, httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired, "Type the container's name to confirm this.")
	}
	// The call may take the recorder down with it, so the attempt is written first. With
	// no row, nothing is sent.
	details := map[string]string{"network": row.Name, "container": own.Name, "node": ct.NodeID, "code": stageInitiated, "stage": stageInitiated, "own": attempt.kind}
	for k, v := range map[string]string{"alias": ep.Alias, "ipv4": ep.IPv4, "ipv6": ep.IPv6} {
		if v != "" {
			details[k] = v
		}
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: networkTargetType, TargetID: row.ID,
		EnvID: envID, Result: audit.ResultSuccess,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit the start of a network change of an own container", "container", own.Name, "network", row.Name, "error", err)
		return attempt, codeActionFailed, httpx.Problem(http.StatusConflict, codeActionFailed, "The change could not be recorded in the audit log, so nothing was sent.")
	}
	attempt.stage = stageInitiated
	return attempt, "", nil
}

// connectContainer attaches a container to a network. Everything the lists can say is
// checked before anything is sent: the addresses, the node (a local network takes only
// its own node's containers), attachability, and whether the container is on it already.
// The write goes to the container's node, once and never retried, and every attempt on a
// container and a network that exist is audited.
func connectContainer(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID string, in *NetworkConnectInput) error {
	body := in.Body
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return snapshotProblem(d.Log, envID, err)
	}
	ct, row, err := endpointTarget(agg, in.NetworkID, body.NodeID, body.ContainerID)
	if err != nil {
		return err
	}
	ep := dockerx.EndpointSpec{Alias: body.Alias, IPv4: body.IPv4, IPv6: body.IPv6}
	var own ownedAttempt
	audited := func(code string) {
		recordEndpoint(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkConnected, row, ct, ep, code, own)
	}
	refuse := func(status int, code, detail string) error {
		audited(code)
		return httpx.Problem(status, code, detail)
	}
	if a, err := netip.ParseAddr(ep.IPv4); ep.IPv4 != "" && (err != nil || !a.Is4()) {
		return refuse(http.StatusUnprocessableEntity, problemAddressInvalid, "Enter an IPv4 address like 10.20.0.5.")
	}
	if a, err := netip.ParseAddr(ep.IPv6); ep.IPv6 != "" && (err != nil || !a.Is6() || a.Is4In6()) {
		return refuse(http.StatusUnprocessableEntity, problemAddressInvalid, "Enter an IPv6 address like fd00:20::5.")
	}
	if row.Scope != dockerx.ScopeSwarm && row.NodeID != ct.NodeID {
		return refuse(http.StatusUnprocessableEntity, problemNodeMismatch,
			row.Name+" is local to "+row.Node+", and "+ct.Name+" is on "+ct.Node+". A container can only join a local network of its own node.")
	}
	if row.Scope == dockerx.ScopeSwarm && !row.Attachable {
		return refuse(http.StatusUnprocessableEntity, problemNetworkNotAttachable,
			row.Name+" is not attachable, so standalone containers can't join it. Only services can.")
	}
	if slices.ContainsFunc(ct.Networks, func(cn dockerx.ContainerNet) bool { return cn.NetworkID == row.ID }) {
		return refuse(http.StatusConflict, problemAlreadyConnected, ct.Name+" is already on "+row.Name+".")
	}
	own, code, err := ownContainerGate(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkConnected, row, ct, ep, body.Confirm,
		func(err error) (string, error) { return networkConnectProblem(d.Log, envID, row, ct, err) })
	if err != nil {
		audited(code)
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, ct.NodeID, func(c *dockerx.Client) error {
		return c.NetworkConnect(writeCtx, row.ID, ct.ID, ep)
	})
	if own.kind != "" {
		own.stage = stageCompleted
	}
	if err != nil {
		if own.kind != "" && isConnectionLost(err) {
			// Joining a network can end the connection that carries this call; it is taken
			// as accepted, and the page shows what happened.
			own.stage = stageConnectionLost
			audited(codeConnected)
			return nil
		}
		code, problem := networkConnectProblem(d.Log, envID, row, ct, err)
		audited(code)
		return problem
	}
	audited(codeConnected)
	return nil
}

// networkConnectProblem turns the error of a connect into the audit code and the problem
// the SPA understands. The engine's own words are kept for the refusals the person can
// act on: an overlay a worker has not learned about yet (Pitfall 11), an address the
// network does not accept, and any other refusal.
func networkConnectProblem(log *slog.Logger, envID string, row envs.NetworkRow, ct envs.ContainerRow, err error) (string, error) {
	text := dockerx.DaemonText(err)
	lower := strings.ToLower(text)
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, dockerx.ErrAddressing):
		return problemAddressInvalid, httpx.Problem(http.StatusUnprocessableEntity, problemAddressInvalid, "Enter an address like 10.20.0.5.")
	case errors.Is(err, envs.ErrHostReadOnly), errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound),
		errors.As(envs.ClassifyDockerError(err), &denied):
		return networkWriteProblem(log, envID, err)
	case !dockerx.IsRefusal(err):
		return networkWriteProblem(log, envID, err)
	case dockerx.IsNotFound(err) && strings.Contains(lower, "no such container"):
		return problemContainerNotFound, httpx.Problem(http.StatusNotFound, problemContainerNotFound, ct.Name+" is already gone.")
	case dockerx.IsNotFound(err) && row.Scope == dockerx.ScopeSwarm:
		return problemNetworkUnavailableOnNode, httpx.Problem(http.StatusConflict, problemNetworkUnavailableOnNode, text)
	case dockerx.IsNotFound(err):
		return problemNetworkNotFound, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, row.Name+" is already gone.")
	case strings.Contains(lower, "already exists in network"):
		return problemAlreadyConnected, httpx.Problem(http.StatusConflict, problemAlreadyConnected, ct.Name+" is already on "+row.Name+".")
	case strings.Contains(lower, "not manually attachable"):
		return problemNetworkNotAttachable, httpx.Problem(http.StatusUnprocessableEntity, problemNetworkNotAttachable,
			row.Name+" is not attachable, so standalone containers can't join it. Only services can.")
	case strings.Contains(lower, "address") || strings.Contains(lower, "ipv4") || strings.Contains(lower, "ipv6") || strings.Contains(lower, "subnet"):
		return problemAddressInvalid, httpx.Problem(http.StatusUnprocessableEntity, problemAddressInvalid, clipDetail(text))
	}
	return codeActionFailed, httpx.Problem(http.StatusConflict, codeActionFailed, clipDetail(text))
}

// disconnectContainer detaches a container from a network, once and never forced. The
// list is not trusted to say whether the container is on the network (it may have joined
// or left a moment ago): the engine is asked, and its answer is mapped. The write goes to
// the container's node, and every attempt on a container and a network that exist is
// audited.
func disconnectContainer(ctx context.Context, d NetworksDeps, p *rbac.Principal, envID string, in *NetworkDisconnectInput) error {
	body := in.Body
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return snapshotProblem(d.Log, envID, err)
	}
	ct, row, err := endpointTarget(agg, in.NetworkID, body.NodeID, body.ContainerID)
	if err != nil {
		return err
	}
	var own ownedAttempt
	audited := func(code string) {
		recordEndpoint(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkDisconnected, row, ct, dockerx.EndpointSpec{}, code, own)
	}
	// A local network of another node cannot hold this container.
	if row.Scope != dockerx.ScopeSwarm && row.NodeID != ct.NodeID {
		audited(problemNodeMismatch)
		return httpx.Problem(http.StatusUnprocessableEntity, problemNodeMismatch,
			row.Name+" is local to "+row.Node+", and "+ct.Name+" is on "+ct.Node+", so it can't be on it.")
	}
	own, code, err := ownContainerGate(ctx, d, p, envID, in.UserAgent, audit.ActionNetworkDisconnected, row, ct, dockerx.EndpointSpec{}, body.Confirm,
		func(err error) (string, error) { return networkDisconnectProblem(d.Log, envID, row, ct, err) })
	if err != nil {
		audited(code)
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, ct.NodeID, func(c *dockerx.Client) error {
		return c.NetworkDisconnect(writeCtx, row.ID, ct.ID)
	})
	if own.kind != "" {
		own.stage = stageCompleted
	}
	if err != nil {
		if own.kind != "" && isConnectionLost(err) {
			// Cutting the server or an agent off its own network ends the connection that
			// carries this call, which is what it is meant to do here; it is taken as
			// accepted, and the page shows what happened.
			own.stage = stageConnectionLost
			audited(codeDisconnected)
			return nil
		}
		code, problem := networkDisconnectProblem(d.Log, envID, row, ct, err)
		audited(code)
		return problem
	}
	audited(codeDisconnected)
	return nil
}

// networkDisconnectProblem turns the error of a disconnect into the audit code and the
// problem the SPA understands.
func networkDisconnectProblem(log *slog.Logger, envID string, row envs.NetworkRow, ct envs.ContainerRow, err error) (string, error) {
	text := dockerx.DaemonText(err)
	lower := strings.ToLower(text)
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrHostReadOnly), errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound),
		errors.As(envs.ClassifyDockerError(err), &denied):
		return networkWriteProblem(log, envID, err)
	// A real engine answers this with a server error rather than a refusal class, so it is
	// recognised by its words before the class is asked.
	case strings.Contains(lower, "is not connected"):
		return problemNotConnected, httpx.Problem(http.StatusConflict, problemNotConnected, ct.Name+" isn't on "+row.Name+" any more.")
	case !dockerx.IsRefusal(err):
		return networkWriteProblem(log, envID, err)
	case dockerx.IsNotFound(err) && strings.Contains(lower, "no such container"):
		return problemContainerNotFound, httpx.Problem(http.StatusNotFound, problemContainerNotFound, ct.Name+" is already gone.")
	case dockerx.IsNotFound(err):
		return problemNetworkNotFound, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, row.Name+" is already gone.")
	}
	return codeActionFailed, httpx.Problem(http.StatusConflict, codeActionFailed, clipDetail(text))
}
