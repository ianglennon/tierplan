// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"fmt"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// ResumeAutoDeployFromOwner lifts the pause of a git stack's automatic deploys that was set because their owner lacked
// the permission, and only that pause. It says whether it did: false means the stack is no longer running on that pause
// (a person turned auto-deploy off, or a roll back paused it for its own reason), and nothing was changed, so a caller
// working from settings it read earlier cannot undo a change made since (review WR-B02).
func (r Repo) ResumeAutoDeployFromOwner(ctx context.Context, stackID string) (bool, error) {
	n, err := r.Store.Q.ResumeStackGitAutoDeployOwner(ctx, sqlc.ResumeStackGitAutoDeployOwnerParams{UpdatedAt: r.now().UnixNano(), StackID: stackID})
	if err != nil {
		return false, fmt.Errorf("resume the auto-deploy: %w", err)
	}
	return n > 0, nil
}

// PauseAutoDeployForOwner pauses a git stack's running automatic deploys because their owner lacks the permission. It
// says whether it did: false means the stack was not running (off, or already paused for some reason), and nothing
// was changed.
func (r Repo) PauseAutoDeployForOwner(ctx context.Context, stackID string) (bool, error) {
	n, err := r.Store.Q.PauseStackGitAutoDeployOwner(ctx, sqlc.PauseStackGitAutoDeployOwnerParams{UpdatedAt: r.now().UnixNano(), StackID: stackID})
	if err != nil {
		return false, fmt.Errorf("pause the auto-deploy: %w", err)
	}
	return n > 0, nil
}
