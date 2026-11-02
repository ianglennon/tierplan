// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// DeployedMasker is the masker of a stack's deployed revision: the secret variables and secret files
// of the newest revision that deployed well, which are what the stack's containers were given. The
// janitor uses it to hide those values in the last lines of the log of a crashed container before it
// saves them (D-06, RESEARCH F8).
//
// A stack that is unknown, has no revision that deployed well, or whose revision cannot be opened
// answers the masker that hides nothing: the clean-up of a crashed container is never blocked by a
// stack's record. A failure to read the record is the error, with the empty masker, so the caller can
// say the stack's values were not hidden.
func (r Repo) DeployedMasker(ctx context.Context, stackID string) (stacks.Masker, error) {
	good, found, err := r.LatestGoodRevision(ctx, stackID)
	if err != nil || !found {
		return stacks.Masker{}, err
	}
	return stacks.NewMasker(good.Copy.Vars, good.Copy.Files), nil
}

// DeployedMasker implements the janitor's stack masker: the deployed masker of a stack of the
// environment, over the same records the network check reads.
func (n NetFacts) DeployedMasker(ctx context.Context, stackID string) (stacks.Masker, error) {
	return n.Repo.DeployedMasker(ctx, stackID)
}
