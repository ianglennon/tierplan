// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package registry

import (
	"container/list"
	"sync"
	"time"
)

// lru is a size-bounded cache whose entries can expire. It is safe for concurrent use. The
// least recently used entry is dropped when the cache is full, so a long-running server holds
// a bounded number of tag lists however many repositories are browsed.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	size  int
	now   func() time.Time
	order *list.List // front is the most recently used
	items map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
	// exp is when the entry stops being served; the zero time never expires.
	exp time.Time
}

// newLRU returns a cache that holds at most size entries. now is the clock, injected for
// tests; nil means time.Now.
func newLRU[K comparable, V any](size int, now func() time.Time) *lru[K, V] {
	if size < 1 {
		size = 1
	}
	if now == nil {
		now = time.Now
	}
	return &lru[K, V]{size: size, now: now, order: list.New(), items: map[K]*list.Element{}}
}

// Get returns the value stored under k and true, or the zero value and false when there is
// none or it has expired. A hit makes the entry the most recently used.
func (c *lru[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		var zero V
		return zero, false
	}
	e := el.Value.(*lruEntry[K, V]) //nolint:errcheck,forcetypeassert // only this type is ever stored
	if !e.exp.IsZero() && !c.now().Before(e.exp) {
		c.remove(el)
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

// Put stores v under k for ttl; a ttl of zero or less keeps it until it is pushed out.
func (c *lru[K, V]) Put(k K, v V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = c.now().Add(ttl)
	}
	if el, ok := c.items[k]; ok {
		e := el.Value.(*lruEntry[K, V]) //nolint:errcheck,forcetypeassert // only this type is ever stored
		e.val, e.exp = v, exp
		c.order.MoveToFront(el)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry[K, V]{key: k, val: v, exp: exp})
	for c.order.Len() > c.size {
		c.remove(c.order.Back())
	}
}

// Len is the number of entries held, expired ones included until they are next read or
// pushed out.
func (c *lru[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *lru[K, V]) remove(el *list.Element) {
	e := c.order.Remove(el).(*lruEntry[K, V]) //nolint:errcheck,forcetypeassert // only this type is ever stored
	delete(c.items, e.key)
}
