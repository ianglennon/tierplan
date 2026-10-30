// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"context"
	"strings"
	"testing"
	"time"
)

// poll is testsupport.Poll with the failure handed back as an error: it returns nil once the
// condition holds, names what it waited for and the last answer when it times out, and gives up
// when its context ends.
func TestPollReturnsNilOnceTheConditionHolds(t *testing.T) {
	calls := 0
	err := poll(context.Background(), 5*time.Second, "three calls", func() (bool, string) {
		calls++
		return calls == 3, "not yet"
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func TestPollTimeoutNamesWhatItAwaitedAndTheLastAnswer(t *testing.T) {
	err := poll(context.Background(), 50*time.Millisecond, "the engine", func() (bool, string) { return false, "refused" })
	if err == nil {
		t.Fatal("poll returned nil for a condition that never holds")
	}
	for _, want := range []string{"waiting for the engine", "last seen: refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestPollGivesUpWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := poll(ctx, time.Minute, "the engine", func() (bool, string) { return false, "" })
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("poll with an ended context returned %v", err)
	}
}
