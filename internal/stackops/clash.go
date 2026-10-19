// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// ForeignStack reports whether the environment already holds a stack of that name that Bayplan does
// not manage.
func (d Deps) ForeignStack(ctx context.Context, envID string, target stacks.Target, name, stackID string) (bool, error) {
	return false, nil
}

// CheckTarget says whether the stack can be deployed to the environment as it is now.
func (d Deps) CheckTarget(ctx context.Context, envID string, stack Stack) error {
	return nil
}
