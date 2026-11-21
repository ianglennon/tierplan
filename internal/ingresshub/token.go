// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// ErrEnvironmentNotFound means there is no environment with that ID.
var ErrEnvironmentNotFound = errors.New("ingresshub: no such environment")

// Token is a newly issued ingress join token. Token is the only place the token ever exists in clear: only
// its hash is stored. Rotated is true when it replaced an earlier token.
type Token struct {
	// ID is the ID of the token's row. It is not secret: it names the token without revealing it, so the
	// swarm secret that carries the token can be named after it.
	ID         string
	Token      string
	Generation int
	Rotated    bool
}

// newToken makes a join token for envID: 32 random bytes, base64url without padding, behind the prefix and
// the environment ID. It returns the whole token and the secret part, which is what is hashed.
func newToken(envID string) (token, secret string, err error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate a join token: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(b)
	return ingresswire.TokenPrefix + envID + "_" + secret, secret, nil
}

// ParseToken splits an ingress join token into its environment ID and secret. The environment ID is a UUID
// and holds no underscore, and the secret may, so the first underscore after the prefix ends the ID. An
// agent join token (tpjt_) is not accepted here, as an ingress token is not accepted by agents.ParseToken.
func ParseToken(token string) (envID, secret string, ok bool) {
	rest, found := strings.CutPrefix(token, ingresswire.TokenPrefix)
	if !found {
		return "", "", false
	}
	envID, secret, found = strings.Cut(rest, "_")
	if !found {
		return "", "", false
	}
	if _, err := uuid.Parse(envID); err != nil {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != secretBytes {
		return "", "", false
	}
	return envID, secret, true
}

// IssueToken makes the environment's ingress join token and returns it, once. The first call writes
// generation 1 and audits ingress.join_token_created; a later call supersedes the current token, writes the
// next generation and audits ingress.join_token_rotated. Either is one transaction. Any environment kind
// may hold a token: whether a node can enrol is decided at enrolment. Nodes already enrolled are not
// touched, since they authenticate with their key.
func (s *Service) IssueToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) (Token, error) {
	now := s.now().UTC()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return Token{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if _, err := q.IngressEnvironmentTopology(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrEnvironmentNotFound
	} else if err != nil {
		return Token{}, fmt.Errorf("look up the environment: %w", err)
	}
	next, err := q.NextIngressTokenGeneration(ctx, envID)
	if err != nil {
		return Token{}, fmt.Errorf("number the join token: %w", err)
	}
	tokenUUID, err := uuid.NewV7()
	if err != nil {
		return Token{}, fmt.Errorf("generate token id: %w", err)
	}
	token, secret, err := newToken(envID)
	if err != nil {
		return Token{}, err
	}
	hash := sha256.Sum256([]byte(secret))
	ts := now.UnixNano()
	superseded, err := q.SupersedeCurrentIngressToken(ctx, sqlc.SupersedeCurrentIngressTokenParams{SupersededAt: ts, EnvID: envID})
	if err != nil {
		return Token{}, fmt.Errorf("supersede the join token: %w", err)
	}
	if err := q.InsertIngressJoinToken(ctx, sqlc.InsertIngressJoinTokenParams{
		ID: tokenUUID.String(), EnvID: envID, TokenHash: hash[:], Generation: next, CreatedAt: ts, CreatedBy: actor.ID,
	}); err != nil {
		return Token{}, fmt.Errorf("insert the join token: %w", err)
	}
	rotated := next > 1 || superseded > 0
	action := audit.ActionIngressJoinTokenCreated
	if rotated {
		action = audit.ActionIngressJoinTokenRotated
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: action, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"generation": strconv.FormatInt(next, 10)},
	}); err != nil {
		return Token{}, fmt.Errorf("audit join token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Token{}, fmt.Errorf("commit: %w", err)
	}
	return Token{ID: tokenUUID.String(), Token: token, Generation: int(next), Rotated: rotated}, nil
}

// DisableToken stops the current join token enrolling anyone, until the next IssueToken makes a new one.
// Nodes already enrolled are not affected. It changes nothing, and audits nothing, when the environment has no
// token or the current one is disabled already.
func (s *Service) DisableToken(ctx context.Context, actor audit.Actor, envID, ip, ua string) error {
	now := s.now().UTC()
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	if _, err := q.IngressEnvironmentTopology(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return ErrEnvironmentNotFound
	} else if err != nil {
		return fmt.Errorf("look up the environment: %w", err)
	}
	cur, err := q.IngressTokenStatus(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("load the join token: %w", err)
	}
	rows, err := q.DisableCurrentIngressToken(ctx, sqlc.DisableCurrentIngressTokenParams{DisabledAt: now.UnixNano(), EnvID: envID})
	if err != nil {
		return fmt.Errorf("disable the join token: %w", err)
	}
	if rows == 0 {
		return nil
	}
	if err := s.aud.RecordTx(ctx, tx, audit.Entry{
		Actor: actor, Action: audit.ActionIngressJoinTokenDisabled, TargetType: targetEnvironment, TargetID: envID,
		EnvID: envID, Result: audit.ResultSuccess, IP: ip, UserAgent: ua,
		Details: map[string]string{"generation": strconv.FormatInt(cur.Generation, 10)},
	}); err != nil {
		return fmt.Errorf("audit join token disabled: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// TokenStatus says which join token of an environment is current: the generation, when and by whom it was made
// and whether it is disabled. It never returns the token, which exists in clear only in IssueToken's result.
type TokenStatus struct {
	// Exists is false when the environment has never had a token.
	Exists bool
	// ID is the ID of the current token's row, the one Token.ID gave when it was issued.
	ID         string
	Generation int
	CreatedAt  time.Time
	CreatedBy  string
	Disabled   bool
}

// TokenStatus reports which join token is current and whether it is disabled.
func (s *Service) TokenStatus(ctx context.Context, envID string) (TokenStatus, error) {
	if _, err := s.st.RQ.IngressEnvironmentTopology(ctx, envID); errors.Is(err, sql.ErrNoRows) {
		return TokenStatus{}, ErrEnvironmentNotFound
	} else if err != nil {
		return TokenStatus{}, fmt.Errorf("look up the environment: %w", err)
	}
	row, err := s.st.RQ.IngressTokenStatus(ctx, envID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TokenStatus{}, nil
	case err != nil:
		return TokenStatus{}, fmt.Errorf("load the join token: %w", err)
	}
	return TokenStatus{
		Exists: true, ID: row.ID, Generation: int(row.Generation), CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
		CreatedBy: row.CreatedBy, Disabled: row.DisabledAt != 0,
	}, nil
}
