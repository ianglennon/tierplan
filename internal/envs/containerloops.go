// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// ContainerLoop is a standalone container that keeps dying (RES-13, D-15).
type ContainerLoop struct {
	NodeID, Name, ContainerID string
	Count                     int
	Window                    time.Duration
	LastExitCode              string
	Since, LatestAt           time.Time
	Muted                     bool
}

// ContainerLoopKey is how a container is named across reads and in its mute: its node and its
// name, so a container recreated under the same name on the same node is the same one (A12).
func ContainerLoopKey(nodeID, name string) string { return nodeID + "/" + name }

// ContainerLoopSource is the die events one node's feed holds.
type ContainerLoopSource struct {
	NodeID string
	Events []dockerx.EventRow
}

// ContainerLoopTracker is the restart-loop detector for the standalone containers of one
// environment.
type ContainerLoopTracker struct {
	mu sync.Mutex
}

// NewContainerLoopTracker returns a tracker that has seen nothing.
func NewContainerLoopTracker() *ContainerLoopTracker { return &ContainerLoopTracker{} }

// Observe is not written yet.
func (t *ContainerLoopTracker) Observe(now time.Time, sources []ContainerLoopSource, rows func(nodeID string) ([]dockerx.ContainerRow, bool), policy func(nodeID string, row dockerx.ContainerRow) (string, bool), threshold int, window time.Duration) map[string]ContainerLoop {
	return nil
}

// ApplyContainerMutes is not written yet.
func ApplyContainerMutes(loops map[string]ContainerLoop, mutedKeys []string) (merged map[string]ContainerLoop, muted map[string]bool) {
	return nil, nil
}
