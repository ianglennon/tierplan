// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// Problem types and field codes of the team routes. The SPA maps each to its copy.
const (
	problemTeamNameTaken  = "team_name_taken"
	problemTeamNotFound   = "team_not_found"
	problemMemberNotFound = "member_not_found"
	codeInvalidTeamName   = "invalid_team_name"
)

// TeamBody is a team as the Teams table shows it.
type TeamBody struct {
	ID          string `json:"id" doc:"The team's ID"`
	Name        string `json:"name" doc:"The team's name; 1 to 64 characters, unique ignoring case"`
	MemberCount int    `json:"memberCount" doc:"How many users are in the team"`
	GrantCount  int    `json:"grantCount" doc:"How many environment assignments the team holds; 0 shows as No access"`
	CreatedAt   string `json:"createdAt" format:"date-time" doc:"When the team was created"`
	UpdatedAt   string `json:"updatedAt" format:"date-time" doc:"The version of the team a rename is based on; send it back unchanged to PATCH"`
}

// TeamMemberBody is one member of a team.
type TeamMemberBody struct {
	UserID        string `json:"userId" doc:"The user's ID"`
	Username      string `json:"username" doc:"The lower-cased username"`
	DisplayName   string `json:"displayName" doc:"The name shown for the user; empty when none was set"`
	InstanceAdmin bool   `json:"instanceAdmin" doc:"True for an instance admin. Membership never grants this"`
	Status        string `json:"status" enum:"active,disabled,locked,invite_pending,invite_expired,deleted" doc:"The user's status, judged like the users table"`
	AddedAt       string `json:"addedAt" format:"date-time" doc:"When the user joined the team"`
}

// TeamDetailBody is a team with its members.
type TeamDetailBody struct {
	TeamBody
	Members []TeamMemberBody `json:"members" doc:"The members, sorted by username"`
}

// ImpactRowBody is one place a change takes access away.
type ImpactRowBody struct {
	UserID          string `json:"userId" doc:"The user who loses access"`
	Username        string `json:"username" doc:"The user's username"`
	EnvironmentID   string `json:"environmentId" doc:"The environment's ID, or * for an assignment that applies to every environment"`
	EnvironmentName string `json:"environmentName" doc:"The environment's name, or All environments for *"`
	Before          string `json:"before" enum:"viewer,operator,admin" doc:"The role the user holds there now"`
	After           string `json:"after" enum:"none,viewer,operator,admin" doc:"The role the user would hold there once the change is applied; none when the access ends"`
}

// ImpactBody is the list of access a change takes away.
type ImpactBody struct {
	Impact []ImpactRowBody `json:"impact" doc:"Who loses which environment access, sorted by username then environment. Empty when no one does"`
}

// TeamOutput wraps TeamBody for huma.
type TeamOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TeamBody
}

// TeamDetailOutput wraps TeamDetailBody for huma.
type TeamDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         TeamDetailBody
}

// ListTeamsOutput is the Teams table.
type ListTeamsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []TeamBody
}

// ImpactOutput wraps ImpactBody for huma.
type ImpactOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImpactBody
}

// AddedBody says how many members an add put in the team.
type AddedBody struct {
	Added int `json:"added" doc:"How many users were added. Users who were already in the team are not counted and leave no audit entry"`
}

// AddedOutput wraps AddedBody for huma.
type AddedOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AddedBody
}

// TeamIDInput names one team in the path, with the caller's origin for the audit entry.
type TeamIDInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
}

// CreateTeamInput is a new team.
type CreateTeamInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      struct {
		Name string `json:"name" maxLength:"256" doc:"1 to 64 characters; not case-sensitive for uniqueness"`
	}
}

// RenameTeamInput is a rename.
type RenameTeamInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
	Body      struct {
		Name      string    `json:"name" maxLength:"256" doc:"1 to 64 characters; not case-sensitive for uniqueness"`
		UpdatedAt time.Time `json:"updatedAt" format:"date-time" doc:"The updatedAt of the team this rename is based on. If the team changed since, the answer is 409 stale_change"`
	}
}

// AddTeamMembersInput is an add.
type AddTeamMembersInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
	Body      struct {
		UserIDs []string `json:"userIds" minItems:"1" maxItems:"100" doc:"The users to add"`
	}
}

// RemoveTeamMemberInput names a member to remove.
type RemoveTeamMemberInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	TeamID    string `path:"teamId" doc:"The team's ID"`
	UserID    string `path:"userId" doc:"The member's user ID"`
}

// PreviewTeamImpactInput is a change to preview.
type PreviewTeamImpactInput struct {
	TeamID string `path:"teamId" doc:"The team's ID"`
	Body   struct {
		Op     string `json:"op" enum:"delete,remove_member" doc:"The change to preview"`
		UserID string `json:"userId,omitempty" doc:"The member to remove, for remove_member"`
	}
}

func teamBodyOf(t access.Team) TeamBody {
	return TeamBody{
		ID: t.ID, Name: t.Name, MemberCount: t.MemberCount, GrantCount: t.GrantCount,
		CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: t.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func teamDetailOf(d access.TeamDetail) TeamDetailBody {
	b := TeamDetailBody{TeamBody: teamBodyOf(d.Team), Members: make([]TeamMemberBody, 0, len(d.Members))}
	for _, m := range d.Members {
		b.Members = append(b.Members, TeamMemberBody{
			UserID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, InstanceAdmin: m.InstanceAdmin,
			Status: m.Status, AddedAt: m.AddedAt.UTC().Format(time.RFC3339),
		})
	}
	return b
}

func impactBodyOf(rows []access.ImpactRow) ImpactBody {
	b := ImpactBody{Impact: make([]ImpactRowBody, 0, len(rows))}
	for _, r := range rows {
		b.Impact = append(b.Impact, ImpactRowBody{
			UserID: r.UserID, Username: r.Username, EnvironmentID: r.EnvID, EnvironmentName: r.EnvName,
			Before: r.Before.String(), After: r.After.String(),
		})
	}
	return b
}

// teamError answers the errors the team routes share.
func teamError(d AccessDeps, op string, err error) error {
	switch {
	case errors.Is(err, access.ErrTeamNotFound):
		return httpx.Problem(http.StatusNotFound, problemTeamNotFound, "That team doesn't exist.")
	case errors.Is(err, access.ErrNotFound):
		return httpx.Problem(http.StatusNotFound, problemUserNotFound, "That user doesn't exist.")
	case errors.Is(err, access.ErrMemberNotFound):
		return httpx.Problem(http.StatusNotFound, problemMemberNotFound, "That user is not in the team.")
	case errors.Is(err, access.ErrTeamNameTaken):
		return httpx.Problem(http.StatusConflict, problemTeamNameTaken, "That team name is already taken.")
	case errors.Is(err, access.ErrStaleChange):
		return httpx.Problem(http.StatusConflict, problemStaleChange,
			"This changed while you were looking at it. Reload the page and try again.")
	case errors.Is(err, access.ErrInvalidTeamName):
		return &huma.ErrorModel{
			Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
			Status: http.StatusUnprocessableEntity, Detail: "validation failed",
			Errors: []*huma.ErrorDetail{{Location: "body.name", Message: codeInvalidTeamName}},
		}
	}
	d.Log.Error(op+" failed", "error", err)
	return huma.Error500InternalServerError("internal error")
}

func registerTeams(r *httpx.Router, d AccessDeps) {
	manage := rbac.Requirement{Perm: rbac.PermTeamsManage, Scope: rbac.ScopeGlobal}
	signedIn := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return p, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listTeams",
		Method:      http.MethodGet,
		Path:        "/api/teams",
		Summary:     "List teams",
		Description: "Every team, sorted by name ignoring case, with how many members and grants each has. " +
			"Only an instance admin may manage teams.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, _ *struct{}) (*ListTeamsOutput, error) {
		teams, err := d.Access.ListTeams(ctx)
		if err != nil {
			return nil, teamError(d, "list teams", err)
		}
		out := &ListTeamsOutput{CacheControl: "no-store", Body: make([]TeamBody, 0, len(teams))}
		for _, t := range teams {
			out.Body = append(out.Body, teamBodyOf(t))
		}
		return out, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "createTeam",
		Method:        http.MethodPost,
		Path:          "/api/teams",
		Summary:       "Create a team",
		DefaultStatus: http.StatusCreated,
		Description: "A team name is 1 to 64 characters and unique ignoring case: a name another team holds answers " +
			"409 team_name_taken and an empty or too long one answers 422 on body.name. Audited as team.created.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *CreateTeamInput) (*TeamOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		t, err := d.Access.CreateTeam(ctx, p.AuditActor(), in.Body.Name, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, teamError(d, "create team", err)
		}
		return &TeamOutput{CacheControl: "no-store", Body: teamBodyOf(t)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getTeam",
		Method:      http.MethodGet,
		Path:        "/api/teams/{teamId}",
		Summary:     "Read one team and its members",
		Description: "The team with its members sorted by username. An unknown team answers 404 team_not_found.",
		Tags:        []string{"teams"},
	}, manage, func(ctx context.Context, in *TeamIDInput) (*TeamDetailOutput, error) {
		t, err := d.Access.GetTeam(ctx, in.TeamID)
		if err != nil {
			return nil, teamError(d, "read team", err)
		}
		return &TeamDetailOutput{CacheControl: "no-store", Body: teamDetailOf(t)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "addTeamMembers",
		Method:      http.MethodPost,
		Path:        "/api/teams/{teamId}/members",
		Summary:     "Add users to a team",
		Description: "All or none: a user who does not exist or is deleted answers 404 user_not_found and nothing is added. " +
			"A user who is already in the team changes nothing and writes no audit entry. Each user added is audited " +
			"as team.member_added. Membership never makes anyone an instance admin.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *AddTeamMembersInput) (*AddedOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		n, err := d.Access.AddMembers(ctx, p.AuditActor(), in.TeamID, in.Body.UserIDs, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, teamError(d, "add team members", err)
		}
		return &AddedOutput{CacheControl: "no-store", Body: AddedBody{Added: n}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "renameTeam",
		Method:      http.MethodPatch,
		Path:        "/api/teams/{teamId}",
		Summary:     "Rename a team",
		Description: "Send the name with the updatedAt you read. If the team changed since, nothing is saved and the " +
			"answer is 409 stale_change. A name another team holds, ignoring case, answers 409 team_name_taken; an " +
			"empty or too long one answers 422 on body.name. Saving the name it already has changes nothing. " +
			"Audited as team.renamed with the old name.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *RenameTeamInput) (*TeamOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		t, err := d.Access.RenameTeam(ctx, p.AuditActor(), in.TeamID, in.Body.Name, in.Body.UpdatedAt,
			access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, teamError(d, "rename team", err)
		}
		return &TeamOutput{CacheControl: "no-store", Body: teamBodyOf(t)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteTeam",
		Method:      http.MethodDelete,
		Path:        "/api/teams/{teamId}",
		Summary:     "Delete a team",
		Description: "Removes the team, its grants and its memberships in one step, so no assignment or membership is " +
			"left pointing at it. Its members lose the access it gave them on their next request, and the automations " +
			"they own that no longer qualify pause. The answer lists who lost which access, worked out inside the same " +
			"step. Audited as one team.deleted entry carrying members, impact_count and impact: the first ten items " +
			"\"username: environment before→after\" joined by \"; \" and cut to 512 bytes. An unknown team answers " +
			"404 team_not_found.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *TeamIDInput) (*ImpactOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		rows, err := d.Access.DeleteTeam(ctx, p.AuditActor(), in.TeamID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, teamError(d, "delete team", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "removeTeamMember",
		Method:      http.MethodDelete,
		Path:        "/api/teams/{teamId}/members/{userId}",
		Summary:     "Remove a user from a team",
		Description: "The user loses the access the team gave them on their next request and keeps every other grant. " +
			"The automations they own that no longer qualify pause. The answer lists what they lost, worked out inside " +
			"the same step. Audited as one team.member_removed entry carrying impact_count and impact (first ten items " +
			"\"username: environment before→after\" joined by \"; \", cut to 512 bytes). A user who is not in the team " +
			"answers 404 member_not_found and nothing is written.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *RemoveTeamMemberInput) (*ImpactOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		rows, err := d.Access.RemoveMember(ctx, p.AuditActor(), in.TeamID, in.UserID, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, teamError(d, "remove team member", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewTeamImpact",
		Method:      http.MethodPost,
		Path:        "/api/teams/{teamId}/impact",
		Summary:     "Preview who loses access if a team changes",
		Description: "Works out, with the same evaluator that enforces access, who would lose which environment access " +
			"if the team were deleted (op delete) or a member removed (op remove_member, with userId). The change is " +
			"applied inside a transaction that is rolled back, so nothing is written and nothing is audited. " +
			"Confirming recomputes the impact when it commits. A remove_member for a user who is not in the team " +
			"answers 404 member_not_found.",
		Tags: []string{"teams"},
	}, manage, func(ctx context.Context, in *PreviewTeamImpactInput) (*ImpactOutput, error) {
		if in.Body.Op == access.OpRemoveMember && in.Body.UserID == "" {
			return nil, &huma.ErrorModel{
				Type: problemValidationFailed, Title: http.StatusText(http.StatusUnprocessableEntity),
				Status: http.StatusUnprocessableEntity, Detail: "validation failed",
				Errors: []*huma.ErrorDetail{{Location: "body.userId", Message: "required for remove_member"}},
			}
		}
		rows, err := d.Access.TeamImpact(ctx, in.TeamID, access.TeamOp{Kind: in.Body.Op, UserID: in.Body.UserID})
		if err != nil {
			return nil, teamError(d, "preview team impact", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	})
}
