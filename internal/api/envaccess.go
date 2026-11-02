// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// problemDelegationDenied is a change an env Admin asked for through an
// environment's Access tab that is beyond what they may give (D-12).
const problemDelegationDenied = "delegation_denied"

// reasonDelegationDenied is the reason a refused delegation is audited with.
const reasonDelegationDenied = "delegation_denied"

// GrantRef is a grant as the Access tab edits it: its own ID, role and extras.
type GrantRef struct {
	ID     string   `json:"id" doc:"The grant's ID, for the edit and remove routes"`
	Role   string   `json:"role" enum:"viewer,operator,admin" doc:"The role this grant gives on the environment"`
	Extras []string `json:"extras" doc:"The extra permissions this grant gives on the environment"`
}

// EnvAccessMemberBody is a member of a team, with the role the team gives them.
type EnvAccessMemberBody struct {
	UserID      string `json:"userId" doc:"The user's ID"`
	Username    string `json:"username" doc:"The user's username"`
	DisplayName string `json:"displayName,omitempty" doc:"The user's display name, when set"`
	Role        string `json:"role" enum:"viewer,operator,admin" doc:"The role the team gives them on the environment"`
}

// EnvAccessTeamBody is a team that can use the environment.
type EnvAccessTeamBody struct {
	TeamID  string                `json:"teamId" doc:"The team's ID"`
	Name    string                `json:"name" doc:"The team's name"`
	Role    string                `json:"role" enum:"viewer,operator,admin" doc:"The highest role the team's grants give on the environment"`
	Extras  []string              `json:"extras" doc:"The union of the extra permissions the team's grants give; an Admin holds every one"`
	Sources []SourceBody          `json:"sources" doc:"Every source: direct for a grant on this environment, all for a grant on every environment"`
	Grant   *GrantRef             `json:"grant,omitempty" doc:"The team's own grant on this environment, the one an env Admin may edit or remove; absent when the team reaches the environment only through a grant on every environment"`
	Members []EnvAccessMemberBody `json:"members" doc:"The members who can sign in, with the role the team gives each"`
}

// EnvAccessPersonBody is a person who can use the environment.
type EnvAccessPersonBody struct {
	UserID        string       `json:"userId" doc:"The user's ID"`
	Username      string       `json:"username" doc:"The user's username"`
	DisplayName   string       `json:"displayName,omitempty" doc:"The user's display name, when set"`
	Role          string       `json:"role" enum:"viewer,operator,admin" doc:"The highest role any source gives"`
	Extras        []string     `json:"extras" doc:"The union of the extra permissions the sources give; an Admin holds every one"`
	Sources       []SourceBody `json:"sources" doc:"Every source, in the order instance, all, team, direct"`
	Grant         *GrantRef    `json:"grant,omitempty" doc:"The person's own grant on this environment, the one an env Admin may edit or remove; absent for an instance admin and for someone who reaches the environment only through a team or a grant on every environment"`
	InstanceAdmin bool         `json:"instanceAdmin" doc:"True for an instance admin, who holds every environment with every extra permission"`
	Self          bool         `json:"self" doc:"True for the signed-in user's own row"`
}

// EnvAccessListBody is everyone who can use an environment and why.
type EnvAccessListBody struct {
	Teams  []EnvAccessTeamBody   `json:"teams" doc:"Teams with a grant on the environment or on every environment, highest role first"`
	People []EnvAccessPersonBody `json:"people" doc:"Everyone who can use the environment, one entry each, highest role first. Instance admins are always listed."`
}

// EnvAccessListOutput wraps EnvAccessListBody for huma.
type EnvAccessListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvAccessListBody
}

// CandidateUserBody is a user who can be given access: never an email.
type CandidateUserBody struct {
	ID          string `json:"id" doc:"The user's ID"`
	Username    string `json:"username" doc:"The user's username"`
	DisplayName string `json:"displayName,omitempty" doc:"The user's display name, when set"`
}

// CandidateTeamBody is a team that can be given access.
type CandidateTeamBody struct {
	ID   string `json:"id" doc:"The team's ID"`
	Name string `json:"name" doc:"The team's name"`
}

// EnvAccessCandidatesBody are the subjects the Who list offers.
type EnvAccessCandidatesBody struct {
	Users []CandidateUserBody `json:"users" doc:"Users who can sign in, are not instance admins and have no direct grant here, by username"`
	Teams []CandidateTeamBody `json:"teams" doc:"Teams with no direct grant here, by name"`
}

// EnvAccessCandidatesOutput wraps EnvAccessCandidatesBody for huma.
type EnvAccessCandidatesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvAccessCandidatesBody
}

// EnvGrantInputBody is a grant to save on the path's environment. It has no
// scope or environment: the route is the environment, so a request cannot aim
// anywhere else (D-12).
type EnvGrantInputBody struct {
	SubjectType string          `json:"subjectType" enum:"user,team" doc:"Whether the grant is made to a user or a team"`
	SubjectID   string          `json:"subjectId" maxLength:"128" doc:"The user's or team's ID"`
	Role        string          `json:"role" enum:"viewer,operator,admin" doc:"The role"`
	Extras      []string        `json:"extras,omitempty" maxItems:"8" doc:"The extra permissions; only Operator may carry any. A Viewer answers 422 viewer_cannot_have_extras, an Admin 422 extras_implied, and a name that is not an extra permission 422 unknown_extra"`
	Expected    *GrantStateBody `json:"expected,omitempty" doc:"The state of the grant the editor started from. If the grant is different now, or gone, nothing is saved and the answer is 409 stale_change"`
}

// PutEnvAccessInput is a grant to save on an environment.
type PutEnvAccessInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	EnvID     string `path:"envId" doc:"The environment ID"`
	Body      EnvGrantInputBody
}

// PreviewEnvAccessInput is a change to one of the environment's grants to preview.
type PreviewEnvAccessInput struct {
	// UserAgent is read only to record a refusal in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	EnvID     string `path:"envId" doc:"The environment ID"`
	GrantID   string `path:"grantId" doc:"The grant's ID"`
	Body      GrantChangeBody
}

// DeleteEnvAccessInput names one of the environment's grants to remove.
type DeleteEnvAccessInput struct {
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	EnvID     string `path:"envId" doc:"The environment ID"`
	GrantID   string `path:"grantId" doc:"The grant's ID"`
}

func grantRefOf(id, role string, extras []string) *GrantRef {
	if id == "" {
		return nil
	}
	if extras == nil {
		extras = []string{}
	}
	return &GrantRef{ID: id, Role: role, Extras: extras}
}

func sourceBodiesOf(in []rbac.Source) []SourceBody {
	out := make([]SourceBody, 0, len(in))
	for _, s := range in {
		out = append(out, SourceBody{Kind: string(s.Kind), TeamID: s.TeamID, TeamName: s.TeamName})
	}
	return out
}

func envAccessBodyOf(teams []access.EnvAccessTeam, people []access.EnvAccessPerson) EnvAccessListBody {
	body := EnvAccessListBody{Teams: make([]EnvAccessTeamBody, 0, len(teams)), People: make([]EnvAccessPersonBody, 0, len(people))}
	for _, t := range teams {
		tb := EnvAccessTeamBody{
			TeamID: t.TeamID, Name: t.Name, Role: t.Role, Extras: t.Extras, Sources: sourceBodiesOf(t.Sources),
			Grant: grantRefOf(t.GrantID, t.GrantRole, t.GrantExtras), Members: make([]EnvAccessMemberBody, 0, len(t.Members)),
		}
		if tb.Extras == nil {
			tb.Extras = []string{}
		}
		for _, m := range t.Members {
			tb.Members = append(tb.Members, EnvAccessMemberBody{UserID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, Role: m.Role})
		}
		body.Teams = append(body.Teams, tb)
	}
	for _, p := range people {
		pb := EnvAccessPersonBody{
			UserID: p.UserID, Username: p.Username, DisplayName: p.DisplayName, Role: p.Role, Extras: p.Extras,
			Sources: sourceBodiesOf(p.Sources), Grant: grantRefOf(p.GrantID, p.GrantRole, p.GrantExtras),
			InstanceAdmin: p.InstanceAdmin, Self: p.Self,
		}
		if pb.Extras == nil {
			pb.Extras = []string{}
		}
		body.People = append(body.People, pb)
	}
	return body
}

func registerEnvAccess(r *httpx.Router, d AccessDeps) {
	need := rbac.Requirement{Perm: rbac.PermEnvironmentsAccess, Scope: rbac.ScopeEnvironment}

	// fail answers an error of an environment's Access routes. A refused
	// delegation is 403 delegation_denied and is audited as permission.denied
	// with that reason, so the attempt is visible to the environment's admins and
	// the instance admins; if the entry cannot be written the answer is 500, as
	// for any refusal (D-49).
	fail := func(ctx context.Context, p *rbac.Principal, route, envID, ua, subjectType, op string, err error) error {
		if !errors.Is(err, access.ErrDelegationDenied) {
			return grantError(d, subjectType, op, err)
		}
		if aerr := d.Access.Audit.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
			Reason: reasonDelegationDenied, TargetType: "route", TargetID: route, EnvID: envID,
			IP: httpx.ClientIP(ctx), UserAgent: ua,
		}); aerr != nil {
			d.Log.Error("could not audit a refused delegation; refusing with 500", "error", aerr)
			return huma.Error500InternalServerError("internal error")
		}
		return httpx.Problem(http.StatusForbidden, problemDelegationDenied, "You can't give that access.")
	}
	limit := func(envID string) access.GrantScopeLimit { return access.GrantScopeLimit{DelegatedEnv: envID} }
	signedIn := func(ctx context.Context) (*rbac.Principal, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		return p, nil
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listEnvAccess",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/access",
		Summary:     "Everyone who can use an environment, and why",
		Description: "The environment's Access tab: the teams with a grant on it or on every environment, and the people, one " +
			"entry each, with the highest role any source gives, the union of the extra permissions and every source (direct, " +
			"team, all environments, instance admin), worked out by the same evaluator that enforces access. Instance admins " +
			"are always listed. A grant carries the ID an edit or removal names, present only for a grant on this environment. " +
			"Needs environments.access on the environment (env Admin or instance admin); another environment answers " +
			"404 environment_not_found, a Viewer or Operator 403.",
		Tags: []string{"grants"},
	}, need, func(ctx context.Context, in *EnvironmentPathInput) (*EnvAccessListOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		teams, people, err := d.Access.EnvAccessList(ctx, in.EnvID, p.ID)
		if err != nil {
			return nil, grantError(d, access.SubjectUser, "list environment access", err)
		}
		return &EnvAccessListOutput{CacheControl: "no-store", Body: envAccessBodyOf(teams, people)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listEnvAccessCandidates",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/access/candidates",
		Summary:     "Who an environment's access can be given to",
		Description: "The users and teams with no direct grant on the environment, for the Who list: usernames, display names " +
			"and team names, never an email. Users who cannot sign in and instance admins are left out. Needs " +
			"environments.access on the environment.",
		Tags: []string{"grants"},
	}, need, func(ctx context.Context, in *EnvironmentPathInput) (*EnvAccessCandidatesOutput, error) {
		users, teams, err := d.Access.AccessCandidates(ctx, in.EnvID)
		if err != nil {
			return nil, grantError(d, access.SubjectUser, "list access candidates", err)
		}
		body := EnvAccessCandidatesBody{Users: make([]CandidateUserBody, 0, len(users)), Teams: make([]CandidateTeamBody, 0, len(teams))}
		for _, u := range users {
			body.Users = append(body.Users, CandidateUserBody{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName})
		}
		for _, t := range teams {
			body.Teams = append(body.Teams, CandidateTeamBody{ID: t.ID, Name: t.Name})
		}
		return &EnvAccessCandidatesOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "putEnvAccess",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/access",
		Summary:     "Give a user or team a role on this environment",
		Description: "An env Admin's (or instance admin's) way to give an existing user or team Admin, Operator or Viewer on this " +
			"environment, with the extra permissions of an Operator. The body names no scope and no environment: the route is " +
			"the environment, and a body carrying either is rejected (422). Saving an existing combination edits it (200, " +
			"access.updated), a new one is created (201, access.granted); the grantee has it on their next request. A subject " +
			"who is an instance admin answers 403 delegation_denied and is audited as permission.denied with that reason. " +
			"Extras and stale edits answer as putUserGrant does. Teams, team membership and users are not managed here. " +
			"Needs environments.access on the environment.",
		Tags: []string{"grants"},
	}, need, func(ctx context.Context, in *PutEnvAccessInput) (*PutGrantOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		gi := access.GrantInput{
			SubjectType: in.Body.SubjectType, SubjectID: in.Body.SubjectID, Scope: access.ScopeEnv, EnvID: in.EnvID,
			Role: in.Body.Role, Extras: in.Body.Extras, DelegatedEnv: in.EnvID,
		}
		if e := in.Body.Expected; e != nil {
			gi.Expected = &access.GrantState{Role: e.Role, Extras: e.Extras}
		}
		g, created, err := d.Access.PutGrant(ctx, p.AuditActor(), gi, access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, fail(ctx, p, "PUT /api/env/{envId}/access", in.EnvID, in.UserAgent, in.Body.SubjectType, "put environment access", err)
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		return &PutGrantOutput{CacheControl: "no-store", Status: status, Body: grantBodyOf(g)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewEnvAccessImpact",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/access/{grantId}/impact",
		Summary:     "Preview what removing or changing one of the environment's grants takes away",
		Description: "As previewUserGrantImpact, for a grant on this environment: who would lose which access if it were deleted " +
			"(op delete) or saved with a lower role (op update). Nothing is written. A grant that does not exist answers 404 " +
			"grant_not_found; a grant on every environment or on another environment, or an instance admin's, answers 403 " +
			"delegation_denied. Needs environments.access on the environment.",
		Tags: []string{"grants"},
	}, need, func(ctx context.Context, in *PreviewEnvAccessInput) (*ImpactOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		if in.Body.Op == access.OpUpdateGrant && in.Body.Role == "" {
			return nil, validationProblem("body.role", "required for update")
		}
		route := "POST /api/env/{envId}/access/{grantId}/impact"
		typ, id, err := d.Access.GrantSubject(ctx, in.GrantID, limit(in.EnvID))
		if err != nil {
			return nil, fail(ctx, p, route, in.EnvID, in.UserAgent, access.SubjectUser, "preview environment access impact", err)
		}
		change := access.GrantChange{Op: in.Body.Op, Role: in.Body.Role, Extras: in.Body.Extras}
		rows, err := d.Access.GrantImpact(ctx, typ, id, in.GrantID, change, limit(in.EnvID))
		if err != nil {
			return nil, fail(ctx, p, route, in.EnvID, in.UserAgent, typ, "preview environment access impact", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteEnvAccess",
		Method:      http.MethodDelete,
		Path:        "/api/env/{envId}/access/{grantId}",
		Summary:     "Remove one of the environment's grants",
		Description: "As deleteUserGrant, for a grant on this environment: the subject loses what it gave on their next request, " +
			"the automations they own that no longer qualify pause, and the answer lists who lost which access. Audited as one " +
			"access.revoked entry with this environment's ID. A grant that does not exist answers 404 grant_not_found; a grant " +
			"on every environment or on another environment, or an instance admin's, answers 403 delegation_denied and is audited " +
			"as permission.denied with that reason. An env Admin may remove their own Admin grant. Needs environments.access on " +
			"the environment.",
		Tags: []string{"grants"},
	}, need, func(ctx context.Context, in *DeleteEnvAccessInput) (*ImpactOutput, error) {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		route := "DELETE /api/env/{envId}/access/{grantId}"
		typ, id, err := d.Access.GrantSubject(ctx, in.GrantID, limit(in.EnvID))
		if err != nil {
			return nil, fail(ctx, p, route, in.EnvID, in.UserAgent, access.SubjectUser, "delete environment access", err)
		}
		rows, err := d.Access.DeleteGrant(ctx, p.AuditActor(), typ, id, in.GrantID, limit(in.EnvID),
			access.WithOrigin(httpx.ClientIP(ctx), in.UserAgent))
		if err != nil {
			return nil, fail(ctx, p, route, in.EnvID, in.UserAgent, typ, "delete environment access", err)
		}
		return &ImpactOutput{CacheControl: "no-store", Body: impactBodyOf(rows)}, nil
	})
}
