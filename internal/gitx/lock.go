// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitx

import (
	"context"
	"sync"
)

// mirrorLock is a reader/writer lock whose waiting honours a context: a fetch takes it for writing and a
// read for reading, and a caller whose context ends stops waiting. sync.RWMutex cannot do that, and a
// lock that cannot be given up lets one stuck holder block every later fetch, read and removal of the
// mirror for good (WR-A01). A waiting writer keeps new readers out, so a stream of reads cannot starve
// a fetch.
type mirrorLock struct {
	mu       sync.Mutex
	readers  int
	writing  bool
	writersW int
	// changed is closed, and replaced, whenever the lock is released or a waiting writer gives up, so the
	// waiters look again.
	changed chan struct{}
}

func newMirrorLock() *mirrorLock { return &mirrorLock{changed: make(chan struct{})} }

// wake tells every waiter to look again. The caller holds l.mu.
func (l *mirrorLock) wake() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// lock waits for the lock for writing. It returns ctx's error, holding nothing, when ctx ends first.
func (l *mirrorLock) lock(ctx context.Context) error {
	l.mu.Lock()
	l.writersW++
	for {
		if !l.writing && l.readers == 0 {
			l.writersW--
			l.writing = true
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
			l.mu.Lock()
		case <-ctx.Done():
			l.mu.Lock()
			l.writersW--
			l.wake() // readers a waiting writer held back may go now
			l.mu.Unlock()
			return ctx.Err()
		}
	}
}

func (l *mirrorLock) unlock() {
	l.mu.Lock()
	l.writing = false
	l.wake()
	l.mu.Unlock()
}

// rlock waits for the lock for reading. It returns ctx's error, holding nothing, when ctx ends first.
func (l *mirrorLock) rlock(ctx context.Context) error {
	l.mu.Lock()
	for {
		if !l.writing && l.writersW == 0 {
			l.readers++
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
			l.mu.Lock()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (l *mirrorLock) runlock() {
	l.mu.Lock()
	l.readers--
	if l.readers == 0 {
		l.wake()
	}
	l.mu.Unlock()
}
