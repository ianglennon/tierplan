// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

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

	problemNetworkBuiltin  = "network_builtin"
	problemNetworkInUse    = "network_in_use"
	problemNetworkNotFound = "network_not_found"
	problemNodeUnreachable = codeNodeUnreachable
	codeRemoved            = "removed"
)

// NetworksDeps are the dependencies of the network routes.
type NetworksDeps struct {
	// Aggregator reaches an engine: a write to one node goes through WriteOnNode, a
	// cluster-scoped one through WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters reads the services of a swarm live before a delete.
	Clusters *envs.Clusters
	Audit    *audit.Writer
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
	if err := d.Audit.Record(ctx, audit.Entry{
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
}

// networkUsageOf counts the attachments in an aggregate. Services come from the cached
// cluster read of a swarm, whatever age it has: a delete reads them live and never
// trusts this.
func networkUsageOf(ctx context.Context, clusters *envs.Clusters, envID string, agg envs.Aggregate) networkUsage {
	u := networkUsage{local: map[[2]string]int{}, wide: map[string]int{}, services: map[string]int{}}
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
// the list the caller saw: every node's containers are read again, and on a swarm the
// service specs are read from a manager. It returns an empty code when nothing uses the
// network. Otherwise it returns the problem code, its status and its words:
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
	fresh, _, err := d.Aggregator.FreshSnapshot(ctx, envID)
	if err != nil {
		return "", 0, "", err
	}
	swarmScoped := row.Scope == dockerx.ScopeSwarm
	switch {
	case swarmScoped && len(fresh.NotReporting) > 0:
		return problemNodeUnreachable, http.StatusConflict,
			"Can't tell whether " + row.Name + " is in use: " + joinMore(fresh.NotReporting, 3) + " didn't answer. Try again when it's back.", nil
	case !swarmScoped && !slices.ContainsFunc(fresh.Nodes, func(n envs.NodeRef) bool { return n.ID == row.NodeID }):
		return problemNodeUnreachable, http.StatusConflict,
			"Can't tell whether " + row.Name + " is in use: " + row.Node + " didn't answer. Try again when it's back.", nil
	}
	if !slices.ContainsFunc(fresh.Networks, func(n envs.NetworkRow) bool {
		return n.ID == row.ID && (swarmScoped || n.NodeID == row.NodeID)
	}) {
		return problemNetworkNotFound, http.StatusNotFound, row.Name + " is already gone.", nil
	}

	var users []string
	for _, c := range fresh.Containers {
		if !swarmScoped && c.NodeID != row.NodeID {
			continue
		}
		if slices.ContainsFunc(c.Networks, func(cn dockerx.ContainerNet) bool { return cn.NetworkID == row.ID }) {
			users = append(users, "container "+c.Name)
		}
	}
	if swarmScoped && (fresh.Target.Probe.Type == dockerx.TypeSwarmManager || fresh.Target.Probe.Type == dockerx.TypeSwarmWorker) {
		var refs []dockerx.ServiceRef
		if d.Clusters == nil {
			return problemNodeUnreachable, http.StatusConflict, "Can't tell whether " + row.Name + " is in use: no manager answered. Try again when one is back.", nil
		}
		readCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		if err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
			var e error
			refs, e = c.ServiceRefs(readCtx)
			return e
		}); err != nil {
			d.Log.Warn("could not read the services before a network delete", "environment", envID, "error", err)
			return problemNodeUnreachable, http.StatusConflict, "Can't tell whether " + row.Name + " is in use: no manager answered. Try again when one is back.", nil
		}
		for _, ref := range refs {
			if slices.Contains(ref.Networks, row.ID) {
				users = append(users, "service "+ref.Name)
			}
		}
	}
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
