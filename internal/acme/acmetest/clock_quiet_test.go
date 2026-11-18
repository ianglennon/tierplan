// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acmetest

import (
	"testing"
	"time"
)

// A quiet view of the clock shares its time and fires on the same Advance, and arming one announces nothing, so a test that
// waits for the other consumer's timer is not answered by it.
func TestQuietClockSharesTimeAndAnnouncesNothing(t *testing.T) {
	t.Parallel()
	c := NewClock(epoch)
	q := c.Quiet()
	ch := q.After(10 * time.Minute)
	select {
	case <-c.armed:
		t.Fatal("arming a quiet timer was announced")
	default:
	}
	c.Advance(10 * time.Minute)
	select {
	case at := <-ch:
		if !at.Equal(epoch.Add(10*time.Minute)) || !q.Now().Equal(c.Now()) {
			t.Fatalf("quiet timer fired with %v, quiet Now %v, clock Now %v", at, q.Now(), c.Now())
		}
	default:
		t.Fatal("a due quiet timer did not fire on Advance")
	}
	_ = c.After(time.Minute)
	select {
	case <-c.armed:
	default:
		t.Fatal("the loud timer was not announced")
	}
}
