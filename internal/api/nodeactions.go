// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
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
	nodeTargetType = "node"

	problemNodeChanged      = "node_changed"
	problemLastManager      = "last_manager"
	problemNodeUpdateFailed = "node_update_failed"
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

// registerNodeActions mounts the impact, availability and role routes.
func registerNodeActions(r *httpx.Router, d NodesDeps) {
	registerNodeImpact(r, d)
	need := rbac.Requirement{Perm: rbac.PermNodesManage, Scope: rbac.ScopeEnvironment}
	const changeErrors = "A node whose version is not the one sent is 409 node_changed, and nothing is retried. 404 node_not_found when the swarm has no such node; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
		"Docker's own refusals come back mapped: 409 last_manager for the demotion of the last manager, 502 node_update_failed with Docker's words for any other. Bayplan itself never blocks a change that was confirmed by typing the node's name. " +
		"Needs nodes.manage (Admin) on the environment."

	httpx.Register(r, huma.Operation{
		OperationID: "setNodeAvailability", Method: http.MethodPost, Path: "/api/env/{envId}/nodes/{nodeId}/availability",
		Summary: "Drain, pause or activate a node",
		Description: "Sets the availability of one node to active, pause or drain, once. The server reads the node fresh and sends its whole spec with only the availability changed and the version sent, " +
			"so nothing else about the node changes. Before it writes, the server judges the change as getNodeActionImpact does; when that says needsConfirm (a drain of the node that runs Bayplan or of the only connected manager agent) " +
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
			"Before it writes, the server judges the change as getNodeActionImpact does; when that says needsConfirm (a demote that puts quorum at risk, of the node that runs Bayplan, of the only connected manager agent, " +
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
	return serviceActionProblem(d.services(), envID, problemNodeUpdateFailed, err)
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
