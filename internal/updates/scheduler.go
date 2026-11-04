// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/schedule"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The scheduler of the update checks (IMG-04, D-01). It is an in-house tick loop on a clock the caller
// gives, like the janitor's: it waits only on that clock, so a test moves time itself.

const (
	// DefaultTick is how often the scheduler looks for environments whose check is due.
	DefaultTick = 30 * time.Second
	// MaxConcurrentChecks is how many passes of checks the scheduler has going at once, so that the
	// schedules of many environments do not all ask the registries together (T-12-20). A pass is one
	// fleet check of every environment that was due in a tick.
	MaxConcurrentChecks = 2
	// SystemActor is who a scheduled check is in a log line. A check changes nothing on a host and is not
	// audited; its result carries when it was made.
	SystemActor = "system:updates"
)

// Scheduler checks each enabled environment for image updates at the slot its policy names. A check it
// starts only reads registries and writes what they answered to SQLite: it takes no operation lock, and it
// never starts an update or changes anything on an engine (IMG-06).
//
// It does not catch up: when the server starts, the first check of every environment is its next slot after
// that moment, and a slot the scheduler was too busy to start is skipped, not made up. A disabled
// environment is never checked on schedule. Every environment due in a tick goes through the one
// fleet-deduplicated Checker together, and at most MaxConcurrentChecks such passes are active at once.
type Scheduler struct {
	// Envs lists the IDs of the environments that exist and are enabled.
	Envs func(ctx context.Context) ([]string, error)
	// Policy gives each environment's schedule.
	Policy *PolicyStore
	// Service reads the workloads of an environment (Service.Workloads) and asks the registries about
	// them (Service.Checker).
	Service *Service
	// Clock is the time the scheduler fires by and waits on. Nil means the wall clock.
	Clock stackops.Clock
	// Loc is the time zone the schedules are read in. Nil means time.Local.
	Loc *time.Location
	// Tick is how often the scheduler looks for a due check. Zero means DefaultTick.
	Tick time.Duration
	// Log receives what the scheduler could not do. Nil means slog.Default().
	Log *slog.Logger
	// Ended, when set, is told that a pass the scheduler started has ended, with the environments it
	// covered. It is the seam a test waits on instead of sleeping.
	Ended func(envIDs []string)

	mu    sync.Mutex
	slots map[string]slot
	// stale is how old a successful check of each environment may be before it reads stale, as its
	// schedule says.
	stale map[string]time.Duration
	// busy are the environments of a pass that has not ended; passes is how many such passes there are.
	busy   map[string]bool
	passes int
	// wg waits for the goroutines that follow each started pass to its end.
	wg sync.WaitGroup
}

// slot is the next check of an environment and the schedule it was worked out from.
type slot struct {
	spec string
	at   time.Time
}

func (s *Scheduler) clock() stackops.Clock {
	if s.Clock != nil {
		return s.Clock
	}
	return stackops.RealClock()
}

func (s *Scheduler) loc() *time.Location {
	if s.Loc != nil {
		return s.Loc
	}
	return time.Local
}

func (s *Scheduler) tick() time.Duration {
	if s.Tick > 0 {
		return s.Tick
	}
	return DefaultTick
}

func (s *Scheduler) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Location is the time zone the schedules are read in.
func (s *Scheduler) Location() *time.Location { return s.loc() }

// Run looks for due checks now and then every tick until ctx ends, and waits for the passes it started to
// end before it returns. The first look works out every environment's next check from now, so nothing a
// stopped server missed is made up.
func (s *Scheduler) Run(ctx context.Context) {
	defer s.wg.Wait()
	for {
		s.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.clock().After(s.tick()):
		}
	}
}

// Next is the time of the environment's next scheduled check, and false when it has none: the environment
// is disabled, has no valid schedule, or the scheduler has not looked at it yet.
func (s *Scheduler) Next(envID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[envID]
	if !ok {
		return time.Time{}, false
	}
	return sl.at, true
}

// StaleAfter is how old a successful check of the environment may be before an unchanged digest reads
// check_stale: three times the gap of its schedule, and a day when its schedule is off or the scheduler has
// not looked at it yet. It is the Service's bound.
func (s *Scheduler) StaleAfter(envID string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.stale[envID]; ok {
		return d
	}
	return DefaultStaleAfter
}

// round is one look at every environment: it works out the slot of one that has none or whose schedule
// changed, and starts the pass of those that are due.
func (s *Scheduler) round(ctx context.Context) {
	ids, err := s.Envs(ctx)
	if err != nil {
		s.log().Error("the update check scheduler could not list the environments", "error", err)
		return
	}
	slices.Sort(ids)
	def, _, err := s.Policy.Defaults(ctx)
	if err != nil {
		s.log().Error("the update check scheduler could not read the defaults", "error", err)
		return
	}
	now := s.clock().Now()
	// The policies are read before the lock is taken: a read is I/O.
	pols := make(map[string]Policy, len(ids))
	for _, id := range ids {
		env, _, err := s.Policy.Env(ctx, id)
		if err != nil {
			s.log().Error("the update check scheduler could not read a policy", "environment", id, "error", err)
			continue
		}
		pols[id] = Effective(def, env)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots, s.stale, s.busy = map[string]slot{}, map[string]time.Duration{}, map[string]bool{}
	}
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	for id := range s.slots {
		if !listed[id] {
			delete(s.slots, id)
			delete(s.stale, id)
		}
	}
	var due []string
	for _, id := range ids {
		pol, ok := pols[id]
		if !ok {
			continue
		}
		s.stale[id] = StaleAfterOf(pol, id, now, s.loc())
		if !pol.Enabled {
			delete(s.slots, id)
			continue
		}
		sl, known := s.slots[id]
		if !known || sl.spec != pol.Schedule {
			// First sight, or the schedule was changed: the next check is the next slot from now.
			at, err := schedule.NextRun(pol.Schedule, id, now, s.loc(), MinInterval)
			if err != nil || at.IsZero() {
				s.log().Warn("an environment's update check schedule cannot be used, so it will not be checked", "environment", id, "schedule", pol.Schedule, "error", err)
				delete(s.slots, id)
				continue
			}
			s.slots[id] = slot{spec: pol.Schedule, at: at}
			continue
		}
		if !now.Before(sl.at) {
			due = append(due, id)
		}
	}
	s.startDue(ctx, due, now)
}

// advance moves the slot of an environment to the next one after now, with the mutex held. It is from now,
// never from the slot: a check that was late is not followed at once by a catch-up.
func (s *Scheduler) advance(id string, now time.Time) {
	sl := s.slots[id]
	if at, err := schedule.NextRun(sl.spec, id, now, s.loc(), MinInterval); err == nil && !at.IsZero() {
		s.slots[id] = slot{spec: sl.spec, at: at}
	} else {
		delete(s.slots, id)
	}
}

// startDue starts one pass over the environments whose slot has come, with the mutex held. A scheduler
// with MaxConcurrentChecks passes active leaves the slots due, so they start on a later tick. An
// environment whose last pass is still going misses this slot instead of being asked twice at once.
func (s *Scheduler) startDue(ctx context.Context, due []string, now time.Time) {
	var run []string
	for _, id := range due {
		if s.busy[id] {
			s.log().Warn("an environment's update check is still working, so this scheduled check is skipped", "environment", id)
			s.advance(id, now)
			continue
		}
		run = append(run, id)
	}
	if len(run) == 0 || s.passes >= MaxConcurrentChecks {
		return
	}
	for _, id := range run {
		s.advance(id, now)
		s.busy[id] = true
	}
	s.passes++
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			s.passes--
			for _, id := range run {
				delete(s.busy, id)
			}
			s.mu.Unlock()
			if s.Ended != nil {
				s.Ended(run)
			}
		}()
		s.check(ctx, run)
	})
}

// check asks the registries about the workloads of the environments, once for each distinct image across all
// of them (Checker.CheckFleet), as the system. It is not forced: a key checked a moment ago is left alone.
func (s *Scheduler) check(ctx context.Context, envIDs []string) {
	workloads := make(map[string][]Workload, len(envIDs))
	var checked []string
	for _, id := range envIDs {
		ws, err := s.Service.Workloads(ctx, id)
		if err != nil {
			if ctx.Err() == nil {
				s.log().Warn("the update check scheduler could not read what an environment runs", "environment", id, "actor", SystemActor, "error", err)
			}
			continue
		}
		workloads[id] = ws
		checked = append(checked, id)
	}
	if len(checked) == 0 {
		return
	}
	if _, err := s.Service.Checker.CheckFleet(ctx, checked, workloads, false); err != nil && ctx.Err() == nil {
		s.log().Warn("a scheduled update check did not finish", "environments", len(checked), "actor", SystemActor, "error", err)
	}
}
