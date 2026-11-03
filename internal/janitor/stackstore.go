// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"fmt"

	"github.com/ianglennon/tierplan/internal/store"
)

// StackStore reads the TierPlan stacks of an environment with their janitor exclusion (D-10).
type StackStore struct {
	St *store.Store
}

// List reads every stack of the environment. A StackStore with no store has none.
func (k StackStore) List(ctx context.Context, envID string) ([]StackFact, error) {
	if k.St == nil {
		return nil, nil
	}
	rows, err := k.St.RQ.ListStacksByEnv(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the stacks the janitor leaves alone: %w", err)
	}
	out := make([]StackFact, 0, len(rows))
	for _, r := range rows {
		out = append(out, StackFact{ID: r.ID, Name: r.Name, Excluded: r.JanitorExcluded != 0})
	}
	return out, nil
}
