// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"sync"
	"time"
)

// HolderKindJanitor is the Kind of the Holder of a janitor run (Phase 11). Such a holder names no
// stack, so its StackID and StackName are empty and its RunID names the run; a message that names
// the holder says the janitor is cleaning the environment.
const HolderKindJanitor = "janitor"

// Holder is who holds an environment's operation lock: the stack, the job that runs the operation,
// what it is (deploy, update, rollback, start, stop or remove), who started it and when.
//
// A janitor run (Kind HolderKindJanitor) names no stack: it names the run it is, in RunID.
type Holder struct {
	StackID, StackName, JobID, Kind, By string
	// RunID is the janitor run that holds the lock, empty for every other Kind.
	RunID   string
	Started time.Time
}

// OperationLock lets one stack operation at a time run on an environment (STK-09, T-07-87). A
// deploy, an update of images, a roll back, a stop, a start and a removal each take it for the life
// of their job, and the janitor of Phase 11 takes the same lock, so a clean-up never runs under a
// deploy. An operation that cannot take it is refused, never queued, and is told who holds it.
type OperationLock interface {
	// TryAcquire takes the lock of an environment for h and returns the function that frees it, or,
	// when the environment is held, the holder and false. Freeing is safe to repeat and frees only the
	// lock it was returned for.
	TryAcquire(envID string, h Holder) (release func(), held Holder, ok bool)
	// Attach names the job that runs the operation holding the environment's lock, once the job has
	// started. It changes nothing when the environment is not held.
	Attach(envID, jobID string)
	// Held returns the holder of an environment's lock.
	Held(envID string) (Holder, bool)
}

// Locks is the in-memory OperationLock: a lock for each environment, none of which blocks another.
// It is memory only on purpose. A server that stops takes its jobs with it, and the deploys they
// were running are marked interrupted when it starts again (Repo.MarkInterrupted), so a lock never
// has to outlive the process.
type Locks struct {
	mu   sync.Mutex
	held map[string]lease
	next uint64
}

// lease is a holder and the token of the TryAcquire that made it, so a release frees only its own.
type lease struct {
	holder Holder
	token  uint64
}

// NewLocks makes an empty set of locks.
func NewLocks() *Locks { return &Locks{held: map[string]lease{}} }

var _ OperationLock = (*Locks)(nil)

// TryAcquire implements OperationLock.
func (l *Locks) TryAcquire(envID string, h Holder) (func(), Holder, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.held[envID]; ok {
		return nil, cur.holder, false
	}
	l.next++
	token := l.next
	l.held[envID] = lease{holder: h, token: token}
	release := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.held[envID]; ok && cur.token == token {
			delete(l.held, envID)
		}
	}
	return release, Holder{}, true
}

// Attach implements OperationLock.
func (l *Locks) Attach(envID, jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.held[envID]; ok {
		cur.holder.JobID = jobID
		l.held[envID] = cur
	}
}

// Held implements OperationLock.
func (l *Locks) Held(envID string) (Holder, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cur, ok := l.held[envID]
	return cur.holder, ok
}
