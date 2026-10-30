// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package gitx

import (
	"context"
	"errors"
	"testing"
)

// The mirror lock gives up with the caller's context (WR-A01): a holder that never lets go cannot block the
// callers behind it for good.

func TestMirrorLockWaitEndsWithTheContext(t *testing.T) {
	t.Parallel()
	l := newMirrorLock()
	if err := l.lock(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.lock(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a second writer with a cancelled context = %v, want context.Canceled", err)
	}
	if err := l.rlock(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a reader with a cancelled context = %v, want context.Canceled", err)
	}
	// Neither left anything behind: the holder lets go and the lock is free again.
	l.unlock()
	if err := l.lock(context.Background()); err != nil {
		t.Errorf("lock after the holder let go: %v", err)
	}
	l.unlock()
}

func TestMirrorLockSharesReadersAndExcludesAWriter(t *testing.T) {
	t.Parallel()
	l := newMirrorLock()
	bg := context.Background()
	if err := l.rlock(bg); err != nil {
		t.Fatal(err)
	}
	if err := l.rlock(bg); err != nil {
		t.Fatalf("a second reader: %v", err)
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	if err := l.lock(cancelled); err == nil {
		t.Error("a writer got the lock while readers held it")
	}
	l.runlock()
	l.runlock()
	if err := l.lock(bg); err != nil {
		t.Fatalf("writer after the readers left: %v", err)
	}
	if err := l.rlock(cancelled); err == nil {
		t.Error("a reader got the lock while a writer held it")
	}
	l.unlock()
}

// A writer that gave up must not keep readers out for good.
func TestMirrorLockAWriterThatGaveUpDoesNotBlockReaders(t *testing.T) {
	t.Parallel()
	l := newMirrorLock()
	bg := context.Background()
	if err := l.rlock(bg); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	if err := l.lock(cancelled); err == nil {
		t.Fatal("a writer got the lock while a reader held it")
	}
	if err := l.rlock(bg); err != nil {
		t.Errorf("a reader after a writer gave up: %v", err)
	}
	l.runlock()
	l.runlock()
}

// Readers that waited for a writer get in once it lets go.
func TestMirrorLockWaitersAreWokenWhenTheHolderLetsGo(t *testing.T) {
	t.Parallel()
	l := newMirrorLock()
	bg := context.Background()
	if err := l.lock(bg); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 2)
	for range 2 {
		go func() {
			err := l.rlock(bg)
			if err == nil {
				l.runlock()
			}
			got <- err
		}()
	}
	l.unlock()
	for range 2 {
		if err := <-got; err != nil {
			t.Errorf("waiting reader: %v", err)
		}
	}
}
