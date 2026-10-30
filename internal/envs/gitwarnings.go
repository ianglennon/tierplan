// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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

// GitAutoDeployNotice is something an automatic deploy of a git stack did that its owner and the environment's Admins
// should hear about: Kind is WarnGitAutoDeploySkipped, WarnGitAutoDeployReverted or WarnGitAutoDeployPaused.
type GitAutoDeployNotice struct {
	Kind               string
	StackID, StackName string
	// Since is when it happened, or since when the automation has been paused.
	Since time.Time
	// Detail is the sentence that goes with a held back commit or a pause.
	Detail string
	// Commit is the short commit that was held back.
	Commit string
	// Count is how many changes a deploy put back and Example the first of them; Revision is the revision it made, or
	// for a pause by a roll back the revision rolled back to.
	Count    int
	Example  string
	Revision int64
	// Reason is why automation is paused (owner or rollback) and Owner who it runs for.
	Reason, Owner string
}

// GitAutoDeployWarnings are the bell items of the automatic deploys of one environment's git stacks: one warning for each
// notice. The caller passes only the notices the reader may see, which are those of an automation they own and, for an
// Admin of the environment, all of them (T-09-100).
func GitAutoDeployWarnings(e Environment, notices []GitAutoDeployNotice) []EnvWarning {
	var out []EnvWarning
	for _, n := range notices {
		out = append(out, EnvWarning{
			ID: n.Kind, Severity: SeverityWarning, EnvID: e.ID, EnvName: cleanText(e.Name, maxNodeLen),
			StackID: n.StackID, StackName: cleanText(n.StackName, maxNodeLen), Since: n.Since,
			ReasonCode: cleanText(n.Reason, maxNodeLen), ReasonDetail: cleanText(n.Detail, maxWarnError),
			Commit: cleanText(n.Commit, maxNodeLen), Count: n.Count, Example: cleanText(n.Example, maxWarnError),
			Revision: n.Revision, Owner: cleanText(n.Owner, maxNodeLen),
		})
	}
	SortWarnings(out)
	return out
}
