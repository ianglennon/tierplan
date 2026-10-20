// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// AllStacksGroup is the stacks of one environment in the answer of the All stacks list.
type AllStacksGroup struct {
	EnvID   string      `json:"envId" doc:"The environment"`
	EnvName string      `json:"envName" doc:"Its name"`
	Target  string      `json:"target" enum:"swarm,standalone" doc:"Where the environment's stacks deploy: swarm or standalone"`
	Stacks  []StackItem `json:"stacks" doc:"The environment's stacks by name"`
}

// FailedEnvironmentBody names an environment whose stacks could not be read, so the list can say so
// in place of its group while the others render.
type FailedEnvironmentBody struct {
	EnvID   string `json:"envId" doc:"The environment"`
	EnvName string `json:"envName" doc:"Its name"`
}

// AllStacksBody is the stacks of every environment the caller may read them on.
type AllStacksBody struct {
	Groups             []AllStacksGroup        `json:"groups" doc:"One group for each environment that has stacks the caller may read, by environment name. An environment the caller holds no stacks.read on is absent"`
	FailedEnvironments []FailedEnvironmentBody `json:"failedEnvironments" doc:"Environments the caller may read stacks on whose stacks could not be read just now"`
}

// AllStacksInput is the query of GET /api/stacks.
type AllStacksInput struct {
	Env string `query:"env" maxLength:"2048" doc:"Only these environments, by ID, comma separated. An ID the caller cannot read stacks on is ignored, as one that does not exist"`
}

// AllStacksOutput wraps AllStacksBody for huma.
type AllStacksOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AllStacksBody
}

// canIn says whether the grants listed for each environment include perm for id, directly or
// through an assignment that covers every environment.
func canIn(perEnv map[string][]rbac.Perm, id string, perm rbac.Perm) bool {
	return slices.Contains(perEnv[id], perm) || slices.Contains(perEnv[rbac.AllEnvironments], perm)
}

// splitIDs reads a comma list, once each and in the order written.
func splitIDs(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out
}

func registerAllStacks(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listAllStacks", Method: http.MethodGet, Path: "/api/stacks",
		Summary: "The stacks Bayplan manages on every environment the caller may read them on",
		Description: "One group for each environment that has stacks, by environment name, each with its stacks by name, " +
			"their state and when they were last deployed. An environment the caller does not hold stacks.read on is left out, " +
			"and so is its name: the list never reveals what the caller cannot open. An environment whose stacks could not be read " +
			"is named in failedEnvironments while the others are answered. env chooses environments by ID.",
		Tags: []string{"stacks"},
	}, rbac.SelfService, func(ctx context.Context, in *AllStacksInput) (*AllStacksOutput, error) {
		return listAllStacks(ctx, d, in)
	})
}

func listAllStacks(ctx context.Context, d StacksDeps, in *AllStacksInput) (*AllStacksOutput, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	_, perEnv, err := d.Checker.EffectivePermissions(ctx, p)
	if err != nil {
		return nil, stackInternal(d, "effective permissions", "", err)
	}
	all, err := d.Store.RQ.ListEnvironments(ctx)
	if err != nil {
		return nil, stackInternal(d, "list environments", "", err)
	}
	chosen := splitIDs(in.Env)
	body := AllStacksBody{Groups: []AllStacksGroup{}, FailedEnvironments: []FailedEnvironmentBody{}}
	for _, e := range all {
		if e.Enabled == 0 || !canIn(perEnv, e.ID, rbac.PermStacksRead) {
			continue
		}
		if len(chosen) > 0 && !slices.Contains(chosen, e.ID) {
			continue
		}
		items, err := d.stacksOf(ctx, e.ID)
		if err != nil {
			d.Log.Error("the stacks of an environment could not be read for the all-stacks list", "environment", e.ID, "error", err)
			body.FailedEnvironments = append(body.FailedEnvironments, FailedEnvironmentBody{EnvID: e.ID, EnvName: e.Name})
			continue
		}
		if len(items) == 0 {
			continue
		}
		body.Groups = append(body.Groups, AllStacksGroup{EnvID: e.ID, EnvName: e.Name, Target: items[0].Target, Stacks: items})
	}
	slices.SortStableFunc(body.Groups, func(a, b AllStacksGroup) int { return compareText(a.EnvName, b.EnvName) })
	slices.SortStableFunc(body.FailedEnvironments, func(a, b FailedEnvironmentBody) int { return compareText(a.EnvName, b.EnvName) })
	return &AllStacksOutput{CacheControl: "no-store", Body: body}, nil
}

// stacksOf builds the list entries of one environment's stacks, by name: the rows Bayplan stores for
// them and, for a stack that has been deployed, what the engine runs, from the caches the lists use.
func (d StacksDeps) stacksOf(ctx context.Context, envID string) ([]StackItem, error) {
	if d.ReadEnv != nil {
		if err := d.ReadEnv(ctx, envID); err != nil {
			return nil, err
		}
	}
	repo := d.repo()
	list, err := repo.Stacks(ctx, envID)
	if err != nil {
		return nil, err
	}
	latest, err := repo.LatestDeploys(ctx, envID)
	if err != nil {
		return nil, err
	}
	done, err := repo.LastDeployed(ctx, envID)
	if err != nil {
		return nil, err
	}
	drafts, err := repo.DraftSaves(ctx, envID)
	if err != nil {
		return nil, err
	}
	reads := newStackReads(ctx, d, envID)
	items := make([]StackItem, 0, len(list))
	for _, st := range list {
		l, hasL := latest[st.ID]
		g, hasG := done[st.ID]
		items = append(items, stackItemOf(reads, st, l, hasL, g, hasG, drafts[st.ID].Pending))
	}
	return items, nil
}

// compareText orders two names case-insensitively, then exactly, so the order never depends on how
// the rows arrived.
func compareText(a, b string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
