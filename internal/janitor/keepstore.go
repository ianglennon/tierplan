// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"fmt"

	"github.com/ianglennon/tierplan/internal/store"
)

// KeepKey names one object a person kept out of cleanup. NodeID is the ID of the node the object is on
// (the one Container.NodeID and Image.NodeID carry), empty for a swarm-wide network; Ref is the
// container's or the image's ID, and a network's name.
type KeepKey struct {
	Kind        Kind
	NodeID, Ref string
}

// KeepStore reads the keeps of an environment from the one table every keep lives in, shared by the
// Janitor page and the Address space page (D-12).
type KeepStore struct {
	St *store.Store
}

// List reads every keep of the environment. A KeepStore with no store has none.
func (k KeepStore) List(ctx context.Context, envID string) (map[KeepKey]bool, error) {
	if k.St == nil {
		return nil, nil
	}
	rows, err := k.St.RQ.ListJanitorKeeps(ctx, envID)
	if err != nil {
		return nil, fmt.Errorf("read the janitor's keeps: %w", err)
	}
	out := make(map[KeepKey]bool, len(rows))
	for _, r := range rows {
		out[KeepKey{Kind: Kind(r.Kind), NodeID: r.NodeID, Ref: r.Ref}] = true
	}
	return out, nil
}
