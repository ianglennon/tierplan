// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"

	"github.com/ianglennon/tierplan/internal/audit"
)

// When a change of the access log reaches the proxies.
const (
	// AppliesRolling: the ensure loop rolls the proxy service, which restarts each node's proxy in turn.
	AppliesRolling = "rolling"
	// AppliesWhenEnabled: the value is stored and applies when ingress is switched on.
	AppliesWhenEnabled = "when_enabled"
	// AppliesWhenResumed: the value is stored and applies when an Admin resumes the environment a restore paused.
	AppliesWhenResumed = "when_resumed"
)

// AccessLogsResult is the answer to a change of the access log.
type AccessLogsResult struct {
	// AccessLogs is the value now stored.
	AccessLogs bool
	// Applies is when it reaches the proxies: one of the Applies* values.
	Applies string
}

// SetAccessLogs stores whether the proxy writes an access log (off by default) and asks the ensure loop for a pass, which
// rolls the proxy service with the variable added or taken away (D-30, D-31). A change is audited as
// ingress.access_logs_changed in the transaction that stores it; asking for the value already stored changes nothing,
// records nothing and wakes nobody. The audit row carries the state only. On an environment whose ingress is off the
// value is stored and nothing is written to the swarm.
func (s *Service) SetAccessLogs(ctx context.Context, actor audit.Actor, envID string, on bool) (AccessLogsResult, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return AccessLogsResult{}, err
	}
	if !info.Swarm {
		return AccessLogsResult{}, ErrStandaloneUnavailable
	}
	state := "off"
	if on {
		state = "on"
	}
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressAccessLogsChanged, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess, Details: map[string]string{"state": state},
		})
	}
	changed, err := s.d.Store.SetAccessLogs(ctx, envID, on, actor.ID, hook)
	if err != nil {
		return AccessLogsResult{}, err
	}
	if changed {
		s.Wake()
	}
	cur, err := s.d.Store.DeployState(ctx, envID)
	if err != nil {
		return AccessLogsResult{}, err
	}
	res := AccessLogsResult{AccessLogs: cur.AccessLogs, Applies: AppliesRolling}
	switch {
	case !cur.Enabled:
		res.Applies = AppliesWhenEnabled
	case cur.Paused:
		res.Applies = AppliesWhenResumed
	}
	return res, nil
}

// Resume lifts the pause a restore put on the environment (D-42): the audit row ingress.deploy_resumed is written with
// the change, and the ensure loop is woken, so its next pass writes again. It reports whether the environment was
// paused; resuming one that is not changes nothing and records nothing. Nothing resumes on its own.
func (s *Service) Resume(ctx context.Context, actor audit.Actor, envID string) (bool, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return false, err
	}
	if !info.Swarm {
		return false, ErrStandaloneUnavailable
	}
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: actor, Action: audit.ActionIngressDeployResumed, TargetType: "environment", TargetID: envID,
			EnvID: envID, Result: audit.ResultSuccess,
		})
	}
	changed, err := s.d.Store.SetPaused(ctx, envID, false, actor.ID, hook)
	if err != nil {
		return false, err
	}
	if changed {
		// SetPaused notifies nobody (it changes no route), so the loop is asked directly.
		s.Wake()
	}
	return changed, nil
}
