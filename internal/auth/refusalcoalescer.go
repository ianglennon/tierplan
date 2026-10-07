// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package auth

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
)

// Defaults of the refusal coalescer (D-20). The window is how long repeats of one
// refusal are folded into a count; the burst threshold is how many refusals of
// one key inside a window make an admin warning; keep is how long that warning
// stays; maxKeys bounds the open windows.
const (
	defaultCoalesceWindow = 60 * time.Second
	defaultBurstThreshold = 10
	defaultBurstKeep      = time.Hour
	defaultMaxCoalesced   = 4096
	// maxBursts bounds the remembered bursts, whatever their age.
	maxBursts = 256
)

// Burst is a user's refusals that came fast enough to be worth an admin's
// attention: Count refusals of one thing inside one window.
type Burst struct {
	ActorID, ActorName string
	Count              int
	At                 time.Time
}

// RefusalCoalescer stands in front of the audit writer for signed-in users'
// permission.denied entries. With real users holding lower roles, a tab that
// refreshes against a forbidden route could refuse thousands of identical
// requests, and each audit row is one write on the store's single write
// connection (D-20, T-03-36).
//
// The first refusal for each (user, action, reason, target, environment) is
// written in full, synchronously, and a failed write is returned so the caller
// still answers 500 and nothing goes unaudited. Identical refusals inside the
// window cost a counter increment; when the window closes, one summary row
// carrying count, first, last and coalesced=true is written. Pending counts are
// flushed on shutdown, so a hard kill loses only the count of repeats, never the
// first refusal (RESEARCH Pitfall 5).
//
// Every other entry passes straight through: anonymous refusals have their own
// per-address throttle (AuditThrottle) and system principals and other actions
// are never counted. When the table of open windows is full a new key is written
// through at once instead of being dropped.
type RefusalCoalescer struct {
	next   AuditRecorder
	log    *slog.Logger
	now    func() time.Time
	window time.Duration
	burst  int
	keep   time.Duration
	maxKey int

	mu         sync.Mutex
	open       map[string]*refusalWindow
	bursts     []Burst
	fullLogged bool
}

// refusalWindow is the first refusal of one key and the repeats since.
type refusalWindow struct {
	entry       audit.Entry
	first, last time.Time
	repeats     int
	burstNoted  bool
}

// CoalescerOption adjusts a RefusalCoalescer.
type CoalescerOption func(*RefusalCoalescer)

// WithWindow sets how long repeats are folded into one entry. A value of zero or
// less is ignored.
func WithWindow(d time.Duration) CoalescerOption {
	return func(c *RefusalCoalescer) {
		if d > 0 {
			c.window = d
		}
	}
}

// WithBurstThreshold sets how many refusals of one key inside a window raise a
// burst. A value below one is ignored.
func WithBurstThreshold(n int) CoalescerOption {
	return func(c *RefusalCoalescer) {
		if n > 0 {
			c.burst = n
		}
	}
}

// WithMaxKeys sets how many windows may be open at once. A value below one is
// ignored.
func WithMaxKeys(n int) CoalescerOption {
	return func(c *RefusalCoalescer) {
		if n > 0 {
			c.maxKey = n
		}
	}
}

// NewRefusalCoalescer returns a coalescer that writes to next. A nil now means
// time.Now; a nil log means slog.Default.
func NewRefusalCoalescer(next AuditRecorder, log *slog.Logger, now func() time.Time, opts ...CoalescerOption) *RefusalCoalescer {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	c := &RefusalCoalescer{
		next: next, log: log, now: now,
		window: defaultCoalesceWindow, burst: defaultBurstThreshold,
		keep: defaultBurstKeep, maxKey: defaultMaxCoalesced,
		open: make(map[string]*refusalWindow),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// coalesces says whether e is the kind of entry that is folded: a refusal of a
// signed-in user.
func coalesces(e audit.Entry) bool {
	return e.Action == audit.ActionPermissionDenied && e.Actor.Type == audit.ActorUser
}

func refusalKey(e audit.Entry) string {
	return strings.Join([]string{e.Actor.ID, e.Action, e.Reason, e.TargetID, e.EnvID}, "\x00")
}

// Record writes e through when it is not a signed-in user's refusal or is the
// first of its key in the window, and otherwise counts it and returns nil. The
// error of a write is returned and opens no window, so the next identical
// refusal is written in full again.
func (c *RefusalCoalescer) Record(ctx context.Context, e audit.Entry) error {
	if !coalesces(e) {
		return c.next.Record(ctx, e)
	}
	e = trimmed(e)
	key := refusalKey(e)

	c.mu.Lock()
	if w, ok := c.open[key]; ok {
		w.repeats++
		w.last = c.now()
		c.mu.Unlock()
		return nil
	}
	full := len(c.open) >= c.maxKey
	if full && !c.fullLogged {
		c.fullLogged = true
		c.log.Warn("the refusal coalescer is full; new refusals are written one by one", "open_windows", len(c.open))
	}
	c.mu.Unlock()

	if err := c.next.Record(ctx, e); err != nil {
		return err
	}
	if full {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// An identical refusal that raced this one has already opened the window and
	// was written in full too; keep that one.
	if _, ok := c.open[key]; !ok && len(c.open) < c.maxKey {
		at := c.now()
		stored := e
		stored.Details = copyDetails(e.Details)
		c.open[key] = &refusalWindow{entry: stored, first: at, last: at}
	}
	return nil
}

// Sweep closes every window that has been open for the window length and writes
// one summary row for each that saw repeats. A window whose write fails is put
// back, to be written with the counts that arrive meanwhile at the next sweep,
// and the first error is returned.
func (c *RefusalCoalescer) Sweep(ctx context.Context) error { return c.sweep(ctx, false) }

// Flush closes every window whatever its age. Run calls it on shutdown.
func (c *RefusalCoalescer) Flush(ctx context.Context) error { return c.sweep(ctx, true) }

func (c *RefusalCoalescer) sweep(ctx context.Context, all bool) error {
	c.mu.Lock()
	now := c.now()
	closing := make(map[string]*refusalWindow)
	for key, w := range c.open {
		if all || now.Sub(w.first) >= c.window {
			closing[key] = w
			delete(c.open, key)
			if w.repeats+1 >= c.burst && !w.burstNoted {
				w.burstNoted = true
				c.bursts = append(c.bursts, Burst{ActorID: w.entry.Actor.ID, ActorName: w.entry.Actor.Name, Count: w.repeats + 1, At: w.last})
			}
		}
	}
	c.pruneBurstsLocked(now)
	if len(c.open) < c.maxKey {
		c.fullLogged = false
	}
	c.mu.Unlock()

	var firstErr error
	for key, w := range closing {
		if w.repeats == 0 {
			continue
		}
		if firstErr == nil {
			e := w.entry
			e.Details = copyDetails(w.entry.Details)
			e.Details["count"] = strconv.Itoa(w.repeats)
			e.Details["first"] = w.first.UTC().Format(time.RFC3339)
			e.Details["last"] = w.last.UTC().Format(time.RFC3339)
			e.Details["coalesced"] = "true"
			if err := c.next.Record(ctx, e); err == nil {
				continue
			} else {
				firstErr = err
			}
		}
		c.restore(key, w)
	}
	return firstErr
}

// restore puts an unwritten window back, adding to any window that opened while
// the sweep ran.
func (c *RefusalCoalescer) restore(key string, w *refusalWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur, ok := c.open[key]; ok {
		cur.repeats += w.repeats
		if w.first.Before(cur.first) {
			cur.first = w.first
		}
		if w.last.After(cur.last) {
			cur.last = w.last
		}
		cur.burstNoted = cur.burstNoted || w.burstNoted
		return
	}
	c.open[key] = w
}

// pruneBurstsLocked drops bursts older than keep and the oldest beyond the cap.
func (c *RefusalCoalescer) pruneBurstsLocked(now time.Time) {
	c.bursts = slices.DeleteFunc(c.bursts, func(b Burst) bool { return now.Sub(b.At) >= c.keep })
	if over := len(c.bursts) - maxBursts; over > 0 {
		c.bursts = c.bursts[over:]
	}
}

// Bursts lists the bursts younger than an hour, newest first.
func (c *RefusalCoalescer) Bursts(now time.Time) []Burst {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Burst, 0, len(c.bursts))
	for i := len(c.bursts) - 1; i >= 0; i-- {
		if b := c.bursts[i]; now.Sub(b.At) < c.keep {
			out = append(out, b)
		}
	}
	return out
}

// Pending is how many windows are open.
func (c *RefusalCoalescer) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.open)
}

// Run sweeps every interval until ctx is done, then flushes every open window
// with a fresh short context, so repeat counts just before shutdown still reach
// the log.
func (c *RefusalCoalescer) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Flush(final); err != nil {
				c.log.Error("could not write the final refusal summary", "error", err)
			}
			return
		case <-tick.C:
			if err := c.Sweep(ctx); err != nil {
				c.log.Error("could not write a refusal summary", "error", err)
			}
		}
	}
}
