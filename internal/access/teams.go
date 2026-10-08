// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// maxTeamNameChars is the longest team name (D-11).
const maxTeamNameChars = 64

var (
	// ErrTeamNameTaken is a team name another team holds, ignoring case.
	ErrTeamNameTaken = errors.New("access: that team name is taken")
	// ErrInvalidTeamName is a team name that is empty or longer than 64 characters.
	ErrInvalidTeamName = errors.New("access: a team name is 1 to 64 characters")
	// ErrMemberNotFound is a user who is not in the team, for a removal.
	ErrMemberNotFound = errors.New("access: that user is not in the team")
	// ErrTeamNotFound is a team that does not exist. It is an ErrNotFound too.
	ErrTeamNotFound = fmt.Errorf("%w: team", ErrNotFound)
)

// Team is a team as the Teams table shows it.
type Team struct {
	ID, Name    string
	MemberCount int
	// GrantCount is how many environment assignments the team holds.
	GrantCount int
	CreatedAt  time.Time
	// UpdatedAt is the version a rename is checked against.
	UpdatedAt time.Time
}

// TeamMember is one member of a team as the Members card shows them.
type TeamMember struct {
	UserID, Username, DisplayName string
	InstanceAdmin                 bool
	// Status is a user status (Status...), judged like the users table does.
	Status  string
	AddedAt time.Time
}

// TeamDetail is a team with its members, sorted by username.
type TeamDetail struct {
	Team
	Members []TeamMember
}

// TeamRef names a team a user belongs to.
type TeamRef struct{ ID, Name string }

func teamOf(r sqlc.GetTeamWithCountsRow) Team {
	return Team{
		ID: r.ID, Name: r.Name, MemberCount: int(r.MemberCount), GrantCount: int(r.GrantCount),
		CreatedAt: time.Unix(0, r.CreatedAt).UTC(), UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(),
	}
}

// cleanTeamName trims a name and checks it is 1 to 64 characters with no
// control characters.
func cleanTeamName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(name)
	if n < 1 || n > maxTeamNameChars || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", ErrInvalidTeamName
	}
	return name, nil
}

// CreateTeam adds a team, in one transaction with its audit entry. Names are
// unique ignoring case. Only an instance admin may call it (teams.manage, D-11);
// the caller has checked that.
func (s *Service) CreateTeam(ctx context.Context, actor audit.Actor, name string, opts ...Option) (Team, error) {
	o := optionsOf(opts)
	name, err := cleanTeamName(name)
	if err != nil {
		return Team{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Team{}, fmt.Errorf("access: generate team id: %w", err)
	}
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return Team{}, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	switch _, err := q.GetTeamByNameCI(ctx, name); {
	case err == nil:
		return Team{}, ErrTeamNameTaken
	case !errors.Is(err, sql.ErrNoRows):
		return Team{}, fmt.Errorf("access: look up team name: %w", err)
	}
	now := s.now().UnixNano()
	if err := q.InsertTeam(ctx, sqlc.InsertTeamParams{ID: id.String(), Name: name, CreatedAt: now, UpdatedAt: now}); err != nil {
		return Team{}, fmt.Errorf("access: insert team: %w", err)
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionTeamCreated, TargetType: "team", TargetID: id.String(),
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"name": name},
	}); err != nil {
		return Team{}, fmt.Errorf("access: audit team created: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Team{}, fmt.Errorf("access: commit: %w", err)
	}
	t := time.Unix(0, now).UTC()
	return Team{ID: id.String(), Name: name, CreatedAt: t, UpdatedAt: t}, nil
}

// ListTeams returns every team with its member and grant counts, sorted by
// name ignoring case.
func (s *Service) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := s.Store.RQ.ListTeamsWithCounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("access: list teams: %w", err)
	}
	out := make([]Team, 0, len(rows))
	for _, r := range rows {
		out = append(out, teamOf(sqlc.GetTeamWithCountsRow(r)))
	}
	return out, nil
}

// GetTeam returns one team with its members. An unknown team is ErrTeamNotFound.
func (s *Service) GetTeam(ctx context.Context, id string) (TeamDetail, error) {
	r, err := s.Store.RQ.GetTeamWithCounts(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamDetail{}, ErrTeamNotFound
	}
	if err != nil {
		return TeamDetail{}, fmt.Errorf("access: read team: %w", err)
	}
	ms, err := s.Store.RQ.ListTeamMembers(ctx, id)
	if err != nil {
		return TeamDetail{}, fmt.Errorf("access: list team members: %w", err)
	}
	now := s.now()
	d := TeamDetail{Team: teamOf(r), Members: make([]TeamMember, 0, len(ms))}
	for _, m := range ms {
		u := sqlc.User{PasswordHash: m.PasswordHash, Disabled: m.Disabled, LockedUntil: m.LockedUntil, DeletedAt: m.DeletedAt}
		d.Members = append(d.Members, TeamMember{
			UserID: m.ID, Username: m.Username, DisplayName: m.DisplayName, InstanceAdmin: m.InstanceAdmin != 0,
			Status:  Status(u, now, m.LinkExpiresAt.Valid && m.LinkExpiresAt.Int64 > now.UnixNano()),
			AddedAt: time.Unix(0, m.AddedAt).UTC(),
		})
	}
	return d, nil
}

// AddMembers puts users in a team, all or none, in one transaction. Each must
// exist and not be a tombstone, else the error is ErrNotFound naming the user
// and nothing is added; an unknown team is ErrTeamNotFound. A user who is
// already a member is skipped without a trace. It returns how many were added,
// and writes one team.member_added entry for each. The automations the new
// members own are rechecked in the same transaction.
func (s *Service) AddMembers(ctx context.Context, actor audit.Actor, teamID string, userIDs []string, opts ...Option) (int, error) {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	t, err := q.GetTeam(ctx, teamID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrTeamNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("access: read team: %w", err)
	}
	var added []string
	seen := map[string]bool{}
	for _, uid := range userIDs {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		u, err := liveUserTx(ctx, q, uid)
		if errors.Is(err, ErrNotFound) {
			return 0, fmt.Errorf("%w: user %s", ErrNotFound, uid)
		}
		if err != nil {
			return 0, err
		}
		n, err := q.InsertTeamMember(ctx, sqlc.InsertTeamMemberParams{TeamID: teamID, UserID: uid, CreatedAt: s.now().UnixNano()})
		if err != nil {
			return 0, fmt.Errorf("access: add team member: %w", err)
		}
		if n == 0 {
			continue // already a member: no change, no entry
		}
		added = append(added, uid)
		if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionTeamMemberAdded, TargetType: "team", TargetID: teamID,
			Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
			Details: map[string]string{"team": t.Name, "username": u.Username},
		}); err != nil {
			return 0, fmt.Errorf("access: audit team member added: %w", err)
		}
	}
	if _, err := s.RecheckOwnedTx(ctx, tx, q, added, actor); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("access: commit: %w", err)
	}
	return len(added), nil
}
