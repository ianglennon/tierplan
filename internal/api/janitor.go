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
	// Scheduler is the janitor's scheduler, which the status reads the next run from. Nil means the
	// next run is worked out from the policy alone.
	Scheduler *janitor.Scheduler
	// Runs reads the record of the runs. The server gives the Runner and the routes the same store.
	Runs janitor.RunStore
	// AddressSpaces is the orphan check the preview judges networks by, the one the Address space
	// page lists them with. Nil means the preview lists no network.
	AddressSpaces *envs.AddressSpaces
	// StackFacts are TierPlan's own stacks, the same ones the Address space check reads. The janitor
	// asks them which stacks and projects of an environment still exist before it removes a compose
	// container. Nil means they are not read, and no compose container is removable.
	StackFacts envs.StackFacts
	// Heartbeat is how often an idle run stream gets a ping, MaxStreamsPerUser how many run streams
	// one person may hold open and MaxLifetime how long one stream may live. Zero takes the defaults
	// every other stream has.
	Heartbeat, MaxLifetime time.Duration
	MaxStreamsPerUser      int
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

// networks is the janitor's way to judge networks, nil when the deps have no orphan check.
func (d JanitorDeps) networks() janitor.Networks {
	if d.AddressSpaces == nil {
		return nil
	}
	return NewJanitorNetworks(AddressSpaceDeps{
		AddressSpaces: d.AddressSpaces, Aggregator: d.Aggregator, Clusters: d.Clusters, Store: d.Store, Audit: d.Audit,
		Checker: d.Checker, Now: d.Now, Log: d.log(),
	})
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
	registerJanitorStatus(r, d)
	registerJanitorRuns(r, d)
	registerJanitorStream(r, d)
	registerJanitorEvidence(r, d)
}
