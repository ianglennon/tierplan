// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// StackRevisionImageChange is one service whose image differs from the previous good revision's.
type StackRevisionImageChange struct {
	Service string `json:"service" doc:"The service's name in the compose file"`
	From    string `json:"from" doc:"The reference the previous good revision ran, tag@sha256; empty for a service that revision did not have"`
	To      string `json:"to" doc:"The reference this revision pinned, tag@sha256"`
}

// StackRevisionItem is one revision of a stack.
type StackRevisionItem struct {
	Seq           int64                      `json:"seq" doc:"The revision number, unique per stack and never reused"`
	Origin        string                     `json:"origin" enum:"deploy,update,rollback,adopted" doc:"How the revision came to be: a deploy, an Update images deploy, a roll back that copied an earlier revision, or the first revision of an adopted stack"`
	RestoredFrom  int64                      `json:"restoredFrom" doc:"The number of the revision a roll back copied; 0 for any other revision"`
	Outcome       string                     `json:"outcome" enum:"pending,good,failed" doc:"pending while its deploy runs, then good or failed"`
	CreatedBy     string                     `json:"createdBy" doc:"Who started the deploy the revision was taken for"`
	CreatedAt     string                     `json:"createdAt" format:"date-time" doc:"When the revision was taken, RFC 3339 UTC"`
	FailedService string                     `json:"failedService" doc:"The service a failed deploy stopped at; empty when it did not fail or the failure names none"`
	Code          string                     `json:"code" doc:"The code of the failure; empty when the revision did not fail"`
	Reason        string                     `json:"reason" doc:"The sentence of the failure code; empty when there is no code or Bayplan has no sentence for it"`
	Deployed      bool                       `json:"deployed" doc:"True for the newest good revision, the one the stack stands at"`
	Images        []StackRevisionImageChange `json:"images" doc:"The services whose image differs from the next older good revision's, by service name. Empty for a revision that changed none, and for one with no older good revision to compare with"`
	Pinned        int                        `json:"pinned" doc:"How many images the revision pinned, when it has no older good revision to compare with (the first or an adopted one); 0 otherwise"`
	AdoptedFrom   string                     `json:"adoptedFrom" doc:"The swarm namespace or Compose project an adopted stack was taken over from; empty for any other revision"`
}

// StackRevisionListBody is the History tab of a stack.
type StackRevisionListBody struct {
	Items  []StackRevisionItem `json:"items" doc:"The revisions of the stack, newest first by revision number. A pruned revision is gone from the list"`
	Capped bool                `json:"capped" doc:"True when the stack has more revisions than the 500 the list holds; the oldest are left out"`
}

// StackRevisionListOutput wraps StackRevisionListBody for huma.
type StackRevisionListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         StackRevisionListBody
}

func registerStackHistory(r *httpx.Router, d StacksDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listStackRevisions", Method: http.MethodGet, Path: "/api/env/{envId}/stacks/{stackId}/revisions",
		Summary: "The revisions of a stack",
		Description: "Every deploy of the stack is a revision. They come newest first by revision number, with how each came to be (origin), the revision a roll back copied, " +
			"the result (outcome, and for a failed one the service and the sentence of its failure code), who started it and when, " +
			"and the images that changed against the next older good revision. The newest good revision is marked deployed. " +
			"Only references, codes and names are listed: no file, variable or rendered text. A stack with no revision answers an empty list. " +
			"A stack ID of another environment is 404 stack_not_found. Needs stacks.read (Viewer).",
		Tags: []string{"stacks"},
	}, rbac.Requirement{Perm: rbac.PermStacksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *StackPathInput) (*StackRevisionListOutput, error) {
			return listStackRevisions(ctx, d, in)
		})
}

func listStackRevisions(ctx context.Context, d StacksDeps, in *StackPathInput) (*StackRevisionListOutput, error) {
	repo := d.repo()
	// The stack is read by its environment and ID first, so a stack of another environment is not found.
	st, err := repo.Stack(ctx, in.EnvID, in.StackID)
	if err != nil {
		return nil, stackLookupProblem(d, in.EnvID, err)
	}
	rows, capped, err := repo.ListRevisions(ctx, st.ID)
	if err != nil {
		return nil, stackInternal(d, "list revisions", in.EnvID, err)
	}
	body := StackRevisionListBody{Items: make([]StackRevisionItem, 0, len(rows)), Capped: capped}
	for _, row := range rows {
		item := StackRevisionItem{
			Seq: row.Seq, Origin: row.Origin, RestoredFrom: row.RestoredFrom, Outcome: row.Outcome, CreatedBy: row.CreatedBy,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339), FailedService: row.FailedService, Code: row.Code,
			Reason: stackops.CodeMessage(row.Code), Deployed: row.Deployed, Pinned: row.Pinned,
			Images: make([]StackRevisionImageChange, 0, len(row.Images)),
		}
		for _, c := range row.Images {
			// A swarm revision pins by swarm service name, {stack}_{service}; the list names the service as the
			// compose file does.
			service := c.Service
			if st.Target == stacks.TargetSwarm {
				service = strings.TrimPrefix(service, st.Name+"_")
			}
			item.Images = append(item.Images, StackRevisionImageChange{Service: service, From: c.From, To: c.To})
		}
		if row.Origin == stackops.OriginAdopted {
			item.AdoptedFrom = st.Source.Ref
		}
		body.Items = append(body.Items, item)
	}
	return &StackRevisionListOutput{CacheControl: "no-store", Body: body}, nil
}
