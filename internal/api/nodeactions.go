// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

const (
	nodeTargetType = "node"

	problemNodeChanged      = "node_changed"
	problemLastManager      = "last_manager"
	problemNodeUpdateFailed = "node_update_failed"
	problemNodeNotDown      = "node_not_down"
	problemNodeIsManager    = "node_is_manager"
	problemNodeRemoveFailed = "node_remove_failed"

	// nodeStateDown and nodeRoleManager are the values of a node's state and role the remove judges.
	nodeStateDown   = "down"
	nodeRoleManager = "manager"
)

// NodeAvailabilityRequest is the body of an availability change.
type NodeAvailabilityRequest struct {
	Availability string `json:"availability" enum:"active,pause,drain" doc:"What the node's spec should let the swarm do with it: active, pause (no new tasks) or drain (move its tasks away)"`
	Version      uint64 `json:"version" doc:"The node's version as the page last read it (the version of getNode); a node changed since is refused 409 node_changed"`
	Confirm      string `json:"confirm,omitempty" maxLength:"128" doc:"The node's host name, typed. Needed when the change's impact says needsConfirm; ignored otherwise"`
}

// NodeAvailabilityInput is the path and body of an availability change.
type NodeAvailabilityInput struct {
	NodeInput
	Body NodeAvailabilityRequest
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// NodeRoleRequest is the body of a role change.
type NodeRoleRequest struct {
	Role    string `json:"role" enum:"manager,worker" doc:"manager promotes a worker, worker demotes a manager"`
	Version uint64 `json:"version" doc:"The node's version as the page last read it (the version of getNode); a node changed since is refused 409 node_changed"`
	Confirm string `json:"confirm,omitempty" maxLength:"128" doc:"The node's host name, typed. Needed when the change's impact says needsConfirm; ignored otherwise"`
}

// NodeRoleInput is the path and body of a role change.
type NodeRoleInput struct {
	NodeInput
	Body NodeRoleRequest
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// NodeRemoveInput is the path and query of DELETE .../nodes/{nodeId}.
type NodeRemoveInput struct {
	NodeInput
	Confirm string `query:"confirm" maxLength:"128" doc:"The node's host name, typed. Always needed: a node is removed only by typing its name"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// registerNodeActions mounts the impact, availability, role and remove routes.
func registerNodeActions(r *httpx.Router, d NodesDeps) {
	registerNodeImpact(r, d)
	need := rbac.Requirement{Perm: rbac.PermNodesManage, Scope: rbac.ScopeEnvironment}
	const changeErrors = "A node whose version is not the one sent is 409 node_changed, and nothing is retried. 404 node_not_found when the swarm has no such node; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
		"Docker's own refusals come back mapped: 409 last_manager for the demotion of the last manager, 502 node_update_failed with Docker's words for any other. TierPlan itself never blocks a change that was confirmed by typing the node's name. " +
		"Needs nodes.manage (Admin) on the environment."

	httpx.Register(r, huma.Operation{
		OperationID: "setNodeAvailability", Method: http.MethodPost, Path: "/api/env/{envId}/nodes/{nodeId}/availability",
		Summary: "Drain, pause or activate a node",
		Description: "Sets the availability of one node to active, pause or drain, once. The server reads the node fresh and sends its whole spec with only the availability changed and the version sent, " +
			"so nothing else about the node changes. Before it writes, the server judges the change as getNodeActionImpact does; when that says needsConfirm (a drain of the node that runs TierPlan or of the only connected manager agent) " +
			"the body's confirm must be the node's host name, else 422 confirmation_required and nothing is sent. " + changeErrors + " Every attempt on a node that exists is audited as node.availability_changed with the name, from, to, whether the name was typed, the warnings and the code.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, need, func(ctx context.Context, in *NodeAvailabilityInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		impact := ""
		if in.Body.Availability == impactDrain || in.Body.Availability == impactPause {
			impact = in.Body.Availability
		}
		return nil, setNode(ctx, d, p, env.ID, nodeChange{
			action: audit.ActionNodeAvailabilityChanged, nodeID: in.NodeID, impact: impact, version: in.Body.Version,
			to: in.Body.Availability, confirm: in.Body.Confirm, userAgent: in.UserAgent,
		})
	})

	httpx.Register(r, huma.Operation{
		OperationID: "setNodeRole", Method: http.MethodPost, Path: "/api/env/{envId}/nodes/{nodeId}/role",
		Summary: "Promote a worker or demote a manager",
		Description: "Sets the role of one node to manager or worker, once. The server reads the node fresh and sends its whole spec with only the role changed and the version sent. " +
			"Before it writes, the server judges the change as getNodeActionImpact does; when that says needsConfirm (a demote that puts quorum at risk, of the node that runs TierPlan, of the only connected manager agent, " +
			"or with the managers' state unreadable) the body's confirm must be the node's host name, else 422 confirmation_required and nothing is sent. " + changeErrors + " Every attempt on a node that exists is audited as node.role_changed with the name, from, to, whether the name was typed, the warnings and the code.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, need, func(ctx context.Context, in *NodeRoleInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		impact := impactPromote
		if in.Body.Role == "worker" {
			impact = impactDemote
		}
		return nil, setNode(ctx, d, p, env.ID, nodeChange{
			action: audit.ActionNodeRoleChanged, nodeID: in.NodeID, impact: impact, version: in.Body.Version,
			to: in.Body.Role, confirm: in.Body.Confirm, userAgent: in.UserAgent,
		})
	})

	httpx.Register(r, huma.Operation{
		OperationID: "removeNode", Method: http.MethodDelete, Path: "/api/env/{envId}/nodes/{nodeId}",
		Summary: "Remove a node that is down",
		Description: "Removes one node from the swarm, never forced. Only a worker whose state is down is removed: a manager is refused 409 node_is_manager (demote it first) and a node that is not down 409 node_not_down, both before anything is written, " +
			"and the query's confirm must be the node's host name, else 422 confirmation_required. The remove is sent once on a manager and never retried; the request to the agent carries no force key, so a node that is still up cannot be removed by any means. " +
			"The swarm forgets the node, and TierPlan revokes the agent that ran on it at once with the reason node_removed and without blocking the host, so a host that rejoins the swarm can enrol again; " +
			"a revoke that fails after the node is gone is logged and left to the reaper, which revokes the agent of a node missing from two node lists. " +
			"404 node_not_found when the swarm has no such node; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected; Docker's own refusal comes back as 409 node_not_down or 502 node_remove_failed with Docker's words. " +
			"Every attempt on a node that exists is audited as node.removed with the host name, the agent revoked and the code. Needs nodes.manage (Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, need, func(ctx context.Context, in *NodeRemoveInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		return nil, removeNode(ctx, d, p, env.ID, in)
	})
}

// removeNode removes one node that is down. It reads the node, refuses a manager and a node that
// is not down and a name that was not typed, all before any write, then removes it once on a
// manager, never forced. Once the node is gone the agent that ran on it is revoked without a
// block; a revoke that fails is logged and left to the reaper. Every attempt on a node that
// exists is audited.
func removeNode(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, in *NodeRemoveInput) error {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return err
	}
	var detail dockerx.NodeDetail
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.NodeInspect(readCtx, in.NodeID)
		return e
	})
	cancel()
	rec := nodeRemoval{hostname: cleanTaskText(detail.Hostname)}
	if err != nil {
		if errors.Is(err, dockerx.ErrNodeNotFound) {
			return nodeNotFound()
		}
		code, problem := nodeActionProblem(d, envID, err)
		rec.code = code
		auditNodeRemoval(ctx, d, p, envID, in, rec)
		return problem
	}
	name := rec.hostname
	switch {
	case detail.Role == nodeRoleManager:
		rec.code = problemNodeIsManager
		auditNodeRemoval(ctx, d, p, envID, in, rec)
		return httpx.Problem(http.StatusConflict, problemNodeIsManager, "Demote "+name+" before removing it.")
	case detail.State != nodeStateDown:
		rec.code = problemNodeNotDown
		auditNodeRemoval(ctx, d, p, envID, in, rec)
		return nodeNotDown(name)
	case in.Confirm == "" || (in.Confirm != detail.Hostname && in.Confirm != name):
		rec.code = codeConfirmationRequired
		auditNodeRemoval(ctx, d, p, envID, in, rec)
		return httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired, "Type the node's name, "+name+", to confirm this.")
	}

	// The remove is sent once and seen through, so a browser that goes away while it runs does not
	// turn a removal that happened into a failure.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancelWrite()
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		return c.NodeRemove(writeCtx, in.NodeID)
	})
	if err != nil {
		var problem error
		if errors.Is(err, dockerx.ErrNodeNotDown) {
			// The node came back between the read and the write.
			rec.code, problem = problemNodeNotDown, nodeNotDown(name)
		} else {
			rec.code, problem = nodeProblemOf(d, envID, problemNodeRemoveFailed, err)
		}
		auditNodeRemoval(ctx, d, p, envID, in, rec)
		return problem
	}
	refreshClusters(ctx, d.services(), envID)
	rec.agent = revokeAgentOfRemovedNode(ctx, d, p, envID, in)
	rec.code = actionDone
	auditNodeRemoval(ctx, d, p, envID, in, rec)
	return nil
}

// nodeNotDown is the refusal to remove a node that is not down.
func nodeNotDown(name string) error {
	return httpx.Problem(http.StatusConflict, problemNodeNotDown, name+" isn't down. Only a node that is down can be removed.")
}

// revokeAgentOfRemovedNode revokes the active agent that ran on the node just removed, without a
// block, and returns its ID, or "" when there was none or the revoke failed. A failure is the
// server's to log, not the caller's: the node is gone either way, and the reaper revokes the
// agent of a node missing from two node lists.
func revokeAgentOfRemovedNode(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, in *NodeRemoveInput) string {
	if d.Agents == nil {
		return ""
	}
	// The node is gone, so the revoke does not stop with the request.
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	agentID, err := d.Agents.ActiveAgentOfSwarmNode(wctx, envID, in.NodeID)
	if err != nil {
		d.Log.Warn("could not find the agent of a removed node; the reaper will revoke it", "node", in.NodeID, "error", err)
		return ""
	}
	if agentID == "" {
		return ""
	}
	switch err := d.Agents.RevokeRemovedNode(wctx, p.AuditActor(), envID, agentID, httpx.ClientIP(ctx), in.UserAgent); {
	case err == nil:
		return agentID
	case errors.Is(err, agents.ErrAlreadyRevoked), errors.Is(err, agents.ErrAgentNotFound):
		return ""
	default:
		d.Log.Warn("could not revoke the agent of a removed node; the reaper will revoke it", "node", in.NodeID, "agent", agentID, "error", err)
		return ""
	}
}

// nodeRemoval is what one remove attempt learned for its audit row.
type nodeRemoval struct {
	hostname, agent, code string
}

// auditNodeRemoval writes the row of one remove attempt: a success when the code is done, else a
// failure whose reason is the code. It is written on a context that outlives the request: the
// removal has already happened or been refused.
func auditNodeRemoval(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, in *NodeRemoveInput, rec nodeRemoval) {
	result, reason := audit.ResultSuccess, ""
	if rec.code != actionDone {
		result, reason = audit.ResultFailure, rec.code
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionNodeRemoved, TargetType: nodeTargetType, TargetID: in.NodeID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"hostname": rec.hostname, "agent": rec.agent, "code": rec.code},
	}); err != nil {
		d.Log.Error("could not audit a node removal", "node", in.NodeID, "error", err)
	}
}

// nodeChange is one availability or role change asked for.
type nodeChange struct {
	// action is the audit action, which says whether availability or role is changed.
	action string
	nodeID string
	// impact is the change as nodeImpact judges it; empty for an activate, which has none.
	impact  string
	version uint64
	// to is the availability or the role asked for.
	to, confirm, userAgent string
}

// setNode makes one availability or role change. It reads the node and judges the change as
// nodeImpact does, refuses one that needs the node's name when it was not typed, and then writes
// once on a manager with the version it was given. Every attempt on a node that exists is audited.
func setNode(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, ch nodeChange) error {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return err
	}
	in, err := gatherImpact(ctx, d, envID, ch.nodeID, ch.impact)
	if err != nil {
		if errors.Is(err, dockerx.ErrNodeNotFound) {
			return nodeNotFound()
		}
		code, problem := nodeActionProblem(d, envID, err)
		auditNodeChange(ctx, d, p, envID, ch, nodeAudit{code: code})
		return problem
	}
	impact := nodeImpact(in)
	from := in.Node.Availability
	if ch.action == audit.ActionNodeRoleChanged {
		from = in.Node.Role
	}
	typed := ch.confirm != "" && (ch.confirm == in.Node.Hostname || ch.confirm == impact.Hostname)
	rec := nodeAudit{name: impact.Hostname, from: from, confirmed: typed, warnings: impact.Warnings}

	if impact.NeedsConfirm && !typed {
		rec.code = codeConfirmationRequired
		auditNodeChange(ctx, d, p, envID, ch, rec)
		return httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired,
			"Type the node's name, "+impact.Hostname+", to confirm this.")
	}

	// The write is sent once and seen through, so a browser that goes away while it runs does not
	// turn a change that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	update := dockerx.NodeChange{Version: ch.version}
	if ch.action == audit.ActionNodeRoleChanged {
		update.Role = ch.to
	} else {
		update.Availability = ch.to
	}
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		return c.NodeUpdate(writeCtx, ch.nodeID, update)
	})
	if err != nil {
		code, problem := nodeActionProblem(d, envID, err)
		rec.code = code
		auditNodeChange(ctx, d, p, envID, ch, rec)
		return problem
	}
	refreshClusters(ctx, d.services(), envID)
	rec.code = actionDone
	auditNodeChange(ctx, d, p, envID, ch, rec)
	return nil
}

// nodeActionProblem turns the error of a node read or update into the code its audit row records
// and the problem the SPA words. Docker's refusal of the last manager and a stale version have
// their own; the rest are the ones every swarm write shares, and any other refusal keeps
// Docker's own words.
func nodeActionProblem(d NodesDeps, envID string, err error) (string, error) {
	return nodeProblemOf(d, envID, problemNodeUpdateFailed, err)
}

// nodeProblemOf is nodeActionProblem for a write whose other refusals are failedType.
func nodeProblemOf(d NodesDeps, envID, failedType string, err error) (string, error) {
	switch {
	case errors.Is(err, dockerx.ErrLastManager):
		return problemLastManager, httpx.Problem(http.StatusConflict, problemLastManager, "You can't demote the last manager. Promote another node first.")
	case errors.Is(err, dockerx.ErrNodeChanged):
		return problemNodeChanged, httpx.Problem(http.StatusConflict, problemNodeChanged,
			"This node was changed by someone else after it was read. Nothing was changed; reload the page and try again.")
	case errors.Is(err, dockerx.ErrNodeNotFound):
		return problemNodeNotFound, nodeNotFound()
	}
	if errors.Is(err, envs.ErrHostReadOnly) {
		return codeHostReadOnly, httpx.Problem(http.StatusConflict, codeHostReadOnly, swarmWriteDetail[codeHostReadOnly])
	}
	return serviceActionProblem(d.services(), envID, failedType, err)
}

// nodeAudit is what one attempt learned for its audit row.
type nodeAudit struct {
	name, from, code string
	confirmed        bool
	warnings         []string
}

// auditNodeChange writes the row of one attempt: a success when the code is done, else a failure
// whose reason is the code. It is written on a context that outlives the request: the change has
// already happened or been refused.
func auditNodeChange(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, ch nodeChange, rec nodeAudit) {
	result, reason := audit.ResultSuccess, ""
	if rec.code != actionDone {
		result, reason = audit.ResultFailure, rec.code
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: ch.action, TargetType: nodeTargetType, TargetID: ch.nodeID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: ch.userAgent,
		Details: map[string]string{
			"name": rec.name, "from": rec.from, "to": ch.to, "confirmed": strconv.FormatBool(rec.confirmed),
			"warnings": strings.Join(rec.warnings, ","), "code": rec.code,
		},
	}); err != nil {
		// The change has been made or refused and the caller must still be told which; the lost
		// row is the server's fault and is logged.
		d.Log.Error("could not audit a node change", "action", ch.action, "node", ch.nodeID, "error", err)
	}
}
