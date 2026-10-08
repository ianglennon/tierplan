// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
)

// Link kinds (user_links.kind).
const (
	// LinkInvite lets a new user choose their first password.
	LinkInvite = "invite"
	// LinkReset lets an existing user choose a new one.
	LinkReset = "reset"
)

const (
	tokenBytes = 32 // 256 bits
	// tokenChars is the length of tokenBytes in unpadded base64url.
	tokenChars = 43
)

// LinkInfo is what a link says about itself to the page that opens it.
type LinkInfo struct {
	Kind     string
	Username string
}

// newToken returns a fresh random token and the hash that is stored for it.
func newToken() (raw string, hash []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("access: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	return raw, hashToken(raw), nil
}

func hashToken(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

// tokenHash is the hash of a submitted token, or false when the token cannot be
// one this service issued (wrong length or alphabet), so garbage never reaches
// the database.
func tokenHash(raw string) ([]byte, bool) {
	if len(raw) != tokenChars {
		return nil, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) != tokenBytes {
		return nil, false
	}
	return hashToken(raw), true
}

// issueLinkTx replaces the user's link with a new one of kind inside the
// caller's write transaction. Only the hash is stored.
func (s *Service) issueLinkTx(ctx context.Context, q *sqlc.Queries, userID, kind, createdBy string) (Link, error) {
	raw, hash, err := newToken()
	if err != nil {
		return Link{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Link{}, fmt.Errorf("access: generate link id: %w", err)
	}
	now := s.now()
	expires := now.Add(s.InviteTTL)
	if err := q.DeleteUserLinks(ctx, userID); err != nil {
		return Link{}, fmt.Errorf("access: replace link: %w", err)
	}
	if err := q.InsertUserLink(ctx, sqlc.InsertUserLinkParams{
		ID: id.String(), UserID: userID, Kind: kind, TokenHash: hash,
		CreatedAt: now.UnixNano(), ExpiresAt: expires.UnixNano(), CreatedBy: createdBy,
	}); err != nil {
		return Link{}, fmt.Errorf("access: store link: %w", err)
	}
	return Link{Token: raw, ExpiresAt: expires}, nil
}

// LookupLink says what a token is for, so the page can greet the user. Every
// token that cannot be used is ErrInvalidLink.
func (s *Service) LookupLink(ctx context.Context, token string) (LinkInfo, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return LinkInfo{}, ErrInvalidLink
	}
	row, err := s.Store.RQ.GetUserLinkByHash(ctx, sqlc.GetUserLinkByHashParams{TokenHash: hash, ExpiresAt: s.now().UnixNano()})
	if errors.Is(err, sql.ErrNoRows) {
		return LinkInfo{}, ErrInvalidLink
	}
	if err != nil {
		return LinkInfo{}, fmt.Errorf("access: look up link: %w", err)
	}
	if row.Disabled != 0 || row.DeletedAt.Valid {
		return LinkInfo{}, ErrInvalidLink
	}
	return LinkInfo{Kind: row.Kind, Username: row.Username}, nil
}

// AcceptLink spends the link and sets the user's password. It does not sign
// anyone in: the user goes to the sign-in page afterwards. The password is
// checked against the policy (auth.ErrPasswordTooShort, auth.ErrPasswordCommon)
// only after the link is known to be good, so a bad link never says anything
// about passwords. Of any number of concurrent calls with one token, exactly one
// returns nil.
func (s *Service) AcceptLink(ctx context.Context, token, password, actorIP, userAgent string) error {
	hash, ok := tokenHash(token)
	if !ok {
		return ErrInvalidLink
	}
	info, err := s.LookupLink(ctx, token)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password, info.Username); err != nil {
		return err
	}
	// Hashing is slow and memory-hungry: do it before taking the write connection.
	pwHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("access: hash password: %w", err)
	}

	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	now := s.now()
	spent, err := q.ConsumeUserLink(ctx, sqlc.ConsumeUserLinkParams{TokenHash: hash, ExpiresAt: now.UnixNano()})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidLink // someone else used it between the look-up and here
	}
	if err != nil {
		return fmt.Errorf("access: consume link: %w", err)
	}
	// The user may have been disabled or deleted since the link was made. The
	// link is not spent by a refusal: the rollback puts it back, and look-up
	// refuses it anyway.
	u, err := q.GetUserByID(ctx, spent.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidLink
	}
	if err != nil {
		return fmt.Errorf("access: read user: %w", err)
	}
	if u.Disabled != 0 || u.DeletedAt.Valid {
		return ErrInvalidLink
	}
	n := now.UnixNano()
	if err := q.UpdatePassword(ctx, sqlc.UpdatePasswordParams{PasswordHash: pwHash, PasswordChangedAt: n, UpdatedAt: n, ID: u.ID}); err != nil {
		return fmt.Errorf("access: set password: %w", err)
	}
	if err := q.ResetLoginFailures(ctx, u.ID); err != nil {
		return fmt.Errorf("access: clear failures: %w", err)
	}
	action := audit.ActionUserInviteAccepted
	if spent.Kind == LinkReset {
		action = audit.ActionUserPasswordResetCompleted
		// A reset ends every session the user had: whoever held the old password
		// must sign in again.
		if err := q.DeleteUserSessionsExcept(ctx, sqlc.DeleteUserSessionsExceptParams{UserID: u.ID, IDHash: []byte{}}); err != nil {
			return fmt.Errorf("access: end sessions: %w", err)
		}
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		// The holder of the link is the user: it is the only proof of who they
		// are that this step has.
		Actor:      audit.Actor{Type: audit.ActorUser, ID: u.ID, Name: u.Username},
		Action:     action,
		TargetType: "user",
		TargetID:   u.ID,
		Result:     audit.ResultSuccess,
		IP:         actorIP,
		UserAgent:  userAgent,
		Details:    map[string]string{"username": u.Username, "link_kind": spent.Kind},
	}); err != nil {
		return fmt.Errorf("access: audit link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("access: commit: %w", err)
	}
	return nil
}

// Option adjusts one call of a service method.
type Option func(*options)

type options struct{ ip, userAgent string }

// WithOrigin records where the request came from in the audit entry: the
// admin's address and user agent.
func WithOrigin(ip, userAgent string) Option {
	return func(o *options) { o.ip, o.userAgent = ip, userAgent }
}

// RegenerateInvite replaces an invited user's link with a new one and returns
// the user's name and the link, in one transaction with its audit entry
// (D-22a). The old link stops working: a user holds one link at a time. A user
// who has already set a password is ErrNotInvited, and one who does not exist or
// is deleted is ErrNotFound; neither changes anything. actor is the admin; the
// caller has already checked users.manage. The audit entry holds the username
// and the expiry, never the token.
func (s *Service) RegenerateInvite(ctx context.Context, actor audit.Actor, userID string, opts ...Option) (string, Link, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	tx, err := s.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return "", Link{}, fmt.Errorf("access: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.Store.Q.WithTx(tx)

	u, err := q.GetUserByID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Link{}, ErrNotFound
	}
	if err != nil {
		return "", Link{}, fmt.Errorf("access: read user: %w", err)
	}
	if u.DeletedAt.Valid {
		return "", Link{}, ErrNotFound
	}
	if u.PasswordHash != "" {
		return "", Link{}, ErrNotInvited
	}
	link, err := s.issueLinkTx(ctx, q, u.ID, LinkInvite, actor.ID)
	if err != nil {
		return "", Link{}, err
	}
	if err := s.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionUserInviteRegenerated, TargetType: "user", TargetID: u.ID,
		Result: audit.ResultSuccess, IP: o.ip, UserAgent: o.userAgent,
		Details: map[string]string{"username": u.Username, "expires_at": link.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00")},
	}); err != nil {
		return "", Link{}, fmt.Errorf("access: audit invite regenerated: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", Link{}, fmt.Errorf("access: commit: %w", err)
	}
	return u.Username, link, nil
}
