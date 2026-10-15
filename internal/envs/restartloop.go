// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The defaults of the per-environment restart-loop setting (Q5): the failures that make a
// loop and the window they are counted in.
const (
	DefaultRestartLoopFailures = 3
	DefaultRestartLoopWindow   = 10 * time.Minute
)

// Task states that count as a failure. A shutdown task (an update's old task, exit 137) and
// a complete one (a job's) never do, and exit codes are never read (RESEARCH E12).
const (
	taskFailed   = "failed"
	taskRejected = "rejected"
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

// serviceFailures is what the tracker keeps of one service: when each of its failed tasks
// failed, by task ID, the newest of them, and when the flag was raised (zero while it is
// not).
type serviceFailures struct {
	at          map[string]time.Time
	latestAt    time.Time
	latestError string
	flaggedAt   time.Time
}

// RestartLoopTracker is the restart-loop detector of one environment (RES-13). It is fed
// the failed tasks of every cluster read and remembers the ones it has seen, so a count
// survives Docker pruning its task history (at most TaskHistoryRetentionLimit - 1 failed
// tasks per slot are ever visible, RESEARCH E11 and Pitfall 11). It only flags: it holds
// no client and no function, so it has nothing to restart, stop or scale a service with. It
// is safe for concurrent use.
type RestartLoopTracker struct {
	mu       sync.Mutex
	services map[string]*serviceFailures
}

// NewRestartLoopTracker returns a tracker that has seen nothing.
func NewRestartLoopTracker() *RestartLoopTracker {
	return &RestartLoopTracker{services: map[string]*serviceFailures{}}
}

// Observe takes the failed tasks one read of the cluster found and the services it listed,
// and returns the services in a restart loop by service ID. now is the clock, so the same
// calls always give the same answer.
//
// A service is flagged when at least threshold distinct tasks of it, in any slot, failed
// within window of now (the comparison is inclusive, and a failure exactly window old is
// out). Once flagged it stays flagged until a whole window passes with no new failure
// (hysteresis, D-14): a sliding count alone would clear while the service was still
// failing, one task at a time. Count is the failures still inside the window, which can be
// below the threshold while the flag holds. A task seen in several reads counts once. A
// task with no time counts from when it was first seen. A service that is no longer listed
// is forgotten. A threshold below one or a window of zero or less is the default.
//
// A job's failed tasks count like any other; its complete tasks, like every task that did
// not fail or get rejected, are ignored.
func (t *RestartLoopTracker) Observe(now time.Time, failed []dockerx.FailedTaskRow, services []dockerx.ServiceRow, threshold int, window time.Duration) map[string]RestartLoop {
	if threshold < 1 {
		threshold = DefaultRestartLoopFailures
	}
	if window <= 0 {
		window = DefaultRestartLoopWindow
	}
	names := make(map[string]string, len(services))
	for _, s := range services {
		names[s.ID] = s.Name
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.services {
		if _, ok := names[id]; !ok {
			delete(t.services, id)
		}
	}
	for _, f := range failed {
		if f.State != taskFailed && f.State != taskRejected {
			continue
		}
		if _, ok := names[f.ServiceID]; !ok {
			continue
		}
		sf := t.services[f.ServiceID]
		if sf == nil {
			sf = &serviceFailures{at: map[string]time.Time{}}
			t.services[f.ServiceID] = sf
		}
		if _, seen := sf.at[f.ID]; seen {
			continue
		}
		at := f.At
		if at.IsZero() {
			at = now
		}
		sf.at[f.ID] = at
		if !at.Before(sf.latestAt) {
			sf.latestAt, sf.latestError = at, f.Err
		}
	}

	out := map[string]RestartLoop{}
	for id, sf := range t.services {
		for task, at := range sf.at {
			if now.Sub(at) >= window {
				delete(sf.at, task)
			}
		}
		count := len(sf.at)
		switch {
		case count == 0:
			delete(t.services, id)
			continue
		case count >= threshold && sf.flaggedAt.IsZero():
			sf.flaggedAt = now
		}
		if sf.flaggedAt.IsZero() {
			continue
		}
		out[id] = RestartLoop{
			ServiceID: id, Name: names[id], Count: count,
			LatestError: sf.latestError, LatestAt: sf.latestAt, Since: sf.flaggedAt,
		}
	}
	return out
}
