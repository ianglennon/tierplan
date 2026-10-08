// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// TeamMemberRole is a member of a team that holds access to an environment, with
// the role the team gives them there.
type TeamMemberRole struct {
	UserID, Username, DisplayName string
	Role                          string
}

// EnvAccessTeam is a team's row on an environment's Access tab: the highest role
// the team's grants give on the environment, the union of their extras and every
// source (a grant on this environment alone, or on every environment). GrantID is
// the team's direct grant on this environment, empty when it holds only a grant
// on every environment, which an env Admin cannot touch; GrantRole and
// GrantExtras are that direct grant's own role and extras, what an edit starts
// from.
type EnvAccessTeam struct {
	TeamID, Name string
	Role         string
	Extras       []string
	Sources      []rbac.Source
	GrantID      string
	GrantRole    string
	GrantExtras  []string
	Members      []TeamMemberRole
}

// EnvAccessPerson is a person's row on an environment's Access tab: one entry
// per person, with the highest role any source gives, the union of extras and
// every source, as rbac.Access.For would answer for them. GrantID is their own
// direct grant on this environment, empty when there is none (and for an
// instance admin, whose access is not an environment grant's to change);
// GrantRole and GrantExtras are that grant's own role and extras. Self marks the
// signed-in user.
type EnvAccessPerson struct {
	UserID, Username, DisplayName string
	Role                          string
	Extras                        []string
	Sources                       []rbac.Source
	GrantID                       string
	GrantRole                     string
	GrantExtras                   []string
	InstanceAdmin, Self           bool
}

// roleRank orders roles for display, highest first.
func roleRank(id string) int {
	r, _ := rbac.ParseRole(id)
	return int(r)
}

// teamFold is a team's grants on the environment while rows are being folded.
type teamFold struct {
	team    EnvAccessTeam
	acc     rbac.EnvAccess
	members map[string]TeamMemberRole
}

// personFold is a person's access while rows are being folded.
type personFold struct {
	p   EnvAccessPerson
	acc rbac.EnvAccess
}

// extrasJSON reads the JSON array of extras a query returns.
func extrasJSON(id, raw string) ([]rbac.Perm, error) {
	var out []rbac.Perm
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("access: assignment %s extras: %w", id, err)
	}
	return out, nil
}

// EnvAccessList is everyone who can use one environment and why (D-06, D-25):
// the teams that hold a grant on it or on every environment, and the people,
// one entry per person, with the highest role, the union of extras and every
// source. It is folded with the evaluator's own merge rules, so it cannot
// disagree with enforcement. Instance admins are always listed (Admin, every
// extra, the one source "instance"); a disabled or deleted user holds nothing
// and is not listed. Both lists are sorted by role, highest first, then name.
// callerID marks the signed-in user's own row. An environment that does not
// exist is ErrEnvironmentNotFound.
func (s *Service) EnvAccessList(ctx context.Context, envID, callerID string) ([]EnvAccessTeam, []EnvAccessPerson, error) {
	if _, err := s.Store.RQ.GetEnvironment(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrEnvironmentNotFound
	} else if err != nil {
		return nil, nil, fmt.Errorf("access: read environment: %w", err)
	}
	rows, err := s.Store.RQ.ListEnvAccessRows(ctx, sql.NullString{String: envID, Valid: true})
	if err != nil {
		return nil, nil, fmt.Errorf("access: list environment access: %w", err)
	}
	admins, err := s.Store.RQ.ListEnvAccessInstanceAdmins(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("access: list instance admins: %w", err)
	}

	teams := map[string]*teamFold{}
	people := map[string]*personFold{}
	adminIDs := map[string]bool{}
	allExtras := adminExtras()
	for _, a := range admins {
		adminIDs[a.ID] = true
		people[a.ID] = &personFold{p: EnvAccessPerson{
			UserID: a.ID, Username: a.Username, DisplayName: a.DisplayName, Role: rbac.RoleAdmin.ID(),
			Extras: slices.Clone(allExtras), Sources: []rbac.Source{{Kind: rbac.SourceInstanceAdmin}},
			GrantExtras: []string{}, InstanceAdmin: true, Self: a.ID == callerID,
		}}
	}
	person := func(id, username, display string) *personFold {
		pf, ok := people[id]
		if !ok {
			pf = &personFold{p: EnvAccessPerson{
				UserID: id, Username: username, DisplayName: display, GrantExtras: []string{}, Self: id == callerID,
			}}
			people[id] = pf
		}
		return pf
	}

	for _, r := range rows {
		role, ok := rbac.ParseRole(r.RoleID)
		if !ok {
			continue // a role this binary does not know grants nothing
		}
		extras, err := extrasJSON(r.ID, r.Extras)
		if err != nil {
			return nil, nil, err
		}
		onEnv := r.EnvID.Valid
		switch r.PrincipalType {
		case SubjectUser:
			if r.SubjectActive == 0 || adminIDs[r.PrincipalID] {
				continue // holds nothing, or holds everything already
			}
			src := rbac.Source{Kind: rbac.SourceAllEnvironments}
			if onEnv {
				src = rbac.Source{Kind: rbac.SourceDirect}
			}
			pf := person(r.PrincipalID, r.SubjectName, r.SubjectDisplay)
			pf.acc = pf.acc.Merge(rbac.EnvAccess{Role: role, Extras: extras, Sources: []rbac.Source{src}})
			if onEnv {
				pf.p.GrantID, pf.p.GrantRole = r.ID, role.ID()
				pf.p.GrantExtras = extrasStrings(extras)
			}
		case SubjectTeam:
			tf, ok := teams[r.PrincipalID]
			if !ok {
				tf = &teamFold{
					team:    EnvAccessTeam{TeamID: r.PrincipalID, Name: r.SubjectName, GrantExtras: []string{}},
					members: map[string]TeamMemberRole{},
				}
				teams[r.PrincipalID] = tf
			}
			src := rbac.Source{Kind: rbac.SourceAllEnvironments}
			if onEnv {
				src = rbac.Source{Kind: rbac.SourceDirect}
				tf.team.GrantID, tf.team.GrantRole = r.ID, role.ID()
				tf.team.GrantExtras = extrasStrings(extras)
			}
			tf.acc = tf.acc.Merge(rbac.EnvAccess{Role: role, Extras: extras, Sources: []rbac.Source{src}})
			// A member who can sign in holds the team's access.
			if r.MemberID != "" && r.MemberActive != 0 {
				tsrc := rbac.Source{Kind: rbac.SourceTeam, TeamID: r.PrincipalID, TeamName: r.SubjectName}
				if !adminIDs[r.MemberID] {
					pf := person(r.MemberID, r.MemberUsername, r.MemberDisplay)
					pf.acc = pf.acc.Merge(rbac.EnvAccess{Role: role, Extras: extras, Sources: []rbac.Source{tsrc}})
				}
				tf.members[r.MemberID] = TeamMemberRole{UserID: r.MemberID, Username: r.MemberUsername, DisplayName: r.MemberDisplay}
			}
		}
	}

	outTeams := make([]EnvAccessTeam, 0, len(teams))
	for _, tf := range teams {
		t := tf.team
		t.Role = tf.acc.Role.ID()
		t.Extras = shownExtras(tf.acc, allExtras)
		t.Sources = tf.acc.Sources
		t.Members = make([]TeamMemberRole, 0, len(tf.members))
		for _, m := range tf.members {
			m.Role = t.Role
			t.Members = append(t.Members, m)
		}
		slices.SortFunc(t.Members, func(a, b TeamMemberRole) int { return compareNames(a.Username, a.UserID, b.Username, b.UserID) })
		outTeams = append(outTeams, t)
	}
	slices.SortFunc(outTeams, func(a, b EnvAccessTeam) int {
		if c := roleRank(b.Role) - roleRank(a.Role); c != 0 {
			return c
		}
		return compareNames(a.Name, a.TeamID, b.Name, b.TeamID)
	})

	outPeople := make([]EnvAccessPerson, 0, len(people))
	for _, pf := range people {
		p := pf.p
		if !p.InstanceAdmin {
			p.Role = pf.acc.Role.ID()
			p.Extras = shownExtras(pf.acc, allExtras)
			p.Sources = pf.acc.Sources
		}
		outPeople = append(outPeople, p)
	}
	slices.SortFunc(outPeople, func(a, b EnvAccessPerson) int {
		if c := roleRank(b.Role) - roleRank(a.Role); c != 0 {
			return c
		}
		return compareNames(a.Username, a.UserID, b.Username, b.UserID)
	})
	return outTeams, outPeople, nil
}

// shownExtras is the extras a row shows: the union of what its sources give, and
// every extra for an Admin, who holds them without listing any (D-16).
func shownExtras(acc rbac.EnvAccess, all []string) []string {
	if acc.Role == rbac.RoleAdmin {
		return slices.Clone(all)
	}
	return extrasStrings(acc.Extras)
}

func extrasStrings(in []rbac.Perm) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, string(p))
	}
	slices.Sort(out)
	return out
}

// compareNames orders by name without regard to case, then by ID.
func compareNames(a, aID, b, bID string) int {
	if c := strings.Compare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return strings.Compare(aID, bID)
}

// CandidateUser is a user an environment's Access tab can give access to. It
// carries no email: an env Admin is offered a list to assign from (D-12), not a
// directory.
type CandidateUser struct{ ID, Username, DisplayName string }

// CandidateTeam is a team an environment's Access tab can give access to.
type CandidateTeam struct{ ID, Name string }

// AccessCandidates are the users and teams that have no direct grant on one
// environment and can be given one: the Who list. Users who cannot sign in
// (disabled or deleted) and instance admins (who hold every environment) are
// left out. Both lists are sorted by name without regard to case.
func (s *Service) AccessCandidates(ctx context.Context, envID string) ([]CandidateUser, []CandidateTeam, error) {
	if _, err := s.Store.RQ.GetEnvironment(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrEnvironmentNotFound
	} else if err != nil {
		return nil, nil, fmt.Errorf("access: read environment: %w", err)
	}
	rows, err := s.Store.RQ.ListAccessCandidates(ctx, sql.NullString{String: envID, Valid: true})
	if err != nil {
		return nil, nil, fmt.Errorf("access: list candidates: %w", err)
	}
	users, teams := []CandidateUser{}, []CandidateTeam{}
	for _, r := range rows {
		switch r.Kind {
		case SubjectUser:
			users = append(users, CandidateUser{ID: r.ID, Username: r.Name, DisplayName: r.DisplayName})
		case SubjectTeam:
			teams = append(teams, CandidateTeam{ID: r.ID, Name: r.Name})
		}
	}
	return users, teams, nil
}
