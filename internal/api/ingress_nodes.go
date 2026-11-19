// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The ingress node routes (Phase 22, PRX-01): an Admin makes the environment's ingress join token, reads which one is
// current, lists the nodes that enrolled with it and revokes one of them. The token appears once, in the answer that
// makes it, and every answer that carries or describes a token is no-store.

const (
	problemIngressNodeNotFound       = "ingress_node_not_found"
	problemIngressNodeAlreadyRevoked = "ingress_node_already_revoked"

	// The values of IngressNodeBody.Sync.
	ingressSyncCurrent      = "current"
	ingressSyncPending      = "pending"
	ingressSyncIncompatible = "incompatible"
	ingressSyncUnknown      = "unknown"
)

// IngressNodesDeps are the dependencies of the ingress node routes.
type IngressNodesDeps struct {
	// Service holds the join tokens and the nodes. Nil answers 503 ingress_unavailable, so the routes are always registered.
	Service *ingresshub.Service
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
	fail := func(what string, err error) error {
		switch {
		case errors.Is(err, ingresshub.ErrEnvironmentNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		case errors.Is(err, ingresshub.ErrNodeNotFound):
			return httpx.Problem(http.StatusNotFound, problemIngressNodeNotFound, "No such ingress node in this environment.")
		case errors.Is(err, ingresshub.ErrAlreadyRevoked):
			return httpx.Problem(http.StatusConflict, problemIngressNodeAlreadyRevoked, "That node was revoked while you were looking at it.")
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
			"their own key. 404 environment_not_found. Needs ingress.manage on the environment (its Admin). Audited as ingress.join_token_created or " +
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
		tok, err := d.Service.IssueToken(ctx, p.AuditActor(), in.EnvID, httpx.ClientIP(ctx), in.UserAgent)
		if err != nil {
			return nil, fail("issue the join token", err)
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
			return nil, fail("ingress join token status", err)
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
			return nil, fail("disable the ingress join token", err)
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
			return nil, fail("look up the environment", err)
		}
		nodes, err := d.Service.ListNodes(ctx, in.EnvID)
		if err != nil {
			return nil, fail("list the ingress nodes", err)
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
}
