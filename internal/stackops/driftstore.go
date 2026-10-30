// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"sync"
	"time"
)

// driftCheckTimeout bounds one shared check of a stack. The check is detached from the caller that started it: the
// callers that joined it must not lose it because the first one went away.
const driftCheckTimeout = 30 * time.Second

// DriftStore holds the newest drift result of each stack, in memory (Docker is the source of truth, so a restart
// loses nothing that cannot be read again), and shares one check among the callers that ask for the same stack at once.
type DriftStore struct {
	mu      sync.Mutex
	results map[string]DriftResult
	calls   map[string]*driftCall

	// Joined, when set before the store is used, is called each time a caller starts or joins the check of a stack,
	// with how many callers wait on it now. A test uses it to know that every caller has joined before it lets the
	// check finish.
	Joined func(stackID string, waiting int)
}

// driftCall is a check in flight: waiters is how many callers wait on it, and res and err are set before done closes.
type driftCall struct {
	done    chan struct{}
	waiters int
	res     DriftResult
	err     error
}

// NewDriftStore is an empty store.
func NewDriftStore() *DriftStore {
	return &DriftStore{results: map[string]DriftResult{}, calls: map[string]*driftCall{}}
}

// Get is the newest result for the stack, and false when it has none.
func (s *DriftStore) Get(stackID string) (DriftResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.results[stackID]
	return r, ok
}

// Put records the newest result for the stack.
func (s *DriftStore) Put(stackID string, r DriftResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[stackID] = r
}

// Forget drops the stack's result, such as when the stack is removed.
func (s *DriftStore) Forget(stackID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.results, stackID)
}

// Check runs fn for the stack and records its result. Callers that arrive while a check of the same stack runs share
// it: fn runs once and each gets its result. The check is not tied to the first caller's context (a caller that gives
// up leaves it running for the others), and a caller returns early with its own context's error when it gives up
// itself. A failed check records nothing.
func (s *DriftStore) Check(ctx context.Context, stackID string, fn func(context.Context) (DriftResult, error)) (DriftResult, error) {
	s.mu.Lock()
	c, joined := s.calls[stackID]
	if joined {
		c.waiters++
	} else {
		c = &driftCall{done: make(chan struct{}), waiters: 1}
		s.calls[stackID] = c
	}
	waiting := c.waiters
	s.mu.Unlock()
	if s.Joined != nil {
		s.Joined(stackID, waiting)
	}
	if !joined {
		go s.run(context.WithoutCancel(ctx), stackID, c, fn)
	}
	select {
	case <-ctx.Done():
		return DriftResult{}, ctx.Err()
	case <-c.done:
		return c.res, c.err
	}
}

// run is the one check of a call: it records a result that succeeded, then frees the stack for the next check.
func (s *DriftStore) run(ctx context.Context, stackID string, c *driftCall, fn func(context.Context) (DriftResult, error)) {
	ctx, cancel := context.WithTimeout(ctx, driftCheckTimeout)
	defer cancel()
	res, err := fn(ctx)
	s.mu.Lock()
	if err == nil {
		s.results[stackID] = res
	}
	delete(s.calls, stackID)
	c.res, c.err = res, err
	s.mu.Unlock()
	close(c.done)
}
