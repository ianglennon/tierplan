// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// DefaultSnapshotTTL is how long a snapshot is served before it is refetched.
const DefaultSnapshotTTL = 5 * time.Second

// Snapshots caches one dockerx.Snapshot per environment for a short time.
type Snapshots struct{}

// NewSnapshots returns a cache over m.
func NewSnapshots(_ *Monitor, _ time.Duration, _ func() time.Time) *Snapshots { return &Snapshots{} }

// Get returns the snapshot of envID.
func (s *Snapshots) Get(_ context.Context, _ string) (dockerx.Snapshot, Status, error) {
	return dockerx.Snapshot{}, Status{}, errors.New("envs: Snapshots.Get is not implemented")
}

// Invalidate drops the cached snapshot of envID.
func (s *Snapshots) Invalidate(_ string) {}
