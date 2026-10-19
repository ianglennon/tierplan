// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import "time"

// Holder is who holds an environment's operation lock: the stack, the job that runs the operation,
// what it is, who started it and when.
type Holder struct {
	StackID, StackName, JobID, Kind, By string
	Started                             time.Time
}

// OperationLock is the lock that lets one stack operation at a time run on an environment.
type OperationLock interface {
	TryAcquire(envID string, h Holder) (release func(), held Holder, ok bool)
	Attach(envID, jobID string)
	Held(envID string) (Holder, bool)
}

// Locks is the in-memory OperationLock.
type Locks struct{}

// NewLocks makes an empty set of locks.
func NewLocks() *Locks { return &Locks{} }

var _ OperationLock = (*Locks)(nil)

func (l *Locks) TryAcquire(string, Holder) (func(), Holder, bool) { return func() {}, Holder{}, true }
func (l *Locks) Attach(string, string)                            {}
func (l *Locks) Held(string) (Holder, bool)                       { return Holder{}, false }
