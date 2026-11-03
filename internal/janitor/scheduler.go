// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The scheduler of the janitor (JAN-01, D-01, D-02, D-08). It is an in-house tick loop on a clock the
// caller gives, like the git poller: it waits only on that clock, so a test moves time itself.

const (
	// DefaultTick is how often the scheduler looks for environments whose run is due.
	DefaultTick = 30 * time.Second
	// MaxConcurrentRuns is how many environments the scheduler cleans at once, so that the schedules of
	// many environments do not all read every node together (T-11-43).
	MaxConcurrentRuns = 2
	// LockWait is how long a scheduled run waits for an environment that a deploy or another operation
	// holds, before it gives up and ends skipped (D-08).
	LockWait = 10 * time.Minute
	// LockPoll is how often a waiting scheduled run tries the lock.
	LockPoll = 10 * time.Second
)

// scheduledBy is the name a scheduled run is shown under where a lock names who holds it.
const scheduledBy = "TierPlan"

// Scheduler starts a run of the janitor on each environment at the slot its policy names. A run it
// starts is a live clean-up of everything removable at that moment, by the system, through the same
// Runner as Clean now.
//
// It does not catch up: when the server starts, the first run of every environment is its next slot
// after that moment, and a slot the scheduler was too busy to start is skipped, not made up. A
// disabled environment never runs. At most MaxConcurrentRuns runs are active at once.
type Scheduler struct {
	// Envs lists the IDs of the environments that exist.
	Envs func(ctx context.Context) ([]string, error)
	// Runner starts the runs and gives the policy of an environment (Runner.Policy, DefaultPolicy when
	// that is nil).
	Runner *Runner
	// Clock is the time the scheduler fires by and waits on. Nil means the wall clock.
	Clock stackops.Clock
	// Loc is the time zone the schedules are read in. Nil means time.Local.
	Loc *time.Location
	// Tick is how often the scheduler looks for a due run. Zero means DefaultTick.
	Tick time.Duration
	// Log receives what the scheduler could not do. Nil means slog.Default().
	Log *slog.Logger

	mu    sync.Mutex
	slots map[string]slot
	// active are the environments with a run the scheduler started that has not ended; their number is
	// what MaxConcurrentRuns caps.
	active map[string]bool
	// wg waits for the goroutines that follow each started run to its end.
	wg sync.WaitGroup
}

// slot is the next run of an environment and the schedule it was worked out from.
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

// Location is the time zone the schedules are read in.
func (s *Scheduler) Location() *time.Location { return s.loc() }

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

// Run looks for due runs now and then every tick until ctx ends, and waits for the runs it started to
// end before it returns. The first look works out every environment's next run from now, so nothing a
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

// Next is the time of the environment's next scheduled run, and false when it has none: the
// environment is disabled, has no valid schedule, or the scheduler has not looked at it yet.
func (s *Scheduler) Next(envID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[envID]
	if !ok {
		return time.Time{}, false
	}
	return sl.at, true
}

// policy is the policy of an environment.
func (s *Scheduler) policy(ctx context.Context, envID string) (Policy, error) {
	if s.Runner == nil || s.Runner.Policy == nil {
		return DefaultPolicy(), nil
	}
	return s.Runner.Policy(ctx, envID)
}

// round is one look at every environment: it works out the slot of one that has none or whose
// schedule changed, and starts the run of one that is due.
func (s *Scheduler) round(ctx context.Context) {
	ids, err := s.Envs(ctx)
	if err != nil {
		s.log().Error("the janitor's scheduler could not list the environments", "error", err)
		return
	}
	slices.Sort(ids)
	now := s.clock().Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = map[string]slot{}
		s.active = map[string]bool{}
	}
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	for id := range s.slots {
		if !listed[id] {
			delete(s.slots, id)
		}
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		pol, err := s.policy(ctx, id)
		if err != nil {
			s.log().Error("the janitor's scheduler could not read a policy", "environment", id, "error", err)
			continue
		}
		if !pol.Enabled {
			delete(s.slots, id)
			continue
		}
		spec := pol.Schedule
		if spec == "" {
			spec = DefaultSchedule
		}
		sl, known := s.slots[id]
		if !known || sl.spec != spec {
			// First sight, or the schedule was changed: the next run is the next slot from now.
			at, err := NextRun(spec, id, now, s.loc())
			if err != nil || at.IsZero() {
				s.log().Warn("an environment's janitor schedule cannot be used, so it will not run", "environment", id, "schedule", spec, "error", err)
				delete(s.slots, id)
				continue
			}
			s.slots[id] = slot{spec: spec, at: at}
			continue
		}
		if now.Before(sl.at) {
			continue
		}
		s.startDue(ctx, id, sl, now)
	}
}

// startDue starts the run of an environment whose slot has come, with the mutex held. A busy
// scheduler (MaxConcurrentRuns active) leaves the slot due, so it starts on a later tick. An
// environment whose last run is still going misses this slot instead of being run twice at once.
func (s *Scheduler) startDue(ctx context.Context, id string, sl slot, now time.Time) {
	advance := func() {
		// From now, never from the slot: a run that was late is not followed at once by a catch-up.
		if at, err := NextRun(sl.spec, id, now, s.loc()); err == nil && !at.IsZero() {
			s.slots[id] = slot{spec: sl.spec, at: at}
		} else {
			delete(s.slots, id)
		}
	}
	if s.active[id] {
		s.log().Warn("an environment's janitor is still working, so this scheduled run is skipped", "environment", id)
		advance()
		return
	}
	if len(s.active) >= MaxConcurrentRuns {
		return
	}
	started, err := s.Runner.Start(ctx, RunRequest{
		EnvID: id, Trigger: TriggerSchedule, Actor: scheduledBy, ActorID: SystemActor,
		Owner: SystemActor + ":" + id, Principal: rbac.System(SystemActor), All: true,
	})
	advance()
	if err != nil {
		s.log().Error("a scheduled janitor run could not be started", "environment", id, "error", err)
		return
	}
	s.active[id] = true
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			delete(s.active, id)
			s.mu.Unlock()
		}()
		// A stopping server ends the wait, not the run: the hub cancels its jobs when it closes.
		if _, err := s.Runner.Jobs.Wait(ctx, started.JobID); err != nil && ctx.Err() == nil {
			s.log().Warn("the scheduler could not follow a janitor run to its end", "environment", id, "run", started.RunID, "error", err)
		}
	})
}
