// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The ingress node routes (Phase 22, PRX-01): an Admin makes the environment's ingress join token, reads which one is
// current, lists the nodes that enrolled with it and revokes one of them. The token appears once, in the answer that
// makes it, and every answer that carries or describes a token is no-store.

const (
	problemIngressNodeNotFound       = "ingress_node_not_found"
	problemIngressNodeAlreadyRevoked = "ingress_node_already_revoked"
	problemIngressNoManager          = "ingress_no_manager"
	problemIngressTokenNotStored     = "ingress_token_not_stored" //nolint:gosec // G101: a problem type, not a credential

	// The values of IngressNodeBody.Sync.
	ingressSyncCurrent      = "current"
	ingressSyncPending      = "pending"
	ingressSyncIncompatible = "incompatible"
	ingressSyncUnknown      = "unknown"
)

// IngressTokenIssuer makes or rotates an environment's join token and, where ingress is on, keeps the swarm's secret in step
// with it: *ingressdeploy.Service has this method.
type IngressTokenIssuer interface {
	IssueJoinToken(ctx context.Context, actor audit.Actor, owner, envID, ip, ua string) (ingresshub.Token, error)
}

var _ IngressTokenIssuer = (*ingressdeploy.Service)(nil)

// IngressNodesDeps are the dependencies of the ingress node routes.
type IngressNodesDeps struct {
	// Service holds the join tokens and the nodes. Nil answers 503 ingress_unavailable, so the routes are always registered.
	Service *ingresshub.Service
	// Issuer makes the join token. On a swarm whose ingress is on it stores the token as the proxies' swarm secret, so the
	// ensure loop does not replace it. Nil makes the token through Service alone.
	Issuer IngressTokenIssuer
	// Live says whether a node has a live session now. Nil means none has: every node shows as not connected.
	Live func(nodeID string) bool
	// CurrentBundle is the ID of the bundle an environment's nodes should hold now, and false when none has been
	// built. Nil means none is known: the sync of every node is unknown.
	CurrentBundle func(envID string) (string, bool)
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// IngressJoinTokenBody is the answer to making or rotating the ingress join token.
type IngressJoinTokenBody struct {
	Token      string `json:"token" doc:"The ingress join token. Shown only in this answer: the server keeps its hash and cannot show it again"`
	Generation int    `json:"generation" doc:"Which token this is for the environment, from 1"`
	Rotated    bool   `json:"rotated" doc:"True when it replaced an earlier token, which stops enrolling nodes. Nodes already enrolled are not affected"`
}

// IngressJoinTokenOutput wraps IngressJoinTokenBody for huma.
type IngressJoinTokenOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressJoinTokenBody
}

// IngressJoinTokenStatusBody is what may be shown of an environment's current ingress join token.
type IngressJoinTokenStatusBody struct {
	Exists     bool   `json:"exists" doc:"False while the environment has never had an ingress join token; the other fields are then empty"`
	Generation int    `json:"generation" doc:"Which token this is for the environment, from 1"`
	CreatedAt  string `json:"createdAt,omitempty" format:"date-time" doc:"When the token was made"`
	CreatedBy  string `json:"createdBy" doc:"Who made it, as recorded in the audit log"`
	Disabled   bool   `json:"disabled" doc:"True while the token enrols nobody, until the next rotation"`
}

// IngressJoinTokenStatusOutput wraps IngressJoinTokenStatusBody for huma.
type IngressJoinTokenStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressJoinTokenStatusBody
}

// IngressNodeBody is one ingress node.
type IngressNodeBody struct {
	ID               string   `json:"id" doc:"The node ID, used in the path of the revoke"`
	Name             string   `json:"name" doc:"The name the node gave itself when it enrolled"`
	SwarmNodeID      string   `json:"swarmNodeId" doc:"The swarm node it said it runs on; empty for a node of a standalone host"`
	Status           string   `json:"status" enum:"active,revoked" doc:"revoked means an administrator revoked the node: it is disconnected and its key is refused"`
	Version          string   `json:"version" doc:"The version of the node's supervisor when it last connected"`
	Schemas          []int    `json:"schemas" doc:"The bundle schemas the node said it can apply; empty until it has said hello"`
	AppliedBundleID  string   `json:"appliedBundleId" doc:"The bundle the node last applied; empty if none"`
	AppliedAt        string   `json:"appliedAt,omitempty" format:"date-time" doc:"When it applied that bundle"`
	RejectedBundleID string   `json:"rejectedBundleId" doc:"The last bundle the node refused; empty if none"`
	RejectedReason   string   `json:"rejectedReason" doc:"Why it refused that bundle, as a stable code"`
	Health           string   `json:"health" doc:"What the node last reported about its proxy: ok, unhealthy, or empty before it has reported"`
	LastSeenAt       string   `json:"lastSeenAt,omitempty" format:"date-time" doc:"When the node was last heard from, to the minute. Absent if it never has been"`
	CreatedAt        string   `json:"createdAt,omitempty" format:"date-time" doc:"When the node enrolled"`
	RevokedAt        string   `json:"revokedAt,omitempty" format:"date-time" doc:"When the node was revoked"`
	RevokedBy        string   `json:"revokedBy" doc:"Who revoked it, as recorded in the audit log"`
	RevokeReason     string   `json:"revokeReason" doc:"The reason given when it was revoked"`
	ReenrolBlocked   bool     `json:"reenrolBlocked" doc:"True when the revoke also blocked the host from enrolling again with the join token still mounted in its container"`
	HeldHostnames    []string `json:"heldHostnames" doc:"Every hostname whose private key this node was ever sent, sorted"`
	Incompatible     bool     `json:"incompatible" doc:"True when the node's supervisor shares no bundle schema with this server, so it keeps the last bundle it had"`
	Connected        bool     `json:"connected" doc:"Whether the node has a live connection now"`
	Sync             string   `json:"sync" enum:"current,pending,incompatible,unknown" doc:"current: the node applied the bundle it should hold. pending: it holds an older one, or none, and a newer is waiting (not a failure). incompatible: it cannot apply this server's bundles. unknown: the node is revoked or no bundle has been built"`
}

// IngressNodePathInput carries {envId} and {nodeId} for the routes that act on one ingress node.
type IngressNodePathInput struct {
	EnvironmentPathInput
	NodeID string `path:"nodeId" doc:"The ingress node ID, as listed by GET /api/env/{envId}/ingress/nodes"`
}

// RevokeIngressNodeBody is the optional body of a revoke.
type RevokeIngressNodeBody struct {
	Reason       string `json:"reason,omitempty" maxLength:"256" doc:"Why the node is revoked, for the audit log and the node table. Optional"`
	AllowReenrol bool   `json:"allowReenrol,omitempty" doc:"Let the host enrol again with the join token still mounted in its container. By default a revoke blocks that"`
}

// RevokeIngressNodeInput is a revoke of one ingress node.
type RevokeIngressNodeInput struct {
	IngressNodePathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      *RevokeIngressNodeBody
}

// RevokeIngressNodeBodyOut is what a revoke says.
type RevokeIngressNodeBodyOut struct {
	HeldHostnames          []string `json:"heldHostnames" doc:"The hostnames whose private keys this node may still hold, sorted, as every one its bundles ever carried. They should be re-issued: they are not changed by the revoke"`
	RotateTokenRecommended bool     `json:"rotateTokenRecommended" doc:"Always true: the join token stays mounted in the revoked node's container until it is rotated"`
	RevokedAt              string   `json:"revokedAt" format:"date-time" doc:"When the node was revoked"`
}

// RevokeIngressNodeOutput wraps RevokeIngressNodeBodyOut for huma.
type RevokeIngressNodeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         RevokeIngressNodeBodyOut
}

// IngressNodeListBody is every ingress node of an environment.
type IngressNodeListBody struct {
	Items []IngressNodeBody `json:"items" doc:"One entry per node, oldest first, revoked nodes included"`
}

// IngressNodeListOutput wraps IngressNodeListBody for huma.
type IngressNodeListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressNodeListBody
}

// ingressNodeSync says how a node stands against the bundle it should hold (D-12). A bundle that has not reached the
// node yet is pending, never a failure.
func ingressNodeSync(n ingresshub.Node, currentID string, haveCurrent bool) string {
	switch {
	case n.Status != "active" || !haveCurrent || currentID == "":
		return ingressSyncUnknown
	case n.Incompatible:
		return ingressSyncIncompatible
	case n.AppliedBundleID == currentID:
		return ingressSyncCurrent
	}
	return ingressSyncPending
}

// RegisterIngressNodes registers the ingress join token routes, the node list and the revoke of one node. The token and
// list routes need ingress.manage on the environment (its Admin); the revoke needs environments.revoke on it, the same
// permission as revoking an agent. The routes are always registered; without a Service they answer 503
// ingress_unavailable.
func RegisterIngressNodes(r *httpx.Router, d IngressNodesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress nodes are not available on this server.")
	}
	fail := func(ctx context.Context, what string, err error) error {
		switch {
		case errors.Is(err, ingresshub.ErrEnvironmentNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		case errors.Is(err, ingresshub.ErrNodeNotFound):
			return httpx.Problem(http.StatusNotFound, problemIngressNodeNotFound, "No such ingress node in this environment.")
		case errors.Is(err, ingresshub.ErrAlreadyRevoked):
			return httpx.Problem(http.StatusConflict, problemIngressNodeAlreadyRevoked, "That node was revoked while you were looking at it.")
		}
		var locked *ingressdeploy.LockedError
		var notStored *ingressdeploy.TokenNotStoredError
		switch {
		case errors.As(err, &locked):
			name := ""
			if env := httpx.EnvFrom(ctx); env != nil {
				name = env.Name
			}
			return operationLocked(name, locked.Holder)
		case errors.Is(err, ingressdeploy.ErrPaused):
			return httpx.Problem(http.StatusConflict, problemIngressPaused, "Ingress on this environment was paused by a restore. Resume it first.")
		case errors.Is(err, ingressdeploy.ErrNoManager):
			return httpx.Problem(http.StatusConflict, problemIngressNoManager, "No manager node's agent is connected, so the new token cannot be stored on the swarm. Nothing was changed.")
		case errors.As(err, &notStored):
			d.Log.Error("ingress node request failed", "step", what, "error", err)
			return httpx.Problem(http.StatusBadGateway, problemIngressTokenNotStored,
				"The swarm would not take the new join token, so no token was handed out. Ingress will make its own token shortly; rotate again if you need to read it.")
		}
		d.Log.Error("ingress node request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "issueIngressJoinToken",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/join-token",
		Summary:     "Make or rotate an environment's ingress join token",
		Description: "Makes the ingress join token that lets ingress nodes enrol into this environment, or, when it has one, makes a new one and " +
			"retires the current one in the same transaction. The token is in the answer, once, with Cache-Control no-store: the server keeps only " +
			"its hash. A rotation stops the old token enrolling new nodes and does not affect nodes that are already enrolled, which authenticate with " +
			"their own key. On a swarm whose ingress is on, the token is also stored as the proxies' swarm secret, under the environment's operation lock, so the " +
			"deployer adopts it and does not make another: 409 operation_locked while another operation holds the environment, 409 ingress_paused while a restore's pause stands, " +
			"409 ingress_no_manager when no manager is connected, 502 ingress_token_not_stored when the swarm refuses the secret; in each case no token is handed out. " +
			"404 environment_not_found. Needs ingress.manage on the environment (its Admin). Audited as ingress.join_token_created or " +
			"ingress.join_token_rotated with the generation, never the token.",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *TokenMutationInput) (*IngressJoinTokenOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		var tok ingresshub.Token
		var err error
		if d.Issuer != nil {
			tok, err = d.Issuer.IssueJoinToken(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		} else {
			tok, err = d.Service.IssueToken(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		}
		if err != nil {
			return nil, fail(ctx, "issue the join token", err)
		}
		return &IngressJoinTokenOutput{CacheControl: "no-store", Body: IngressJoinTokenBody{
			Token: tok.Token, Generation: tok.Generation, Rotated: tok.Rotated,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressJoinToken",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/join-token",
		Summary:     "Show which ingress join token an environment has",
		Description: "Whether the environment has an ingress join token, its generation, when and by whom it was made, and whether it is disabled. " +
			"Never the token: it is stored only as a hash. Answered with Cache-Control no-store. 404 environment_not_found. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressJoinTokenStatusOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		st, err := d.Service.TokenStatus(ctx, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "ingress join token status", err)
		}
		return &IngressJoinTokenStatusOutput{CacheControl: "no-store", Body: IngressJoinTokenStatusBody{
			Exists: st.Exists, Generation: st.Generation, CreatedAt: rfc3339(st.CreatedAt), CreatedBy: st.CreatedBy, Disabled: st.Disabled,
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "disableIngressJoinToken",
		Method:        http.MethodDelete,
		Path:          "/api/env/{envId}/ingress/join-token",
		Summary:       "Disable an environment's ingress join token",
		DefaultStatus: http.StatusNoContent,
		Description: "The current ingress join token enrols nobody from now on. Nodes that are already enrolled keep working. Making a new token " +
			"enables enrolment again. Disabling a disabled or missing token changes nothing. Answered with Cache-Control no-store. 404 environment_not_found. " +
			"Needs ingress.manage on the environment (its Admin). Audited as ingress.join_token_disabled.",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *TokenMutationInput) (*IngressNoContentOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		if err := d.Service.DisableToken(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent); err != nil {
			return nil, fail(ctx, "disable the ingress join token", err)
		}
		return &IngressNoContentOutput{CacheControl: "no-store"}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listIngressNodes",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/nodes",
		Summary:     "List the ingress nodes of an environment",
		Description: "Every node that enrolled with the environment's ingress join token, revoked ones included, oldest first: its name, the swarm node it " +
			"runs on, status, supervisor version and bundle schemas, the bundle it applied and the one it refused, its proxy health, when it was last " +
			"seen, whether it is connected now, whether it is in sync with the bundle it should hold (a bundle still on its way is pending, not a " +
			"failure), whether it is incompatible with this server, and for a revoked node who revoked it, why, and the hostnames whose keys it held. " +
			"Never carries a key or a token. Answered with Cache-Control no-store. 404 environment_not_found. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressNodeListOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		// An environment with no nodes is a 200 with an empty list; one that does not exist is a 404.
		if _, err := d.Service.TokenStatus(ctx, in.EnvID); err != nil {
			return nil, fail(ctx, "look up the environment", err)
		}
		nodes, err := d.Service.ListNodes(ctx, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "list the ingress nodes", err)
		}
		var (
			currentID string
			haveBndl  bool
		)
		if d.CurrentBundle != nil {
			currentID, haveBndl = d.CurrentBundle(in.EnvID)
		}
		items := make([]IngressNodeBody, 0, len(nodes))
		for _, n := range nodes {
			b := IngressNodeBody{
				ID: n.ID, Name: n.Name, SwarmNodeID: n.SwarmNodeID, Status: n.Status, Version: n.Version, Schemas: n.Schemas,
				AppliedBundleID: n.AppliedBundleID, AppliedAt: rfc3339(n.AppliedAt),
				RejectedBundleID: n.RejectedBundleID, RejectedReason: n.RejectedReason,
				Health: n.Health, LastSeenAt: rfc3339(n.LastSeenAt), CreatedAt: rfc3339(n.CreatedAt), RevokedAt: rfc3339(n.RevokedAt),
				RevokedBy: n.RevokedBy, RevokeReason: n.RevokeReason, ReenrolBlocked: n.ReenrolBlocked,
				HeldHostnames: n.HeldHostnames, Incompatible: n.Incompatible,
				Sync: ingressNodeSync(n, currentID, haveBndl),
			}
			if b.Schemas == nil {
				b.Schemas = []int{}
			}
			if b.HeldHostnames == nil {
				b.HeldHostnames = []string{}
			}
			if d.Live != nil && n.Status == "active" {
				b.Connected = d.Live(n.ID)
			}
			items = append(items, b)
		}
		return &IngressNodeListOutput{CacheControl: "no-store", Body: IngressNodeListBody{Items: items}}, nil
	})
	httpx.Register(r, huma.Operation{
		OperationID: "revokeIngressNode",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/nodes/{nodeId}/revoke",
		Summary:     "Revoke one ingress node",
		Description: "The node is disconnected within a second and its key is refused from now on; no other node is touched and nothing on its host " +
			"is changed. Unless allowReenrol is true, its host is also blocked from enrolling again with the join token still mounted in its container. " +
			"The answer lists heldHostnames: the names whose private keys this node may still hold and that should be re-issued. " +
			"rotateTokenRecommended is always true. The optional reason is kept in the audit log. 404 environment_not_found or " +
			problemIngressNodeNotFound + " (a node of another environment is the same 404); 409 " + problemIngressNodeAlreadyRevoked + " when it was revoked already. " +
			"Answered with Cache-Control no-store. Needs environments.revoke on the environment (env Admin or instance admin), the permission that revokes an " +
			"agent. Audited as ingress.node_revoked.",
		Tags: []string{"ingress"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRevoke, Scope: rbac.ScopeEnvironment}, func(ctx context.Context, in *RevokeIngressNodeInput) (*RevokeIngressNodeOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		var opts ingresshub.RevokeOptions
		if in.Body != nil {
			opts = ingresshub.RevokeOptions{Reason: in.Body.Reason, AllowReenrol: in.Body.AllowReenrol}
		}
		res, err := d.Service.Revoke(ctx, p.AuditActor(), in.EnvID, in.NodeID, opts, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail(ctx, "revoke the ingress node", err)
		}
		held := res.HeldHostnames
		if held == nil {
			held = []string{}
		}
		return &RevokeIngressNodeOutput{CacheControl: "no-store", Body: RevokeIngressNodeBodyOut{
			HeldHostnames: held, RotateTokenRecommended: res.RotateTokenRecommended, RevokedAt: rfc3339(res.RevokedAt),
		}}, nil
	})
}
