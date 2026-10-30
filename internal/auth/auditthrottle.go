// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package auth

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
)

// AuditRecorder is where an audit entry is written. *audit.Writer implements
// it, and so does *AuditThrottle, so the throttle can sit in front of a writer
// wherever an httpx.Auditor is accepted.
type AuditRecorder interface {
	Record(ctx context.Context, e audit.Entry) error
}

// defaultMaxPending bounds how many distinct suppressed-refusal keys the
// throttle holds between flushes (T-01-G05).
const defaultMaxPending = 4096

// Caps on the attacker-controlled text a pending entry keeps in memory. They
// match the caps audit.Writer applies when it writes the row.
const (
	maxPendingName   = 128
	maxPendingReason = 512
	maxPendingAgent  = 512
)

// AuditThrottle stands between anonymous refusals and the audit writer. Every
// audit row is one write on the store's single write connection, so an
// anonymous client that can make the server refuse it in a loop could starve
// every signed-in user of that connection (WR-04). The throttle gives each
// client address (an IPv6 /64 counts as one) a token-bucket budget of audit
// rows. A refusal inside the budget is written at once. A refusal over it is
// counted in memory and written later as one summary row per action, reason,
// target and address, carrying a "suppressed" count and the entry's other
// details, so nothing is dropped: D-49 and T-01-35 still hold, with the count
// standing in for the individual rows.
//
// The rows one address can cost the writer are therefore bounded by its burst,
// plus its refill, plus one summary per key per flush. Actors that are signed
// in, and system principals, are never counted: they are already
// authenticated and their entries pass straight to the writer.
//
// A summary is made only of fields of an entry that already went through
// audit.Writer's checks when it was first refused, plus the numeric
// "suppressed" detail and the RFC 3339 "since" of the first suppressed refusal;
// no secret value can enter it (T-01-G06 and the plan 01-07 prohibition).
type AuditThrottle struct {
	next AuditRecorder
	lim  *IPLimiter
	log  *slog.Logger
	now  func() time.Time

	mu         sync.Mutex
	pending    map[string]*suppressed
	maxPending int
}

// suppressed is the first over-budget entry for one key and how many there were.
type suppressed struct {
	entry audit.Entry
	count int
	since time.Time
}

// NewAuditThrottle returns a throttle that writes to next and spends lim, which
// is keyed on the entry's address. A nil now means time.Now; a nil log means
// slog.Default.
func NewAuditThrottle(next AuditRecorder, lim *IPLimiter, log *slog.Logger, now func() time.Time) *AuditThrottle {
	if log == nil {
		log = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &AuditThrottle{
		next: next, lim: lim, log: log, now: now,
		pending: make(map[string]*suppressed), maxPending: defaultMaxPending,
	}
}

// Record writes e through to the next recorder when it is within its address's
// budget or comes from a signed-in actor, and otherwise counts it for the next
// Flush and returns nil.
func (t *AuditThrottle) Record(ctx context.Context, e audit.Entry) error {
	if e.Actor.Type != audit.ActorAnonymous || t.lim.Allow(e.IP) {
		return t.next.Record(ctx, e)
	}
	t.count(e)
	return nil
}

// overflowMarker makes the per-action overflow key: it cannot collide with a
// real key because real keys carry four NUL-separated parts.
const overflowMarker = "\x00overflow"

func (t *AuditThrottle) count(e audit.Entry) {
	e = trimmed(e)
	key := strings.Join([]string{e.Action, e.Reason, e.TargetID, limiterKey(e.IP)}, "\x00")
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.pending[key]; ok {
		s.count++
		return
	}
	if len(t.pending) >= t.maxPending {
		okey := e.Action + overflowMarker
		if s, ok := t.pending[okey]; ok {
			s.count++
			return
		}
		// One overflow row per action: the addresses are no longer named, but
		// the refusals are still counted.
		o := e
		o.IP = ""
		o.Details = copyDetails(e.Details)
		o.Details["sources"] = "many"
		t.pending[okey] = &suppressed{entry: o, count: 1, since: t.now()}
		return
	}
	t.pending[key] = &suppressed{entry: e, count: 1, since: t.now()}
}

// Flush writes one summary row for every key with suppressed refusals and
// clears them. If a write fails, that key and every unwritten one stay pending
// for the next Flush and the error is returned.
func (t *AuditThrottle) Flush(ctx context.Context) error {
	t.mu.Lock()
	batch := t.pending
	t.pending = make(map[string]*suppressed)
	t.mu.Unlock()

	var firstErr error
	for key, s := range batch {
		if firstErr == nil {
			e := s.entry
			e.Details = copyDetails(s.entry.Details)
			e.Details["suppressed"] = strconv.Itoa(s.count)
			e.Details["since"] = s.since.UTC().Format(time.RFC3339)
			if err := t.next.Record(ctx, e); err == nil {
				continue
			} else {
				firstErr = err
			}
		}
		t.restore(key, s)
	}
	return firstErr
}

// restore puts an unwritten key back, adding to any count that arrived while
// the flush ran.
func (t *AuditThrottle) restore(key string, s *suppressed) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.pending[key]; ok {
		cur.count += s.count
		if s.since.Before(cur.since) {
			cur.since = s.since
		}
		return
	}
	t.pending[key] = s
}

// Run flushes every interval until ctx is done, then once more with a fresh
// short context, so refusals counted just before shutdown still reach the log.
func (t *AuditThrottle) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := t.Flush(final); err != nil {
				t.log.Error("could not write the final audit summary", "error", err)
			}
			return
		case <-tick.C:
			if err := t.Flush(ctx); err != nil {
				t.log.Error("could not write an audit summary", "error", err)
			}
		}
	}
}

func copyDetails(d map[string]string) map[string]string {
	out := make(map[string]string, len(d)+3)
	for k, v := range d {
		out[k] = v
	}
	return out
}

// trimmed cuts the attacker-controlled strings of a pending entry to the sizes
// audit.Writer would store, so the pending map cannot hold more than its cap
// times a few hundred bytes. audit.Writer repairs a cut multi-byte character.
func trimmed(e audit.Entry) audit.Entry {
	e.TargetID = cut(e.TargetID, maxPendingName)
	e.EnvID = cut(e.EnvID, maxPendingName)
	e.Reason = cut(e.Reason, maxPendingReason)
	e.UserAgent = cut(e.UserAgent, maxPendingAgent)
	e.Actor.ID = cut(e.Actor.ID, maxPendingName)
	e.Actor.Name = cut(e.Actor.Name, maxPendingName)
	e.IP = cut(e.IP, 64)
	return e
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
