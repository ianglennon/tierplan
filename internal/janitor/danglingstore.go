// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"fmt"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// DanglingStore keeps when the janitor first saw each image without a name (D-07), which an image's grace
// period is measured from: Docker does not say when an image lost its tag, and its build time is usually long
// before that.
type DanglingStore struct {
	St *store.Store
}

// imageKey names one image on one node.
type imageKey struct{ nodeID, imageID string }

// Stamp gives each image of s that has no name the time it was first seen so (Image.DanglingSince), starting
// the clock at now for an image seen so for the first time. The clock of an image on a node that reported is
// stopped when the image is gone or has a name again, so one that is tagged and loses the tag again starts a
// new grace period; a node that did not report keeps its clocks. A DanglingStore with no store keeps no clock.
// A clock that cannot be read or written is an error: judged without it, an image would be measured from its
// build time and could be removed the moment it lost its name.
func (d DanglingStore) Stamp(ctx context.Context, envID string, s *Snapshot, now time.Time) error {
	if d.St == nil {
		return nil
	}
	rows, err := d.St.RQ.ListImageDanglingSeen(ctx, envID)
	if err != nil {
		return fmt.Errorf("read when the images lost their names: %w", err)
	}
	seen := make(map[imageKey]time.Time, len(rows))
	for _, r := range rows {
		seen[imageKey{r.NodeID, r.ImageID}] = time.Unix(0, r.FirstSeenAt).UTC()
	}
	now = now.UTC()
	live := make(map[imageKey]bool, len(s.Images))
	var fresh []imageKey
	for i := range s.Images {
		img := &s.Images[i]
		if !IsDangling(img.RepoTags, img.RepoDigests) {
			continue
		}
		k := imageKey{img.NodeID, img.ID}
		live[k] = true
		at, ok := seen[k]
		if !ok {
			at = now
			fresh = append(fresh, k)
		}
		img.DanglingSince = at
	}
	reporting := make(map[string]bool, len(s.Nodes))
	for _, n := range s.Nodes {
		reporting[n.ID] = true
	}
	var stale []imageKey
	for k := range seen {
		if reporting[k.nodeID] && !live[k] {
			stale = append(stale, k)
		}
	}
	if len(fresh) == 0 && len(stale) == 0 {
		return nil
	}
	if err := d.write(ctx, envID, now, fresh, stale); err != nil {
		return fmt.Errorf("record when the images lost their names: %w", err)
	}
	return nil
}

// write starts the clocks of fresh and stops those of stale in one transaction.
func (d DanglingStore) write(ctx context.Context, envID string, now time.Time, fresh, stale []imageKey) error {
	tx, err := d.St.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.St.Q.WithTx(tx)
	for _, k := range fresh {
		if err := q.InsertImageDanglingSeen(ctx, sqlc.InsertImageDanglingSeenParams{
			EnvID: envID, NodeID: k.nodeID, ImageID: k.imageID, FirstSeenAt: now.UnixNano(),
		}); err != nil {
			return err
		}
	}
	for _, k := range stale {
		if err := q.DeleteImageDanglingSeen(ctx, sqlc.DeleteImageDanglingSeenParams{
			EnvID: envID, NodeID: k.nodeID, ImageID: k.imageID,
		}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
