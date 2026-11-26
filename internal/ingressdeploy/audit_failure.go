// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"

	"github.com/ianglennon/tierplan/internal/audit"
)

// The step and code of a standalone enable whose project did not come up. The deploy ends its own items with codes of its
// own, so the audit row names the step as a whole and says the project did not come up.
const (
	auditStepSystem        = "system"
	CodeEnableSystemFailed = "enable_system_failed"
)

// auditJobFailed writes the one audit row of a switch job that failed or was cancelled: action is ingress.enabled or
// ingress.disabled, step is the item that ended the job and code its stable code (jobs.CodeCancelled for a cancel). The row
// holds only the step and the code, never the engine's or the daemon's words, which can echo a field of the spec (A-01 class F).
//
// It is the one writer of such a row. It writes with a context that outlives the job's own, since a cancelled job is the
// case it records; it writes nothing when no audit writer is wired; and a write that fails is logged and never fails the job,
// which has already ended its items.
func (s *Service) auditJobFailed(ctx context.Context, actor audit.Actor, action, envID, step, code string) {
	if s.d.Audit == nil {
		return
	}
	err := s.d.Audit.Record(context.WithoutCancel(ctx), audit.Entry{
		Actor: actor, Action: action, TargetType: "environment", TargetID: envID, EnvID: envID,
		Result: audit.ResultFailure, Reason: code,
		Details: map[string]string{"step": step, "code": code},
	})
	if err != nil {
		s.log().Warn("could not audit a failed ingress switch", "environment", envID, "action", action, "step", step, "code", code, "error", err)
	}
}
