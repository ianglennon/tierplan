// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/access"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Problem types of the grant routes. The SPA maps each to its copy.
const (
	problemViewerExtras  = "viewer_cannot_have_extras"
	problemExtrasImplied = "extras_implied"
	problemUnknownExtra  = "unknown_extra"
	problemGrantNotFound = "grant_not_found"
)

// GrantStateBody is the state of a grant an edit started from.
type GrantStateBody struct {
	Role   string   `json:"role" enum:"viewer,operator,admin" doc:"The role the grant had when the editor opened it"`
	Extras []string `json:"extras,omitempty" maxItems:"8" doc:"The extra permissions the grant had when the editor opened it"`
}

// GrantBody is one grant: the role and extras a user or a team holds on one
// environment, or on all of them. A grant never expires (D-21).
type GrantBody struct {
	ID          string   `json:"id" doc:"The grant's ID"`
	SubjectType string   `json:"subjectType" enum:"user,team" doc:"Whether the grant is made to a user or a team"`
	SubjectID   string   `json:"subjectId" doc:"The user's or team's ID"`
	Scope       string   `json:"scope" enum:"env,all" doc:"env for one environment, all for every environment, including ones added later"`
	EnvID       string   `json:"envId,omitempty" doc:"The environment's ID, for scope env"`
	EnvName     string   `json:"envName,omitempty" doc:"The environment's name, for scope env"`
	Role        string   `json:"role" enum:"viewer,operator,admin" doc:"The role"`
	Extras      []string `json:"extras" doc:"The extra (dangerous) permissions the grant carries; only an Operator grant may carry any"`
	CreatedAt   string   `json:"createdAt" format:"date-time" doc:"When the grant was first made"`
}

// EffectiveEnvBody is a user's effective access to one environment.
type EffectiveEnvBody struct {
	EnvID     string       `json:"envId" doc:"The environment's ID"`
	EnvName   string       `json:"envName" doc:"The environment's name"`
	Role      string       `json:"role" enum:"viewer,operator,admin" doc:"The highest role any source gives"`
	Extras    []string     `json:"extras" doc:"The union of the extra permissions the sources give; an Admin holds every one"`
	Sources   []SourceBody `json:"sources" doc:"Every source, in the order instance, all, team, direct"`
	Inherited bool         `json:"inherited" doc:"True when the user reaches this environment only through a grant on every environment"`
}

// UserAccessBody is what a user may do on each environment and why.
type UserAccessBody struct {
	InstanceAdmin bool               `json:"instanceAdmin" doc:"True for an instance admin, who holds every environment with every extra permission and so has no environments listed"`
	Environments  []EffectiveEnvBody `json:"environments" doc:"The environments the user can reach, sorted by name"`
	Grants        []GrantBody        `json:"grants" doc:"The grants made to this user directly, the ones editable here"`
}

// UserAccessOutput wraps UserAccessBody for huma.
type UserAccessOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UserAccessBody
}

// ListGrantsOutput is the list of grants made to a subject.
type ListGrantsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []GrantBody
}

// PutGrantOutput is a saved grant. Status is 201 for a new grant and 200 for an edit.
type PutGrantOutput struct {
	CacheControl string `header:"Cache-Control"`
	Status       int
	Body         GrantBody
}

// GrantInputBody is a grant to save, the same for a user and a team.
type GrantInputBody struct {
	Scope    string          `json:"scope" enum:"env,all" doc:"env for one environment, all for every environment"`
	EnvID    string          `json:"envId,omitempty" maxLength:"128" doc:"The environment, for scope env"`
	Role     string          `json:"role" enum:"viewer,operator,admin" doc:"The role"`
	Extras   []string        `json:"extras,omitempty" maxItems:"8" doc:"The extra permissions; only Operator may carry any. A Viewer answers 422 viewer_cannot_have_extras, an Admin 422 extras_implied, and a name that is not an extra permission 422 unknown_extra"`
	Expected *GrantStateBody `json:"expected,omitempty" doc:"The state of the grant the editor started from. If the grant is different now, or gone, nothing is saved and the answer is 409 stale_change"`
}

// PutUserGrantInput is a grant to save for a user.
type PutUserGrantInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
	Body      GrantInputBody
}

// PutTeamGrantInput is a grant to save for a team.
type PutTeamGrantInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
	Body      GrantInputBody
}

// GrantChangeBody is a change to a grant to preview.
type GrantChangeBody struct {
	Op     string   `json:"op" enum:"delete,update" doc:"The change to preview: delete the grant, or save it with a new role and extras"`
	Role   string   `json:"role,omitempty" enum:"viewer,operator,admin" doc:"The role the grant would be saved with, for update"`
	Extras []string `json:"extras,omitempty" maxItems:"8" doc:"The extra permissions the grant would be saved with, for update"`
}

// PreviewUserGrantInput is a change to one of a user's grants to preview.
type PreviewUserGrantInput struct {
	UserID  string `path:"userId" doc:"The user's ID"`
	GrantID string `path:"grantId" doc:"The grant's ID"`
	Body    GrantChangeBody
}

// PreviewTeamGrantInput is a change to one of a team's grants to preview.
type PreviewTeamGrantInput struct {
	TeamID  string `path:"teamId" doc:"The team's ID"`
	GrantID string `path:"grantId" doc:"The grant's ID"`
	Body    GrantChangeBody
}

// DeleteUserGrantInput names one of a user's grants to remove.
type DeleteUserGrantInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	UserID    string `path:"userId" doc:"The user's ID"`
	GrantID   string `path:"grantId" doc:"The grant's ID"`
}

// DeleteTeamGrantInput names one of a team's grants to remove.
type DeleteTeamGrantInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
	GrantID   string `path:"grantId" doc:"The grant's ID"`
}

func grantBodyOf(g access.Grant) GrantBody {
	b := GrantBody{
		ID: g.ID, SubjectType: g.SubjectType, SubjectID: g.SubjectID, Scope: access.ScopeAll,
		Role: g.Role, Extras: g.Extras, CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if b.Extras == nil {
		b.Extras = []string{}
	}
	if g.EnvID != "" {
		b.Scope, b.EnvID, b.EnvName = access.ScopeEnv, g.EnvID, g.EnvName
	}
	return b
}

func grantBodiesOf(gs []access.Grant) []GrantBody {
	out := make([]GrantBody, 0, len(gs))
	for _, g := range gs {
		out = append(out, grantBodyOf(g))
	}
	return out
}

func effectiveBodyOf(r access.EnvRow) EffectiveEnvBody {
	b := EffectiveEnvBody{
		EnvID: r.EnvID, EnvName: r.EnvName, Role: r.Role, Extras: r.Extras, Inherited: r.Inherited,
		Sources: make([]SourceBody, 0, len(r.Sources)),
	}
	if b.Extras == nil {
		b.Extras = []string{}
	}
	for _, s := range r.Sources {
		b.Sources = append(b.Sources, SourceBody{Kind: string(s.Kind), TeamID: s.TeamID, TeamName: s.TeamName})
	}
	return b
}

func grantInputOf(subjectType, subjectID string, f GrantInputBody) access.GrantInput {
	in := access.GrantInput{
		SubjectType: subjectType, SubjectID: subjectID, Scope: f.Scope, EnvID: f.EnvID, Role: f.Role, Extras: f.Extras,
	}
	if f.Expected != nil {
		in.Expected = &access.GrantState{Role: f.Expected.Role, Extras: f.Expected.Extras}
	}
	return in
}

func validationProblem(location, message string) error {
	return &huma.ErrorModel{
		Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
		Status: http.StatusUnprocessableEntity, Detail: "validation failed",
		Errors: []*huma.ErrorDetail{{Location: location, Message: message}},
	}
}

// grantError answers the errors the grant routes share. subjectType says which
// 404 a missing subject is.
func grantError(d AccessDeps, subjectType, op string, err error) error {
	switch {
	case errors.Is(err, access.ErrViewerExtras):
		return httpx.Problem(http.StatusUnprocessableEntity, problemViewerExtras,
			"Viewers are read only, so they can't hold extra permissions.")
	case errors.Is(err, access.ErrExtrasImplied):
		return httpx.Problem(http.StatusUnprocessableEntity, problemExtrasImplied,
			"An Admin holds every extra permission already, so none are listed.")
	case errors.Is(err, access.ErrUnknownExtra):
		return httpx.Problem(http.StatusUnprocessableEntity, problemUnknownExtra, "That is not an extra permission.")
	case errors.Is(err, access.ErrInvalidRole):
		return validationProblem("body.role", "invalid_role")
	case errors.Is(err, access.ErrInvalidScope):
		return validationProblem("body.scope", "invalid_scope")
	case errors.Is(err, access.ErrInvalidGrantOp):
		return validationProblem("body.op", "invalid_op")
	case errors.Is(err, access.ErrEnvironmentNotFound):
		return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
	case errors.Is(err, access.ErrGrantNotFound):
		return httpx.Problem(http.StatusNotFound, problemGrantNotFound, "That access doesn't exist.")
	case errors.Is(err, access.ErrSubjectNotFound):
		if subjectType == access.SubjectTeam {
			return httpx.Problem(http.StatusNotFound, problemTeamNotFound, "That team doesn't exist.")
		}
		return httpx.Problem(http.StatusNotFound, problemUserNotFound, "That user doesn't exist.")
	case errors.Is(err, access.ErrStaleChange):
		return httpx.Problem(http.StatusConflict, problemStaleChange,
			"This changed while you were looking at it. Reload the page and try again.")
	}
	d.Log.Error(op+" failed", "error", err)
	return huma.Error500InternalServerError("internal error")
}

func registerGrants(r *httpx.Router, d AccessDeps) {
	usersManage := rbac.Requirement{Perm: rbac.PermUsersManage, Scope: rbac.ScopeGlobal}
	teamsManage := rbac.Requirement{Perm: rbac.PermTeamsManage, Scope: rbac.ScopeGlobal}
	signedIn := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return p, nil
	}

	put := func(ctx context.Context, subjectType, subjectID string, f GrantInputBody, ua string) (*PutGrantOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		g, created, err := d.Access.PutGrant(ctx, p.AuditActor(), grantInputOf(subjectType, subjectID, f),
			access.WithOrigin(httpx.ClientIP(ctx), ua))
		if err != nil {
			return nil, grantError(d, subjectType, "put grant", err)
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		return &PutGrantOutput{CacheControl: "no-store", Status: status, Body: grantBodyOf(g)}, nil
	}
	preview := func(ctx context.Context, subjectType, subjectID, grantID string, f GrantChangeBody) (*ImpactOutput, error) {
		change := access.GrantChange{Op: f.Op, Role: f.Role, Extras: f.Extras}
		if f.Op == access.OpUpdateGrant && f.Role == "" {
			return nil, validationProblem("body.role", "required for update")
		}
		rows, err := d.Access.GrantImpact(ctx, subjectType, subjectID, grantID, change)
		if err != nil {
			return nil, grantError(d, subjectType, "preview grant impact", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	}
	remove := func(ctx context.Context, subjectType, subjectID, grantID, ua string) (*ImpactOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		rows, err := d.Access.DeleteGrant(ctx, p.AuditActor(), subjectType, subjectID, grantID,
			access.WithOrigin(httpx.ClientIP(ctx), ua))
		if err != nil {
			return nil, grantError(d, subjectType, "delete grant", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "getUserAccess",
		Method:      http.MethodGet,
		Path:        "/api/users/{userId}/access",
		Summary:     "What a user may do on each environment, and why",
		Description: "The user's effective access: for each environment the user can reach, the highest role any source " +
			"gives, the union of the extra permissions and every source (instance, all, team, direct), computed by the " +
			"same evaluator that enforces access. An instance admin holds every environment, so the list is empty and " +
			"instanceAdmin is true. grants lists the user's own grants, the ones editable on the user page. An unknown " +
			"user answers 404 user_not_found.",
		Tags: []string{"grants"},
	}, usersManage, func(ctx context.Context, in *UserIDInput) (*UserAccessOutput, error) {
		grants, err := d.Access.ListGrants(ctx, access.SubjectUser, in.UserID)
		if err != nil {
			return nil, grantError(d, access.SubjectUser, "list grants", err)
		}
		admin, rows, err := d.Access.EffectiveAccess(ctx, in.UserID)
		if err != nil {
			return nil, grantError(d, access.SubjectUser, "effective access", err)
		}
		body := UserAccessBody{InstanceAdmin: admin, Environments: make([]EffectiveEnvBody, 0, len(rows)), Grants: grantBodiesOf(grants)}
		for _, row := range rows {
			body.Environments = append(body.Environments, effectiveBodyOf(row))
		}
		return &UserAccessOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "putUserGrant",
		Method:      http.MethodPut,
		Path:        "/api/users/{userId}/grants",
		Summary:     "Give a user a role on an environment",
		Description: "Gives the user Admin, Operator or Viewer on one environment (scope env, with envId) or on all of them " +
			"(scope all). A user holds at most one grant per scope: saving an existing combination edits it (200, audited " +
			"as access.updated), a new one is created (201, access.granted). Extras are accepted only on Operator: a Viewer " +
			"answers 422 viewer_cannot_have_extras, an Admin 422 extras_implied and a permission that is not an extra " +
			"422 unknown_extra. With expected set, a grant that has changed answers 409 stale_change. A grant never " +
			"expires. An unknown user answers 404 user_not_found and an unknown environment 404 environment_not_found.",
		Tags: []string{"grants"},
	}, usersManage, func(ctx context.Context, in *PutUserGrantInput) (*PutGrantOutput, error) {
		return put(ctx, access.SubjectUser, in.UserID, in.Body, in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewUserGrantImpact",
		Method:      http.MethodPost,
		Path:        "/api/users/{userId}/grants/{grantId}/impact",
		Summary:     "Preview what removing or changing a user's grant takes away",
		Description: "Works out, with the same evaluator that enforces access, who would lose which environment access if " +
			"the grant were deleted (op delete) or saved with a lower role (op update, with role and extras). The change is " +
			"applied inside a transaction that is rolled back, so nothing is written and nothing is audited. A grant " +
			"that is not the user's answers 404 grant_not_found.",
		Tags: []string{"grants"},
	}, usersManage, func(ctx context.Context, in *PreviewUserGrantInput) (*ImpactOutput, error) {
		return preview(ctx, access.SubjectUser, in.UserID, in.GrantID, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteUserGrant",
		Method:      http.MethodDelete,
		Path:        "/api/users/{userId}/grants/{grantId}",
		Summary:     "Remove a user's grant",
		Description: "Removes the grant. The user loses the access it gave on their next request and keeps every other grant, " +
			"and the automations they own that no longer qualify pause. The answer lists who lost which access, worked out " +
			"inside the same step. Audited as one access.revoked entry carrying the subject, role, scope, impact_count and " +
			"impact. A grant that is not the user's answers 404 grant_not_found.",
		Tags: []string{"grants"},
	}, usersManage, func(ctx context.Context, in *DeleteUserGrantInput) (*ImpactOutput, error) {
		return remove(ctx, access.SubjectUser, in.UserID, in.GrantID, in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listTeamGrants",
		Method:      http.MethodGet,
		Path:        "/api/teams/{teamId}/grants",
		Summary:     "List a team's grants",
		Description: "The grants made to the team: the one on every environment first, then those on single environments by " +
			"name. Every member holds what they give. An unknown team answers 404 team_not_found.",
		Tags: []string{"grants"},
	}, teamsManage, func(ctx context.Context, in *TeamIDInput) (*ListGrantsOutput, error) {
		grants, err := d.Access.ListGrants(ctx, access.SubjectTeam, in.TeamID)
		if err != nil {
			return nil, grantError(d, access.SubjectTeam, "list team grants", err)
		}
		return &ListGrantsOutput{CacheControl: "no-store", Body: grantBodiesOf(grants)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "putTeamGrant",
		Method:      http.MethodPut,
		Path:        "/api/teams/{teamId}/grants",
		Summary:     "Give a team a role on an environment",
		Description: "As putUserGrant, for a team: every member holds the role (the highest across their sources). An unknown " +
			"team answers 404 team_not_found.",
		Tags: []string{"grants"},
	}, teamsManage, func(ctx context.Context, in *PutTeamGrantInput) (*PutGrantOutput, error) {
		return put(ctx, access.SubjectTeam, in.TeamID, in.Body, in.UserAgent)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewTeamGrantImpact",
		Method:      http.MethodPost,
		Path:        "/api/teams/{teamId}/grants/{grantId}/impact",
		Summary:     "Preview what removing or changing a team's grant takes away",
		Description: "As previewUserGrantImpact, for a grant made to a team: the impact is on every member.",
		Tags:        []string{"grants"},
	}, teamsManage, func(ctx context.Context, in *PreviewTeamGrantInput) (*ImpactOutput, error) {
		return preview(ctx, access.SubjectTeam, in.TeamID, in.GrantID, in.Body)
	})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteTeamGrant",
		Method:      http.MethodDelete,
		Path:        "/api/teams/{teamId}/grants/{grantId}",
		Summary:     "Remove a team's grant",
		Description: "As deleteUserGrant, for a grant made to a team: every member loses what it gave them.",
		Tags:        []string{"grants"},
	}, teamsManage, func(ctx context.Context, in *DeleteTeamGrantInput) (*ImpactOutput, error) {
		return remove(ctx, access.SubjectTeam, in.TeamID, in.GrantID, in.UserAgent)
	})
}
