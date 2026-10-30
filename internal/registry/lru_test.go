// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"testing"
	"time"
)

// The bounded cache behind the tag name lists (plan 05-18, T-05-63).

func TestLRUDropsTheLeastRecentlyUsedEntryWhenFull(t *testing.T) {
	c := newLRU[string, int](2, nil)
	c.Put("a", 1, 0)
	c.Put("b", 2, 0)
	if _, ok := c.Get("a"); !ok { // a is now the most recently used
		t.Fatal("a is missing")
	}
	c.Put("c", 3, 0)
	if _, ok := c.Get("b"); ok {
		t.Error("b survived, want it dropped as the least recently used")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.Get(k); !ok {
			t.Errorf("%s was dropped", k)
		}
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d, want 2", c.Len())
	}
}

func TestLRUStopsServingAnEntryAfterItsTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newLRU[string, string](4, func() time.Time { return now })
	c.Put("k", "v", time.Minute)
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("Get = %q, %v, want a hit", v, ok)
	}
	now = now.Add(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Error("the entry expired early")
	}
	now = now.Add(2 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Error("the entry was served after its TTL")
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d, want the expired entry removed on read", c.Len())
	}
}

func TestLRUPutReplacesTheValueAndRestartsTheTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	c := newLRU[string, int](4, func() time.Time { return now })
	c.Put("k", 1, time.Minute)
	now = now.Add(50 * time.Second)
	c.Put("k", 2, time.Minute)
	now = now.Add(50 * time.Second)
	if v, ok := c.Get("k"); !ok || v != 2 {
		t.Errorf("Get = %d, %v, want 2 after the refresh", v, ok)
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1", c.Len())
	}
}
