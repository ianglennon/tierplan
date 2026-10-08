// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

var (
	// ErrCurrentSession is an attempt to revoke the session making the request.
	// The row's own "Revoke" is not offered, and "sign out" is how a user ends
	// the session they are in (D-23).
	ErrCurrentSession = errors.New("access: that is the session you are using")
	// ErrSessionNotFound is a session handle that names none of the user's
	// sessions: unknown, already ended, or somebody else's. The three look alike
	// on purpose.
	ErrSessionNotFound = errors.New("access: no such session")
)

// deviceMax is how much of a user agent an audit entry keeps.
const deviceMax = 120

// SessionInfo is one session as a session list shows it. PublicID is the handle
// to revoke it by; the hash of the session ID never leaves this package.
type SessionInfo struct {
	PublicID   string
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	// Current is true for the session making the request.
	Current bool
}

// ListSessions lists a user's sessions, most recently active first. currentHash
// is the caller's own session (Principal.SessionIDHash), which is marked
// Current; nil marks none, as for an admin looking at someone else. A user who
// does not exist or has been deleted is ErrNotFound.
func (s *Service) ListSessions(ctx context.Context, userID string, currentHash []byte) ([]SessionInfo, error) {
	u, err := s.Store.RQ.GetUserByID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("access: read user: %w", err)
	}
	if u.DeletedAt.Valid {
		return nil, ErrNotFound
	}
	rows, err := s.Store.RQ.ListUserSessions(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("access: list sessions: %w", err)
	}
	out := make([]SessionInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionInfo{
			PublicID: r.PublicID, UserAgent: r.UserAgent, IP: r.Ip,
			CreatedAt: time.Unix(0, r.CreatedAt).UTC(), LastSeenAt: time.Unix(0, r.LastSeenAt).UTC(),
			Current: len(currentHash) > 0 && bytes.Equal(r.IDHash, currentHash),
		})
	}
	return out, nil
}

// cutDevice keeps the first deviceMax characters of a user agent, never
// splitting one.
func cutDevice(ua string) string {
	if utf8.RuneCountInString(ua) <= deviceMax {
		return ua
	}
	r := []rune(ua)
	return string(r[:deviceMax])
}

// RevokeSession ends one of a user's sessions, which is signed out at its next
// request. The session making the request cannot be revoked this way
// (ErrCurrentSession); a handle that is not one of the user's is
// ErrSessionNotFound. The entry names the handle and the device, never the
// session ID (D-23).
func (s *Service) RevokeSession(ctx context.Context, actor audit.Actor, userID, publicID string, currentHash []byte, opts ...Option) error {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := liveUserTx(ctx, q, userID)
	if err != nil {
		return err
	}
	row, err := q.GetUserSessionByPublicID(ctx, sqlc.GetUserSessionByPublicIDParams{UserID: userID, PublicID: sql.NullString{String: publicID, Valid: publicID != ""}})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("access: read session: %w", err)
	}
	if len(currentHash) > 0 && bytes.Equal(row.IDHash, currentHash) {
		return ErrCurrentSession
	}
	n, err := q.DeleteUserSessionByPublicID(ctx, sqlc.DeleteUserSessionByPublicIDParams{UserID: userID, PublicID: sql.NullString{String: publicID, Valid: true}})
	if err != nil {
		return fmt.Errorf("access: revoke session: %w", err)
	}
	if n == 0 {
		return ErrSessionNotFound
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionSessionRevoked, TargetType: "user", TargetID: userID,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username, "handle": publicID, "device": cutDevice(row.UserAgent)},
	}); err != nil {
		return fmt.Errorf("access: audit session revoked: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// RevokeOtherSessions ends every session of the user except the one in
// currentHash, and says how many ended. When nothing else was signed in nothing
// is written.
func (s *Service) RevokeOtherSessions(ctx context.Context, actor audit.Actor, userID string, currentHash []byte, opts ...Option) (int, error) {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := liveUserTx(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	// A nil hash would compare as NULL and match nothing; the empty slice
	// matches no row, so every session goes.
	if currentHash == nil {
		currentHash = []byte{}
	}
	n, err := q.DeleteUserSessionsExceptHash(ctx, sqlc.DeleteUserSessionsExceptHashParams{UserID: userID, IDHash: currentHash})
	if err != nil {
		return 0, fmt.Errorf("access: revoke other sessions: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.recordSessionsRevokedTx(ctx, tx, actor, u.ID, u.Username, n, o); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("access: commit: %w", err)
	}
	return int(n), nil
}

// RevokeAllSessions ends every session of a user, for an admin looking at the
// user's page, and says how many ended. A user who does not exist or has been
// deleted is ErrNotFound.
func (s *Service) RevokeAllSessions(ctx context.Context, actor audit.Actor, userID string, opts ...Option) (int, error) {
	o := optionsOf(opts)
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := liveUserTx(ctx, q, userID)
	if err != nil {
		return 0, err
	}
	n, err := q.DeleteAllUserSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("access: revoke sessions: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.recordSessionsRevokedTx(ctx, tx, actor, u.ID, u.Username, n, o); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("access: commit: %w", err)
	}
	return int(n), nil
}

func (s *Service) recordSessionsRevokedTx(ctx context.Context, tx *sql.Tx, actor audit.Actor, userID, username string, ended int64, o options) error {
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserSessionsRevoked, TargetType: "user", TargetID: userID,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": username, "ended": strconv.FormatInt(ended, 10)},
	}); err != nil {
		return fmt.Errorf("access: audit sessions revoked: %w", err)
	}
	return nil
}
