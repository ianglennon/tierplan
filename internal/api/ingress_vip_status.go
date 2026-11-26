// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The virtual IP status and move history routes (Phase 26, VIP-03, D-22, D-23): an Admin reads who holds the address and how
// every candidate stands, and the history of the address's moves. Nothing here carries the VRRP key (PROH-26-03).

const (
	problemVIPNotEnabled = "vip_not_enabled"

	vipNotEnabledText = "This environment has no virtual IP."

	// vipMovesDefaultLimit and vipMovesMaxLimit are the page size of the move history when none is asked for and its largest.
	vipMovesDefaultLimit = 50
	vipMovesMaxLimit     = 100

	problemVIPBadCursor = "vip_bad_cursor"
)

// IngressVIPHolderBody says which nodes hold the address.
type IngressVIPHolderBody struct {
	State string   `json:"state" enum:"none,one,many" doc:"none: no node reports MASTER. one: exactly one does, the healthy case. many: more than one does, which is a problem"`
	Nodes []string `json:"nodes" doc:"The swarm node IDs of the nodes reporting MASTER, ordered by host name then ID; empty for none"`
}

// IngressVIPCandidateBody is one node that can hold the address and how it stands.
type IngressVIPCandidateBody struct {
	NodeID     string `json:"nodeId" doc:"The swarm node ID"`
	Hostname   string `json:"hostname" doc:"The node's host name, empty when it is not known"`
	VRRPState  string `json:"vrrpState" enum:"MASTER,BACKUP,FAULT,STOP," doc:"The state the node's keepalived last reported; empty when it has not reported any"`
	ProxyReady bool   `json:"proxyReady" doc:"True when the node's proxy is enrolled, healthy and on the current bundle"`
	TaskState  string `json:"taskState" doc:"The state of the node's newest keepalived task in the swarm, empty when it has none or the task list could not be read"`
	LastFault  string `json:"lastFault" doc:"Why the node's keepalived last reported a fault, empty when it has not"`
	LastHeard  string `json:"lastHeard,omitempty" format:"date-time" doc:"When TierPlan last read the node's keepalived log without a failure; absent when it never has"`
}

// IngressVIPManagerBody is the privileged-services opt-in of one manager.
type IngressVIPManagerBody struct {
	NodeID      string `json:"nodeId" doc:"The manager's swarm node ID"`
	Hostname    string `json:"hostname" doc:"The host name the swarm lists for the manager"`
	State       string `json:"state" enum:"allows,refuses,unknown" doc:"allows: the manager lets services run privileged. refuses: it does not, so keepalived cannot run on it. unknown: its agent did not answer"`
	LocalSocket bool   `json:"localSocket" doc:"True for a manager reached only through TierPlan's own Docker socket, which counts as allowing"`
}

// IngressVIPForeignRouterBody is the warning that another router uses the router ID.
type IngressVIPForeignRouterBody struct {
	Detected bool   `json:"detected" doc:"True when any node reports another router using the router ID on the network"`
	Sender   string `json:"sender,omitempty" doc:"The address that router sent from, when one was reported"`
}

// IngressVIPEnsureBody is the last pass that keeps the virtual IP's service as wanted.
type IngressVIPEnsureBody struct {
	At    string `json:"at,omitempty" format:"date-time" doc:"When the last pass ended; absent when none has run"`
	Error string `json:"error,omitempty" doc:"The class of its failure; absent when it succeeded"`
}

// IngressVIPBody is an environment's virtual IP status. It carries addresses, names and states, never the key.
type IngressVIPBody struct {
	Address           string                      `json:"address" doc:"The virtual address with the prefix length of its network, for example 192.168.10.250/24"`
	RouterID          int                         `json:"routerId" doc:"The VRRP router ID"`
	RouterDerived     bool                        `json:"routerDerived" doc:"True when TierPlan chose the router ID, false when an Admin did"`
	KeyGeneration     int                         `json:"keyGeneration" doc:"How many times the VRRP key has been made; the key itself is never shown"`
	Enabled           bool                        `json:"enabled" doc:"True while the virtual IP is switched on"`
	Holder            IngressVIPHolderBody        `json:"holder" doc:"Which nodes hold the address"`
	HolderlessSince   string                      `json:"holderlessSince,omitempty" format:"date-time" doc:"When the address last lost its holder; absent while it is held, or before TierPlan has read the environment"`
	Candidates        []IngressVIPCandidateBody   `json:"candidates" doc:"The nodes that run a keepalived task or have reported a state, ordered by host name then node ID"`
	Managers          []IngressVIPManagerBody     `json:"managers" doc:"The privileged-services opt-in of every manager"`
	ManagersReadable  bool                        `json:"managersReadable" doc:"False when the managers could not be read, which leaves managers empty"`
	ForeignRouter     IngressVIPForeignRouterBody `json:"foreignRouter" doc:"The warning that another router on the network uses this router ID"`
	ClockSkewNodes    []string                    `json:"clockSkewNodes" doc:"The swarm node IDs of the nodes whose clock is too far from the others for the authentication window"`
	AuthWindowSeconds int                         `json:"authWindowSeconds" doc:"How far, in seconds, an advert's authentication may differ from a node's clock before the node refuses it"`
	LastEnsure        IngressVIPEnsureBody        `json:"lastEnsure" doc:"The last pass that keeps the keepalived service as wanted"`
}

// IngressVIPOutput wraps IngressVIPBody for huma.
type IngressVIPOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressVIPBody
}

// ingressVIPBody maps the deployer's status to the body.
func ingressVIPBody(st ingressdeploy.VIPStatus) IngressVIPBody {
	body := IngressVIPBody{
		RouterID: st.RouterID, RouterDerived: st.RouterDerived, KeyGeneration: st.KeyGeneration, Enabled: st.Enabled,
		Holder:            IngressVIPHolderBody{State: st.Holder.State, Nodes: append([]string{}, st.Holder.NodeIDs...)},
		HolderlessSince:   rfc3339(st.HolderlessSince),
		Candidates:        make([]IngressVIPCandidateBody, 0, len(st.Candidates)),
		Managers:          make([]IngressVIPManagerBody, 0, len(st.Managers)),
		ManagersReadable:  st.ManagersReadable,
		ForeignRouter:     IngressVIPForeignRouterBody{Detected: st.ForeignRouter},
		ClockSkewNodes:    append([]string{}, st.ClockSkewNodes...),
		AuthWindowSeconds: int(ingressbundle.VIPAuthWindow.Seconds()),
		LastEnsure:        IngressVIPEnsureBody{At: rfc3339(st.EnsureAt), Error: st.EnsureError},
	}
	if st.Address.IsValid() {
		body.Address = st.Address.String()
	}
	if st.ForeignSender.IsValid() {
		body.ForeignRouter.Sender = st.ForeignSender.String()
	}
	for _, c := range st.Candidates {
		body.Candidates = append(body.Candidates, IngressVIPCandidateBody{
			NodeID: c.NodeID, Hostname: c.Hostname, VRRPState: c.VRRPState, ProxyReady: c.ProxyReady,
			TaskState: c.TaskState, LastFault: c.LastFault, LastHeard: rfc3339(c.LastHeard),
		})
	}
	for _, m := range st.Managers {
		body.Managers = append(body.Managers, IngressVIPManagerBody{NodeID: m.NodeID, Hostname: m.Hostname, State: m.State, LocalSocket: m.LocalSocket})
	}
	return body
}

// RegisterIngressVIP registers the virtual IP's status, its move history and its write routes (ingress_vip.go). All need
// ingress.manage on the environment (its Admin) and answer with Cache-Control no-store. They share the deployer's dependencies: without a Service they answer
// 503 ingress_unavailable.
func RegisterIngressVIP(r *httpx.Router, d IngressDeployDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress is not available on this server.")
	}
	fail := func(what string, err error) error {
		d.Log.Error("virtual IP request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressVIP",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/vip",
		Summary:     "Read the virtual IP status of a swarm",
		Description: "Says which address the swarm's ingress answers on and which node holds it (none, one or many), when it last lost its holder, how each node that can hold it stands " +
			"(its VRRP state, whether its proxy is ready, its keepalived task, its last fault and when TierPlan last heard from it), whether each manager allows privileged services, " +
			"whether another router uses the router ID, which nodes have clock trouble, the authentication window in seconds and the last pass that kept the service as wanted. " +
			"A swarm with no virtual IP answers 404 vip_not_enabled. It never carries the VRRP key. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressVIPOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		st, err := d.Service.VIPStatus(ctx, in.EnvID)
		if errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			return nil, httpx.Problem(http.StatusNotFound, problemVIPNotEnabled, vipNotEnabledText)
		}
		if err != nil {
			return nil, fail("read the virtual IP status", err)
		}
		return &IngressVIPOutput{CacheControl: "no-store", Body: ingressVIPBody(st)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listIngressVIPMoves",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/vip/moves",
		Summary:     "List the moves of a swarm's virtual IP",
		Description: "Pages the history of the address moving from one node to another, newest first by time and, for moves at the same instant, newest recorded first. " +
			"limit is 1 to 100 (50 by default); before is the next of the previous page, an opaque cursor, and a value that is not one is 422 vip_bad_cursor. " +
			"next is absent on the last page. A swarm with no moves answers an empty list. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressVIPMovesInput) (*IngressVIPMovesOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		var before *ingressstore.VIPMoveCursor
		if in.Before != "" {
			c, ok := decodeVIPMoveCursor(in.Before)
			if !ok {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemVIPBadCursor, "before is not a cursor this server gave.")
			}
			before = &c
		}
		limit := in.Limit
		if limit < 1 {
			limit = vipMovesDefaultLimit
		}
		limit = min(limit, vipMovesMaxLimit)
		moves, next, err := d.Service.VIPMoves(ctx, in.EnvID, before, limit)
		if err != nil {
			return nil, fail("list the virtual IP moves", err)
		}
		body := IngressVIPMovesBody{Items: make([]IngressVIPMoveBody, 0, len(moves))}
		for _, m := range moves {
			body.Items = append(body.Items, IngressVIPMoveBody{
				At: rfc3339(m.At), FromNodeID: m.FromNodeID, FromNode: m.FromNodeName, ToNodeID: m.ToNodeID, ToNode: m.ToNodeName, Reason: m.Reason,
			})
		}
		if next != nil {
			body.Next = encodeVIPMoveCursor(*next)
		}
		return &IngressVIPMovesOutput{CacheControl: "no-store", Body: body}, nil
	})

	registerIngressVIPWrites(r, d)
}

// IngressVIPMovesInput is the path and the paging of the move history.
type IngressVIPMovesInput struct {
	EnvironmentPathInput
	Before string `query:"before" maxLength:"64" doc:"The next of the previous page; leave it out for the newest moves"`
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"50" doc:"Moves per page, 1 to 100; 50 by default"`
}

// IngressVIPMoveBody is one move of the address from one node to another.
type IngressVIPMoveBody struct {
	At         string `json:"at" format:"date-time" doc:"When the move happened, as the new holder's log says"`
	FromNodeID string `json:"fromNodeId" doc:"The swarm node ID of the previous holder; empty when the address had no holder before"`
	FromNode   string `json:"fromNode" doc:"The previous holder's host name; empty when the address had no holder before"`
	ToNodeID   string `json:"toNodeId" doc:"The swarm node ID of the new holder; empty when the address has no holder after"`
	ToNode     string `json:"toNode" doc:"The new holder's host name; empty when the address has no holder after"`
	Reason     string `json:"reason" enum:"holder_failed,probe_failing,drain_handoff,disabled,unknown" doc:"Why it moved. holder_failed: the previous holder went silent or its task is gone. probe_failing: its proxy was not ready. drain_handoff: TierPlan handed the address over before draining the node. disabled: the virtual IP was switched off. unknown: none of these explains it"`
}

// IngressVIPMovesBody is one page of the move history.
type IngressVIPMovesBody struct {
	Items []IngressVIPMoveBody `json:"items" doc:"The moves of this page, newest first; never null"`
	Next  string               `json:"next,omitempty" doc:"Pass this as before to read the next, older page; absent on the last page"`
}

// IngressVIPMovesOutput wraps IngressVIPMovesBody for huma.
type IngressVIPMovesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressVIPMovesBody
}

// encodeVIPMoveCursor is the opaque form of a cursor: the base64url, unpadded, of "<at-nanos>.<id>".
func encodeVIPMoveCursor(c ingressstore.VIPMoveCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.At.UnixNano(), 10) + "." + strconv.FormatInt(c.ID, 10)))
}

// decodeVIPMoveCursor reads a cursor strictly: unpadded base64url, then two canonical decimal integers around one dot, the
// instant above zero and the id not below it. Anything else, including a number that has a sign, a leading zero or a space, is
// refused, so the one form a cursor has is the one encodeVIPMoveCursor writes.
func decodeVIPMoveCursor(s string) (ingressstore.VIPMoveCursor, bool) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		return ingressstore.VIPMoveCursor{}, false
	}
	at, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return ingressstore.VIPMoveCursor{}, false
	}
	nanos, err := strconv.ParseInt(at, 10, 64)
	if err != nil || nanos <= 0 || strconv.FormatInt(nanos, 10) != at {
		return ingressstore.VIPMoveCursor{}, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != id {
		return ingressstore.VIPMoveCursor{}, false
	}
	return ingressstore.VIPMoveCursor{At: time.Unix(0, nanos).UTC(), ID: n}, true
}
