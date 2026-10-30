// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

// triggerRun is the work a Trigger has in hand for one stack: running is always true while the entry exists, and
// pending says another trigger arrived since the check began.
type triggerRun struct {
	pending bool
}

// Trigger asks for a check of a stack's repository and returns at once. It is what a webhook delivery does: it names no
// ref and no commit, because nothing the request says is believed; the check reads the stack's own ref (D-15).
//
// The check runs on a context of its own, detached from the request and ended by Shutdown. While a check that a trigger started runs, any
// number of further triggers for the stack collapse into one more check after it, so a burst of pushes costs one check
// and one trailing check. A check that was already running when the first trigger came (a poll, or a person) may have
// begun before the push, so it is waited for and a fresh one follows it.
func (s *Service) Trigger(envID, stackID, trigger string) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	if run, ok := s.triggers[stackID]; ok {
		run.pending = true
		s.mu.Unlock()
		return
	}
	s.triggers[stackID] = &triggerRun{}
	s.work.Add(1) // Shutdown waits for it (WR-B01)
	s.mu.Unlock()
	go func() {
		defer s.work.Done()
		s.runTriggered(envID, stackID, trigger)
	}()
}

// runTriggered runs on the service's own life, so that Shutdown ends it, and not on a request's context.
func (s *Service) runTriggered(envID, stackID, trigger string) {
	ctx := s.life
	if s.InFlight(stackID) {
		// It began before this push; its answer may not show the push. Wait for it, then look again.
		_, _ = s.CheckWith(ctx, envID, stackID, CheckOptions{Trigger: trigger})
	}
	for {
		out, err := s.CheckWith(ctx, envID, stackID, CheckOptions{Trigger: trigger})
		if err != nil && !s.shuttingDown() {
			s.log.Error("a check started by a webhook failed", "stack", stackID, "error", err)
		}
		s.mu.Lock()
		run := s.triggers[stackID]
		more := run.pending
		if more {
			run.pending = false
		} else {
			delete(s.triggers, stackID)
		}
		s.mu.Unlock()
		if s.triggered != nil {
			s.triggered(stackID, out, err, more)
		}
		if !more {
			return
		}
	}
}
