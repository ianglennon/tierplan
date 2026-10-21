// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The states of a swarm service's UpdateStatus and of a swarm task that convergence reads.
const (
	// UpdateNone is the state of a service with no UpdateStatus: never updated, or only scaled
	// (RESEARCH E9).
	UpdateNone = "none"

	updateUpdating        = "updating"
	updatePaused          = "paused"
	updateRollbackStarted = "rollback_started"
	updateRollbackPaused  = "rollback_paused"
	updateRollbackDone    = "rollback_completed"

	// The states a task is read in.
	TaskRunning   = "running"
	TaskFailed    = "failed"
	TaskRejected  = "rejected"
	taskPreparing = "preparing"
	taskComplete  = "complete"

	modeReplicated = "replicated"
)

// PollInterval is how often a deploy reads a service while it waits for it to settle, and how often
// the console's task rows refresh (D-13).
const PollInterval = 2 * time.Second

// exhaustedReads is how many reads in a row must find a service exhausted before the wait gives up on
// it. The swarm adds a task to a dead slot when the service is written again (a redeploy of an
// unchanged spec, a Start), and the first read after the write can come before it does.
const exhaustedReads = 2

// maxPollErrors is how many reads in a row may fail before the wait gives up: a manager that fails
// over is back within a poll or two, and one that is not should not make a deploy wait out its whole
// timeout to say so.
const maxPollErrors = 3

var (
	// ErrConvergeTimeout means the service had not settled when the timeout passed. Docker may still
	// finish it.
	ErrConvergeTimeout = errors.New("stackops: the service did not settle in time")
	// ErrUpdateStopped means the swarm paused the update of the service or rolled it back.
	ErrUpdateStopped = errors.New("stackops: the swarm stopped the service's update")
	// ErrTasksExhausted means every task of the service on its current template ended and the swarm will
	// not start another by itself: the restart policy is used up, or it says never to restart.
	ErrTasksExhausted = errors.New("stackops: the swarm will not start the service's tasks again")
)

// Convergence is how far a service has got toward running the spec it has. It is the counting of the
// service rollout endpoint (Phase 6), moved here so a deploy waits on exactly what the page shows.
type Convergence struct {
	// Running counts the tasks the swarm wants running that run, on the service's current task
	// template: the new version after an update, the earlier one after a rollback. Desired counts the
	// tasks the swarm wants running.
	Running, Desired int
	// Want is how many tasks must run for the service to be settled: its replica count when it is
	// replicated (the tasks of a service just written may not all exist yet), else Desired.
	Want int
	// Mode is the service's mode: replicated, global, replicated-job or global-job.
	Mode string
	// State is the UpdateStatus state, UpdateNone when the service has none. Message is the swarm's
	// own words about the update. Both are Docker's text: a caller that shows them cleans them.
	State, Message string
	// Err is the error text of the newest failed or rejected task, Docker's free text.
	Err string
	// Live counts the slots (replicated, keyed by TaskRow.Slot) or nodes (any other mode, keyed by NodeID)
	// that hold a task the swarm still means to run: one desired in a state up to running, a restart in its
	// delay (desired ready) included.
	Live int
	// Dead counts the slots or nodes that hold no such task and whose newest task on the current template
	// ended failed, rejected or complete: the swarm will not start another task there by itself.
	Dead int
	// DeadErr is the swarm's words about the newest of those dead tasks, its Err, else its Message. It is
	// Docker's text: a caller that shows it cleans it.
	DeadErr string
	// Template is the hash of the service's current task template: the SpecHash of a task that runs
	// it.
	Template string
	// Pulling is true while a task on the current template is preparing, which is where the swarm
	// pulls the image.
	Pulling bool
	// update is the status this was read from, to tell a stale one from a new one.
	update *dockerx.ServiceUpdateStatus
}

// Progress counts how far a service has got from its inspect and its tasks (newest first).
func Progress(detail dockerx.ServiceDetail, rows []dockerx.TaskRow) Convergence {
	c := Convergence{State: UpdateNone, Mode: detail.Mode, Template: detail.TemplateHash}
	if u := detail.UpdateStatus; u != nil && u.State != "" {
		c.State, c.Message, c.update = u.State, u.Message, u
	}
	latestSeen := false
	for _, r := range rows {
		if r.DesiredState == TaskRunning {
			c.Desired++
			current := detail.TemplateHash != "" && r.SpecHash == detail.TemplateHash
			if r.State == TaskRunning && current {
				c.Running++
			}
			if r.State == taskPreparing && current {
				c.Pulling = true
			}
		}
		if !latestSeen && (r.State == TaskFailed || r.State == TaskRejected) {
			latestSeen = true
			c.Err = r.Err
		}
	}
	c.Want = c.Desired
	if detail.Mode == modeReplicated {
		c.Want = int(min(detail.Replicas, math.MaxInt32))
	}
	c.countSlots(detail, rows)
	return c
}

// slotFacts is what the rows of one slot (replicated) or one node (any other mode) say.
type slotFacts struct {
	live bool
	// newest is the index in rows of the slot's newest task on the current template, -1 when it has none.
	newest int
}

// countSlots sets Live, Dead and DeadErr from the rows, newest first. Only a task on the current template
// can make a slot dead, so an earlier version's failures and a service with no task yet are never counted.
func (c *Convergence) countSlots(detail dockerx.ServiceDetail, rows []dockerx.TaskRow) {
	slots := map[string]*slotFacts{}
	for i, r := range rows {
		key := r.NodeID
		if detail.Mode == modeReplicated {
			key = strconv.Itoa(r.Slot)
		}
		f := slots[key]
		if f == nil {
			f = &slotFacts{newest: -1}
			slots[key] = f
		}
		if wantsToRun(r.DesiredState) {
			f.live = true
		}
		if f.newest < 0 && detail.TemplateHash != "" && r.SpecHash == detail.TemplateHash {
			f.newest = i
		}
	}
	deadAt := -1
	for _, f := range slots {
		if f.live {
			c.Live++
			continue
		}
		if f.newest < 0 {
			continue
		}
		switch rows[f.newest].State {
		case TaskFailed, TaskRejected, taskComplete:
			c.Dead++
			if deadAt < 0 || f.newest < deadAt {
				deadAt = f.newest
			}
		}
	}
	if deadAt >= 0 {
		c.DeadErr = firstWords(rows[deadAt].Err, rows[deadAt].Message)
	}
}

// wantsToRun reports whether a desired state is one at or before running: the swarm means the task to
// run, or to start. A replacement in its restart delay is desired ready.
func wantsToRun(desired string) bool {
	switch desired {
	case "new", "allocated", "pending", "assigned", "accepted", taskPreparing, "ready", "starting", TaskRunning:
		return true
	}
	return false
}

// IgnoringUpdate returns c as if the service had no update status when the status it has is
// the very one prior was read as before the deploy wrote the service: a paused or rolled-back
// update of an earlier deploy is not this deploy's. A status that differs in its state or its
// times is a new one.
func (c Convergence) IgnoringUpdate(prior *dockerx.ServiceUpdateStatus) Convergence {
	u := c.update
	if prior == nil || u == nil || u.State != prior.State || !u.StartedAt.Equal(prior.StartedAt) || !u.CompletedAt.Equal(prior.CompletedAt) {
		return c
	}
	c.State, c.Message, c.update = UpdateNone, "", nil
	return c
}

// Updating reports whether the swarm is still rolling an update or a rollback out.
func (c Convergence) Updating() bool {
	return c.State == updateUpdating || c.State == updateRollbackStarted
}

// Stopped reports whether the swarm paused the update or rolled it back: the service will not
// settle on this spec by itself.
func (c Convergence) Stopped() bool {
	return c.State == updatePaused || c.State == updateRollbackPaused || c.State == updateRollbackDone
}

// Exhausted reports whether the service can never settle because the swarm will not start its tasks
// again: some slot is dead, an update is not still rolling out, and the live slots are fewer than a
// replicated service's replica count, or, for a global service, no node is live. A global service with
// one dead node and the rest running is not stuck: its Want is Desired, so it settles, and it is not
// exhausted. A service with no task yet has no dead slot and is never exhausted.
func (c Convergence) Exhausted() bool {
	if c.Updating() || c.Dead == 0 {
		return false
	}
	if c.Mode == modeReplicated {
		return c.Live < c.Want
	}
	return c.Live == 0
}

// Settled reports whether the service runs what its spec says: every task it wants runs on the
// current template and no update is still rolling out. A global service with no task yet has not
// settled, because its tasks are made after the write; a replicated one scaled to zero has when
// its old tasks are gone.
func (c Convergence) Settled() bool {
	if c.Updating() || c.Stopped() || c.Exhausted() {
		return false
	}
	if c.Mode == modeReplicated {
		if c.Want == 0 {
			return c.Desired == 0
		}
		return c.Running >= c.Want
	}
	return c.Desired > 0 && c.Running >= c.Desired
}

// WaitConverged polls a service until it has settled and returns nil, until the swarm stops its
// update (ErrUpdateStopped), until the service is exhausted on exhaustedReads reads in a row
// (ErrTasksExhausted), until timeout has passed on clock (ErrConvergeTimeout), until ctx ends
// (its error), or until reads have failed maxPollErrors times in a row (the last error). poll reads
// the service and its tasks; on, when not nil, is called with every read that succeeded, so the
// caller can show it. The first read is made at once and the next every PollInterval of clock. It
// writes nothing.
func WaitConverged(ctx context.Context, clock Clock, timeout time.Duration,
	poll func(context.Context) (Convergence, []dockerx.TaskRow, error), on func(Convergence, []dockerx.TaskRow)) error {
	deadline := clock.Now().Add(timeout)
	failures, exhausted := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, rows, err := poll(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if failures++; failures >= maxPollErrors {
				return err
			}
		default:
			failures = 0
			if on != nil {
				on(c, rows)
			}
			if c.Stopped() {
				return ErrUpdateStopped
			}
			if c.Exhausted() {
				exhausted++
			} else {
				exhausted = 0
			}
			if exhausted >= exhaustedReads {
				return ErrTasksExhausted
			}
			if c.Settled() {
				return nil
			}
		}
		if !clock.Now().Before(deadline) {
			return ErrConvergeTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clock.After(PollInterval):
		}
	}
}
