// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/store"
)

// JanitorDeps are the dependencies of the janitor routes of the cleanup phase. Every dependency
// the phase's routes use is here, so a later route is added without new wiring.
type JanitorDeps struct {
	// Aggregator and Clusters reach the engines of an environment. Clusters may be nil on a
	// server with no swarm reader; every environment is then read as one engine.
	Aggregator *envs.Aggregator
	Clusters   *envs.Clusters
	// Store holds what the phase persists beside Docker's own state.
	Store *store.Store
	Audit *audit.Writer
	// Checker says what the caller may do beyond the permission of a route.
	Checker *rbac.Checker
	// Locks is the environment's operation lock, which a janitor run takes beside a deploy. A
	// preview never takes it.
	Locks stackops.OperationLock
	// Jobs is the hub a janitor run is a job in, and Clock the time a run judges ages at (nil
	// means the wall clock). Runner starts the runs: the server builds it from the three above
	// and the engines, so a test can hand in one with its own engine.
	Jobs   *jobs.Hub
	Clock  stackops.Clock
	Runner *janitor.Runner
	// Runs reads the record of the runs. The server gives the Runner and the routes the same store.
	Runs janitor.RunStore
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// now is the deps' clock.
func (d JanitorDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// log is the deps' logger.
func (d JanitorDeps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// RegisterJanitor registers the janitor routes.
func RegisterJanitor(r *httpx.Router, d JanitorDeps) {
	registerJanitorPreview(r, d)
	registerJanitorRuns(r, d)
}
