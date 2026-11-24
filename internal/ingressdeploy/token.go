// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"fmt"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// ErrPaused means a restore paused the environment's ingress, so nothing is written to its swarm until an Admin resumes it.
var ErrPaused = errors.New("ingressdeploy: ingress on this environment is paused")

// ErrTokenNotEnrolled means a join token rotation on a standalone environment with ingress on was refused because the proxy
// has not enrolled yet: rotating now would supersede, in the server, the token inside the proxy's container before the
// proxy uses it, and nothing would put the new one there but a redeploy. Once the proxy has enrolled it authenticates with
// its own key and not with the token, so a rotation then changes nothing on the host.
var ErrTokenNotEnrolled = errors.New("ingressdeploy: the standalone proxy has not enrolled with the token it was given")

// CodeTokenNotEnrolled is the problem code the API gives ErrTokenNotEnrolled.
const CodeTokenNotEnrolled = "ingress_token_not_enrolled"

// ErrNoManager means no manager node's agent is connected, so the swarm cannot be written to.
var ErrNoManager = errors.New("ingressdeploy: no manager is connected")

// TokenNotStoredError means the join token was made but the swarm would not take its secret. The Admin is not given the token:
// the ensure loop replaces a current token that has no swarm secret, so it would be dead within a minute.
type TokenNotStoredError struct{ Err error }

// Error says what the swarm refused.
func (e *TokenNotStoredError) Error() string {
	return "the new join token could not be stored as a swarm secret: " + e.Err.Error()
}

// Unwrap gives the swarm's answer.
func (e *TokenNotStoredError) Unwrap() error { return e.Err }

// IssueJoinToken is an Admin's make or rotate of the environment's ingress join token. Where nothing is deployed (ingress is
// off, or the environment has no ingress state) it is ingresshub's IssueToken and nothing else.
//
// A standalone host whose ingress is on keeps no swarm secret: its proxy was given the token when its container was made, and
// once it has enrolled it authenticates with its key. So such a rotation is refused with ErrTokenNotEnrolled while no proxy node
// has enrolled, and after that it only issues the next token: nothing is written to the host and no redeploy is woken (D-26).
//
// Where ingress is on, the proxies take the token from a swarm secret and the ensure loop keeps that secret in step with the
// current token. The loop can only make a secret from a token's clear text, which exists nowhere but in IssueToken's answer, so
// a token made behind its back would have no secret and the loop would replace it within a minute, and the token the Admin
// was shown would be dead (WR-15). So the token is made, and its secret created on the swarm, under the environment's operation
// lock: the loop then finds the current token's secret already there, moves the service to it and removes the one before
// (the loop re-reads under the same lock). The rotation is refused before a token is made when it cannot be kept in step: the
// lock is held (*LockedError), a restore paused the environment (ErrPaused) or no manager is connected (ErrNoManager). A swarm
// that refuses the secret after the token was made is a *TokenNotStoredError, with no token returned.
func (s *Service) IssueJoinToken(ctx context.Context, actor audit.Actor, owner, envID, ip, ua string) (ingresshub.Token, error) {
	standalone, err := s.joinTokenStandalone(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if standalone {
		return s.issueStandaloneJoinToken(ctx, actor, owner, envID, ip, ua)
	}
	deployed, err := s.joinTokenDeployed(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if !deployed {
		return s.d.Tokens.IssueToken(ctx, actor, envID, ip, ua)
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: owner, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		return ingresshub.Token{}, &LockedError{Holder: held}
	}
	defer release()
	// Under the lock the switches and the manager are read again: an enable, a switch-off or a pause may have come in.
	if deployed, err = s.joinTokenDeployed(ctx, envID); err != nil {
		return ingresshub.Token{}, err
	}
	if !deployed {
		return s.d.Tokens.IssueToken(ctx, actor, envID, ip, ua)
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if state.Paused {
		return ingresshub.Token{}, ErrPaused
	}
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if !info.ManagerConnected {
		return ingresshub.Token{}, ErrNoManager
	}
	tok, err := s.d.Tokens.IssueToken(ctx, actor, envID, ip, ua)
	if err != nil {
		return ingresshub.Token{}, err
	}
	name := SecretName(envID, tok.Generation, tok.ID)
	if err := s.d.Engine.WriteOnManager(ctx, envID, func(c *dockerx.Client) error {
		_, err := c.SecretCreate(ctx, SecretSpec(name, []byte(tok.Token)))
		return err
	}); err != nil {
		s.log().Warn("the swarm would not take a new join token's secret", "environment", envID, "error", err)
		return ingresshub.Token{}, &TokenNotStoredError{Err: fmt.Errorf("create the secret: %w", err)}
	}
	// The loop moves the service to the new secret soon, and removes the earlier one once the update has settled.
	s.Wake()
	return tok, nil
}

// joinTokenDeployed is whether the environment is a swarm whose ingress is on, so that its join token is kept as a swarm
// secret. An environment that does not exist is not: IssueToken answers for it.
func (s *Service) joinTokenDeployed(ctx context.Context, envID string) (bool, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	switch {
	case errors.Is(err, envs.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case !info.Swarm:
		return false, nil
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return false, err
	}
	return state.Enabled, nil
}

// joinTokenStandalone is whether the environment is a standalone host whose ingress is on: the kind of environment whose
// rotation is ruled by enrolment and not by a swarm secret. joinTokenDeployed keeps its meaning for a swarm only.
func (s *Service) joinTokenStandalone(ctx context.Context, envID string) (bool, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	switch {
	case errors.Is(err, envs.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case info.Swarm:
		return false, nil
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return false, err
	}
	return state.Enabled, nil
}

// issueStandaloneJoinToken is the rotation on a standalone host whose ingress is on. The operation lock is taken so a
// redeploy, an enable or a switch-off is not under way, and everything is read again under it. A restore's pause refuses it
// (ErrPaused), and so does a proxy that has not enrolled (ErrTokenNotEnrolled), with no token made. Otherwise the next token
// is issued and nothing else happens: the host is not written to and no redeploy is asked for.
func (s *Service) issueStandaloneJoinToken(ctx context.Context, actor audit.Actor, owner, envID, ip, ua string) (ingresshub.Token, error) {
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: owner, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		return ingresshub.Token{}, &LockedError{Holder: held}
	}
	defer release()
	standalone, err := s.joinTokenStandalone(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if !standalone {
		return s.d.Tokens.IssueToken(ctx, actor, envID, ip, ua)
	}
	state, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if state.Paused {
		return ingresshub.Token{}, ErrPaused
	}
	active, err := s.hasActiveNode(ctx, envID)
	if err != nil {
		return ingresshub.Token{}, err
	}
	if !active {
		return ingresshub.Token{}, ErrTokenNotEnrolled
	}
	return s.d.Tokens.IssueToken(ctx, actor, envID, ip, ua)
}
