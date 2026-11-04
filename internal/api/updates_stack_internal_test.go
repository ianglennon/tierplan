// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The reason a run keeps for a deploy that failed after it changed a service, and which failures say something about the version
// that was pulled (plan 12-23, D-13, D-14).
func TestUpdateFailureReasons(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]string{
		stackops.CodeConvergeTimeout: updates.ReasonTimeout,
		stackops.CodeTasksFailed:     updates.ReasonDidntStart,
		stackops.CodeUpdateStopped:   updates.ReasonDidntStart,
		stackops.CodeNetworkFull:     updates.ReasonDidntStart,
		stackops.CodeServiceRejected: updates.ReasonRejected,
		stackops.CodeServiceChanged:  updates.ReasonServiceChanged,
		stackops.CodeEngineError:     updates.ReasonEngine,
	} {
		if got := updateFailureReason(code); got != want {
			t.Errorf("updateFailureReason(%q) = %q, want %q", code, got, want)
		}
	}
	for code, want := range map[string]bool{
		dockerx.PullCodeNotFound:       true,
		dockerx.PullCodeRateLimited:    true,
		dockerx.PullCodeRegistryTLS:    true,
		stackops.CodeNodeUnreachable:   false,
		stackops.CodeHostReadOnly:      false,
		stackops.CodeAgentPolicyDenied: false,
		dockerx.PullCodeCancelled:      false,
		stackops.CodeTasksFailed:       false,
		"":                             false,
	} {
		if got := isPullFailureCode(code); got != want {
			t.Errorf("isPullFailureCode(%q) = %t, want %t", code, got, want)
		}
	}
}
