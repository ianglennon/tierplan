// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
)

// UnknownAgentsDeps are the dependencies of the unknown-agents route.
type UnknownAgentsDeps struct {
	// Unknown is the list of agents refused unknown_agent. Nil means there are none.
	Unknown *agents.UnknownAgents
	// Store names the environments.
	Store *store.Store
	// Now is the clock the list is aged by. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// UnknownAgentBody is one agent TierPlan does not know.
type UnknownAgentBody struct {
	EnvID       string `json:"envId" doc:"The environment whose CA signed the agent's certificate"`
	EnvName     string `json:"envName" doc:"That environment's name"`
	Node        string `json:"node" doc:"The agent as its certificate names it (the certificate's common name)"`
	IP          string `json:"ip" doc:"The address of the newest attempt"`
	LastAttempt string `json:"lastAttempt" format:"date-time" doc:"When the agent last tried to connect"`
}

// UnknownAgentsBody lists the agents TierPlan does not know.
type UnknownAgentsBody struct {
	Items []UnknownAgentBody `json:"items" doc:"Newest attempt first; empty when there are none"`
}

// UnknownAgentsOutput wraps UnknownAgentsBody for huma.
type UnknownAgentsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UnknownAgentsBody
}

// unknownAgentItems is the list as the route and the warning give it: the agents
// refused unknown_agent in the last 24 hours, for environments that still exist.
func unknownAgentItems(ctx context.Context, d UnknownAgentsDeps) ([]UnknownAgentBody, error) {
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	items := []UnknownAgentBody{}
	if d.Unknown == nil {
		return items, nil
	}
	names := map[string]string{}
	for _, a := range d.Unknown.List(now) {
		name, ok := names[a.EnvID]
		if !ok {
			env, err := d.Store.RQ.GetEnvironment(ctx, a.EnvID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				names[a.EnvID] = ""
				continue
			case err != nil:
				return nil, err
			}
			name = env.Name
			names[a.EnvID] = name
		}
		if name == "" {
			continue
		}
		items = append(items, UnknownAgentBody{
			EnvID: a.EnvID, EnvName: name, Node: a.Node, IP: a.IP, LastAttempt: rfc3339(a.LastAttempt),
		})
	}
	return items, nil
}

// RegisterUnknownAgents registers the list of agents TierPlan does not know, which
// an instance admin reads after a restore (D-31).
func RegisterUnknownAgents(r *httpx.Router, d UnknownAgentsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listUnknownAgents",
		Method:      http.MethodGet,
		Path:        "/api/environments/unknown-agents",
		Summary:     "List the agents TierPlan does not know",
		Description: "After a restore from a backup, agents that were installed after the backup was taken present a certificate the " +
			"environment issued but are not in the restored database, and are refused unknown_agent. This lists the ones that tried in " +
			"the last 24 hours, newest first, from memory: it is empty after a restart until they try again, and an agent drops off " +
			"24 hours after it stops trying. Each is reinstalled with its environment's install command. Needs environments.token " +
			"(instance admin).",
		Tags: []string{"environments"},
	}, rbac.Require(rbac.PermEnvironmentsToken), func(ctx context.Context, _ *struct{}) (*UnknownAgentsOutput, error) {
		items, err := unknownAgentItems(ctx, d)
		if err != nil {
			d.Log.Error("list the unknown agents failed", "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return &UnknownAgentsOutput{CacheControl: "no-store", Body: UnknownAgentsBody{Items: items}}, nil
	})
}
