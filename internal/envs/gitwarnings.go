// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import "time"

// GitSyncError is a git stack whose last sync check ended in error.
type GitSyncError struct {
	StackID, StackName string
	// ReasonCode and ReasonDetail are the check's: a fixed code and a sentence that carries nothing from the server.
	ReasonCode, ReasonDetail string
	// Since is when the stack went into error.
	Since time.Time
}

// GitSyncWarnings are the bell items of the git stacks of one environment whose sync check is in error: one error for
// each. The caller passes only the stacks of an environment whose git the reader manages (git.manage), and only those
// in error, so a stack that is back in sync has no item and nothing is stored to dismiss.
func GitSyncWarnings(e Environment, stacks []GitSyncError) []EnvWarning {
	var out []EnvWarning
	for _, s := range stacks {
		out = append(out, EnvWarning{
			ID: WarnGitSyncError, Severity: SeverityError, EnvID: e.ID, EnvName: cleanText(e.Name, maxNodeLen),
			StackID: s.StackID, StackName: cleanText(s.StackName, maxNodeLen), ReasonCode: cleanText(s.ReasonCode, maxNodeLen),
			ReasonDetail: cleanText(s.ReasonDetail, maxWarnError), Since: s.Since,
		})
	}
	SortWarnings(out)
	return out
}
