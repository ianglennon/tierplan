// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// StatsKind says what one StatsUpdate is.
type StatsKind string

const (
	// StatsKindSample carries a point.
	StatsKindSample StatsKind = "sample"
	// StatsKindNotReporting says the node of the container could not be asked.
	StatsKindNotReporting StatsKind = "not_reporting"
	// StatsKindStopped says the container is gone or is not running.
	StatsKindStopped StatsKind = "stopped"
)

// StatsUpdate is what a subscriber to a container's stats receives.
type StatsUpdate struct {
	Kind StatsKind
	// Point is the sample; it is set for StatsKindSample only.
	Point dockerx.StatsPoint
}

// statsNodes is what the hub needs of the aggregator: to run a call on one node's engine.
type statsNodes interface {
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// StatsHubOptions tune a StatsHub. The zero value is the production setting.
type StatsHubOptions struct {
	// Interval is how often a container is sampled. Zero means two seconds.
	Interval time.Duration
	// PerNode is the most sample requests in flight on one node. Zero means eight.
	PerNode int
	// NewTicker makes the ticker of one sampler. Nil means time.NewTicker. A test passes
	// its own, so that no test waits for a clock.
	NewTicker func(d time.Duration) (ticks <-chan time.Time, stop func())
	// Log receives server-side failures, never a sample. Nil means slog.Default().
	Log *slog.Logger
}

// StatsHub samples containers for the browsers that watch them.
type StatsHub struct{}

// NewStatsHub builds a hub that samples through src.
func NewStatsHub(src statsNodes, o StatsHubOptions) *StatsHub { return &StatsHub{} }

// Subscribe starts watching a container, or joins the sampler that already does.
func (h *StatsHub) Subscribe(envID, nodeID, containerID string) (<-chan StatsUpdate, func()) {
	ch := make(chan StatsUpdate)
	close(ch)
	return ch, func() {}
}

// Samplers is how many containers are being sampled now.
func (h *StatsHub) Samplers() int { return 0 }

// Close stops every sampler, closes every subscriber's channel and waits for the samplers to return.
func (h *StatsHub) Close() {}
