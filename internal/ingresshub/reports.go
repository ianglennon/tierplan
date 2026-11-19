// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresshub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// forgetSeen drops a node from the once-a-minute map, so the memory of a revoked node does not stay.
func (s *Service) forgetSeen(nodeID string) {
	s.seenMu.Lock()
	delete(s.seen, nodeID)
	s.seenMu.Unlock()
}

// decodeHeld reads a held_hostnames column. It is never nil: a node that was never sent a bundle, and a
// column that cannot be read, are the empty list.
func decodeHeld(col string) []string {
	var out []string
	if err := json.Unmarshal([]byte(col), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// RecordSent adds the hostnames of a bundle's routes to the set of hostnames whose keys the node was ever
// sent (D-24). The hub calls it before a bundle frame leaves, so a revoke lists every hostname the node could
// have held, whether or not the frame arrived. The set is sorted, has no duplicates and only grows; the row is
// written only when the set changes, so a bundle that is sent again costs the single writer nothing.
func (s *Service) RecordSent(ctx context.Context, nodeID string, hostnames []string) error {
	row, err := s.st.RQ.GetIngressNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNodeNotFound
	} else if err != nil {
		return fmt.Errorf("load the ingress node: %w", err)
	}
	if _, grew := mergeHeld(decodeHeld(row.HeldHostnames), hostnames); !grew {
		return nil
	}

	// The set grew: merge again inside the write transaction, so two sends at once lose no name.
	tx, err := s.st.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := s.st.Q.WithTx(tx)
	row, err = q.GetIngressNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNodeNotFound
	} else if err != nil {
		return fmt.Errorf("load the ingress node: %w", err)
	}
	merged, grew := mergeHeld(decodeHeld(row.HeldHostnames), hostnames)
	if !grew {
		return nil
	}
	enc, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encode the held hostnames: %w", err)
	}
	if err := q.UpdateIngressNodeHeld(ctx, sqlc.UpdateIngressNodeHeldParams{HeldHostnames: string(enc), ID: nodeID, EnvID: row.EnvID}); err != nil {
		return fmt.Errorf("record the held hostnames: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// mergeHeld returns the sorted, de-duplicated union of held and add, and whether it has a name held did not.
func mergeHeld(held, add []string) (merged []string, grew bool) {
	base := slices.Compact(slices.Sorted(slices.Values(held)))
	merged = slices.Clone(base)
	for _, h := range add {
		if h != "" {
			merged = append(merged, h)
		}
	}
	slices.Sort(merged)
	merged = slices.Compact(merged)
	return merged, len(merged) > len(base)
}
