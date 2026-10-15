// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The defaults of the per-environment restart-loop setting (Q5): the failures that make a
// loop and the window they are counted in.
const (
	DefaultRestartLoopFailures = 3
	DefaultRestartLoopWindow   = 10 * time.Minute
)

// RestartLoop is a swarm service whose tasks keep failing (RES-13, D-14).
type RestartLoop struct {
	ServiceID, Name string
	// Count is how many distinct tasks failed inside the window as of the last read.
	Count int
	// LatestError is the error text of the newest failed task, LatestAt when it failed.
	LatestError string
	LatestAt    time.Time
	// Since is when the service was first flagged, in the current run of the flag.
	Since time.Time
}

// RestartLoopTracker is the restart-loop detector of one environment.
type RestartLoopTracker struct{}

// NewRestartLoopTracker returns a tracker that has seen nothing.
func NewRestartLoopTracker() *RestartLoopTracker { return &RestartLoopTracker{} }

// Observe is not written yet.
func (t *RestartLoopTracker) Observe(now time.Time, failed []dockerx.FailedTaskRow, services []dockerx.ServiceRow, threshold int, window time.Duration) map[string]RestartLoop {
	return nil
}
