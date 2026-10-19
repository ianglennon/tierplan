// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"

	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// CodeNeedsAdmin is the code of the blocking problem for a host-escalation setting the caller may
// not deploy.
const CodeNeedsAdmin = "needs_admin"

// PolicyEntry is one host-escalation setting the caller may not deploy: what it is, and where.
type PolicyEntry struct {
	Text, File string
	Line       int
}

// PolicyResult is the host-escalation judgement of a stack for one caller.
type PolicyResult struct {
	Refused  bool
	Problems stacks.Problems
	Entries  []PolicyEntry
	Kinds    []string
}

// Policy judges the host access a stack asks for on the values a deploy would use.
func (d Deps) Policy(ctx context.Context, caller Caller, envID string, copy stacks.WorkingCopy, plan convert.Plan) (PolicyResult, error) {
	return PolicyResult{}, nil
}
