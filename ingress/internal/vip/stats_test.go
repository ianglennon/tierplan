// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// instantClock is a clock whose waits are over at once: After moves time by the wait and returns a fired
// channel, so a poll's bounded wait for the file runs without a real delay.
type instantClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *instantClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *instantClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name) //nolint:gosec // G304: a fixed testdata name
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// statsText is a statistics file of the helper's instance with the given counters.
func statsText(c authCounters) string {
	return fmt.Sprintf(`VRRP Instance: %s
  Advertisements:
    Received: 5
    Sent: 0
  Authentication Errors:
    Invalid Type: 0
    Failure: 0
  Authentication Extension Errors:
    Missing: %d
    Malformed: %d
    Unknown Key: %d
    Invalid HMAC: %d
    Stale: %d
    Replay: %d
  Priority Zero:
    Received: 0
`, InstanceName, c.Missing, c.Malformed, c.UnknownKey, c.InvalidHMAC, c.Stale, c.Replay)
}

// statsRig is a statsWatcher over a fake signal and a fake file system: each signal "writes" the next dump of
// the queue (an empty string writes nothing, as a keepalived that did not answer), and a remove deletes it.
type statsRig struct {
	t       *testing.T
	w       *statsWatcher
	out     *syncBuf
	facts   *probeFacts
	mu      sync.Mutex
	file    *string
	queue   []string
	signals int
	sigErr  error
	opens   int
}

func newStatsRig(t *testing.T) *statsRig {
	t.Helper()
	r := &statsRig{t: t, out: newSyncBuf(), facts: &probeFacts{}}
	clock := &instantClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	cfg, err := Load(testEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r.w = &statsWatcher{
		clock: clock,
		em:    NewEmitter(r.out, cfg, clock),
		facts: r.facts,
		signal: func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.signals++
			if r.sigErr != nil {
				return r.sigErr
			}
			if len(r.queue) > 0 {
				next := r.queue[0]
				r.queue = r.queue[1:]
				if next != "" {
					r.file = &next
				}
			}
			return nil
		},
		remove: func(string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.file = nil
			return nil
		},
		open: func(path string) (io.ReadCloser, error) {
			if path != StatsPath {
				t.Errorf("opened %q, want %q", path, StatsPath)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			r.opens++
			if r.file == nil {
				return nil, os.ErrNotExist
			}
			return io.NopCloser(strings.NewReader(*r.file)), nil
		},
	}
	return r
}

// poll queues the dump and polls once.
func (r *statsRig) poll(dump string) {
	r.mu.Lock()
	r.queue = append(r.queue, dump)
	r.mu.Unlock()
	r.w.poll(context.Background())
}

func (r *statsRig) events() []ingressbundle.VIPEvent { return parseAll(r.t, r.out.lines()) }

// authLine is what keepalived logs for a failed advert, with the instance's own name.
func authLine(kind, addr string) string {
	return "Tue Nov 24 12:00:01 2026: (" + InstanceName + ") " + kind + " from " + addr
}

func TestParseStats(t *testing.T) {
	got, err := parseStats(strings.NewReader(testdata(t, "stats-invalid-hmac.txt")), InstanceName)
	if err != nil {
		t.Fatalf("parseStats: %v", err)
	}
	if want := (authCounters{InvalidHMAC: 7}); got != want {
		t.Fatalf("counters = %+v, want %+v", got, want)
	}
	// The same names appear under the plain authentication block and must not be read.
	got, err = parseStats(strings.NewReader(statsText(authCounters{Missing: 1, Malformed: 2, UnknownKey: 3, InvalidHMAC: 4, Stale: 5, Replay: 6})), InstanceName)
	if err != nil || got != (authCounters{Missing: 1, Malformed: 2, UnknownKey: 3, InvalidHMAC: 4, Stale: 5, Replay: 6}) {
		t.Fatalf("counters = %+v, %v", got, err)
	}
	for name, text := range map[string]string{
		"empty":              "",
		"another instance":   strings.ReplaceAll(testdata(t, "stats-clean.txt"), InstanceName, "OTHER"),
		"a counter missing":  strings.Replace(testdata(t, "stats-clean.txt"), "    Replay: 0\n", "", 1),
		"a counter unread":   strings.Replace(testdata(t, "stats-clean.txt"), "Invalid HMAC: 0", "Invalid HMAC: x", 1),
		"cut off in a value": strings.Replace(testdata(t, "stats-clean.txt"), "Stale: 0", "Stale:", 1),
	} {
		if _, err := parseStats(strings.NewReader(text), InstanceName); err == nil {
			t.Errorf("%s: parseStats accepted it", name)
		}
	}
	// A later section of the instance wins; a half-written later one is not read as the earlier.
	two := testdata(t, "stats-invalid-hmac.txt") + statsText(authCounters{InvalidHMAC: 9})
	if got, err := parseStats(strings.NewReader(two), InstanceName); err != nil || got.InvalidHMAC != 9 {
		t.Fatalf("two sections: %+v, %v", got, err)
	}
	cut := testdata(t, "stats-invalid-hmac.txt") + "VRRP Instance: " + InstanceName + "\n  Authentication Extension Errors:\n    Missing: 0\n"
	if _, err := parseStats(strings.NewReader(cut), InstanceName); err == nil {
		t.Error("a cut-off later section read as the earlier one")
	}
}

func TestForeignRouterFromStats(t *testing.T) {
	clean, hmac := testdata(t, "stats-clean.txt"), testdata(t, "stats-invalid-hmac.txt")

	t.Run("a rise writes one line with the counter and the sender, three quiet polls clear it", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll(clean)
		if l := r.out.lines(); len(l) != 0 {
			t.Fatalf("a clean poll wrote %q", l)
		}
		r.facts.scan([]byte(authLine("invalid authentication HMAC", "192.168.1.77")))
		r.poll(hmac)
		evs := r.events()
		if len(evs) != 1 {
			t.Fatalf("lines = %q, want one", r.out.lines())
		}
		ev := evs[0]
		if ev.Event != ingressbundle.VIPEventForeignRouter || !ev.Active || ev.Counter != ingressbundle.VIPCounterInvalidHMAC || ev.Addr != "192.168.1.77" {
			t.Fatalf("event = %+v", ev)
		}
		// The count stays at 7: no rise. Two quiet polls change nothing, the third clears.
		r.poll(hmac)
		r.poll(hmac)
		if n := len(r.out.lines()); n != 1 {
			t.Fatalf("%d lines after two quiet polls, want 1", n)
		}
		r.poll(hmac)
		evs = r.events()
		if len(evs) != 2 {
			t.Fatalf("lines = %q, want two", r.out.lines())
		}
		if ev := evs[1]; ev.Event != ingressbundle.VIPEventForeignRouter || ev.Active || ev.Counter != "" || ev.Addr != "" {
			t.Fatalf("clearing event = %+v", ev)
		}
		// Nothing more while it stays quiet.
		r.poll(hmac)
		if n := len(r.out.lines()); n != 2 {
			t.Fatalf("%d lines after clearing, want 2", n)
		}
		if r.signals != 6 {
			t.Fatalf("%d signals for six polls", r.signals)
		}
	})

	t.Run("a rise while it is active is not written again, and a rise resets the quiet count", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll(statsText(authCounters{InvalidHMAC: 1}))
		r.poll(statsText(authCounters{InvalidHMAC: 1})) // quiet 1
		r.poll(statsText(authCounters{InvalidHMAC: 1})) // quiet 2
		r.poll(statsText(authCounters{InvalidHMAC: 4})) // rise: quiet back to 0
		r.poll(statsText(authCounters{InvalidHMAC: 4})) // quiet 1
		r.poll(statsText(authCounters{InvalidHMAC: 4})) // quiet 2
		if n := len(r.out.lines()); n != 1 {
			t.Fatalf("lines = %q, want one", r.out.lines())
		}
		r.poll(statsText(authCounters{InvalidHMAC: 4})) // quiet 3
		if evs := r.events(); len(evs) != 2 || evs[1].Active {
			t.Fatalf("events = %+v", evs)
		}
	})

	t.Run("each of the three counters is a foreign router", func(t *testing.T) {
		for counter, c := range map[string]authCounters{
			ingressbundle.VIPCounterInvalidHMAC: {InvalidHMAC: 2},
			ingressbundle.VIPCounterMissing:     {Missing: 2},
			ingressbundle.VIPCounterUnknownKey:  {UnknownKey: 2},
		} {
			r := newStatsRig(t)
			r.poll(statsText(c))
			evs := r.events()
			if len(evs) != 1 || evs[0].Counter != counter || !evs[0].Active {
				t.Errorf("%s: events = %+v", counter, evs)
			}
		}
	})

	t.Run("malformed and replay are not a foreign router", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll(statsText(authCounters{Malformed: 3, Replay: 3}))
		if l := r.out.lines(); len(l) != 0 {
			t.Fatalf("wrote %q", l)
		}
	})

	t.Run("a counter that went down is a new baseline", func(t *testing.T) {
		r := newStatsRig(t)
		r.w.last = authCounters{InvalidHMAC: 9}
		r.poll(statsText(authCounters{InvalidHMAC: 2}))
		if l := r.out.lines(); len(l) != 0 {
			t.Fatalf("wrote %q", l)
		}
		r.poll(statsText(authCounters{InvalidHMAC: 3}))
		if len(r.events()) != 1 {
			t.Fatalf("a rise from the new baseline was not written: %q", r.out.lines())
		}
	})

	t.Run("the sender is omitted unless a failure line named a valid address", func(t *testing.T) {
		for name, line := range map[string]string{
			"no line":          "",
			"text, not an ip":  authLine("invalid authentication HMAC", "<script>alert(1)</script>"),
			"another counter":  authLine("missing authentication trailer", "10.0.0.9"),
			"another instance": "Tue Nov 24 12:00:01 2026: (OTHER) invalid authentication HMAC from 10.0.0.8",
		} {
			r := newStatsRig(t)
			if line != "" {
				r.facts.scan([]byte(line))
			}
			r.poll(hmac)
			evs := r.events()
			if len(evs) != 1 || evs[0].Addr != "" {
				t.Errorf("%s: events = %+v", name, evs)
			}
		}
		// The unknown-key line carries the key id before the sender.
		r := newStatsRig(t)
		r.facts.scan([]byte("(" + InstanceName + ") unknown authentication key id 3 from fe80::1%eth0"))
		r.poll(statsText(authCounters{UnknownKey: 1}))
		if evs := r.events(); len(evs) != 1 || evs[0].Addr != "fe80::1" {
			t.Errorf("unknown key: events = %+v", evs)
		}
	})

	t.Run("a dropped line is tried again at the next poll", func(t *testing.T) {
		r := newStatsRig(t)
		// Use up the rate limit: the next foreign_router line is dropped.
		for range MaxLinesPerSecond {
			r.w.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventClockSkew, Active: true})
		}
		before := len(r.out.lines())
		r.w.observe(authCounters{InvalidHMAC: 1})
		if len(r.out.lines()) != before || r.w.foreign {
			t.Fatalf("the dropped line counted as written")
		}
		r.w.clock.After(time.Second) // a second later the bucket has refilled
		r.w.observe(authCounters{InvalidHMAC: 2})
		if len(r.out.lines()) != before+1 || !r.w.foreign {
			t.Fatalf("the line was not written at the next poll: %q", r.out.lines())
		}
	})

	t.Run("a signal that fails or a file that never comes writes nothing and counts nothing", func(t *testing.T) {
		r := newStatsRig(t)
		r.sigErr = errors.New("no such process")
		r.poll(hmac)
		if len(r.out.lines()) != 0 || r.opens != 0 {
			t.Fatalf("a failed signal still read the file: lines %q opens %d", r.out.lines(), r.opens)
		}
		r.sigErr = nil
		r.queue = nil // the dump the failed signal never produced
		r.poll("")    // keepalived did not answer
		if len(r.out.lines()) != 0 {
			t.Fatalf("wrote %q", r.out.lines())
		}
		if r.opens != statsReadTries {
			t.Fatalf("%d reads, want the bounded %d", r.opens, statsReadTries)
		}
		// The baseline is unchanged by the skipped polls: the next real dump is compared with zero.
		r.poll(hmac)
		if len(r.events()) != 1 {
			t.Fatalf("lines = %q", r.out.lines())
		}
	})

	t.Run("a stale file left from before is not read", func(t *testing.T) {
		r := newStatsRig(t)
		stale := hmac
		r.file = &stale
		r.poll("") // the signal writes nothing; the poll removed the old file first
		if len(r.out.lines()) != 0 {
			t.Fatalf("the old file was read: %q", r.out.lines())
		}
	})
}

// TestVIPEntrypointPollsStatistics runs the real wiring: Run signals the child (the fake takes SIGUSR2) every
// StatsInterval, reads the file through its deps and writes the foreign_router line after the state lines.
func TestVIPEntrypointPollsStatistics(t *testing.T) {
	r := newRig(t, "lines")
	dump := testdata(t, "stats-invalid-hmac.txt")
	r.deps.RemoveFile = func(path string) error {
		// The statistics file before each poll; the address flag is the check's, at the start.
		if path != StatsPath && path != r.deps.FlagPath {
			t.Errorf("removed %q, want %q", path, StatsPath)
		}
		return nil
	}
	r.deps.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(dump)), nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := r.run(ctx)

	r.out.waitLines(t, 4)
	r.clock.waitTimer(t, StatsInterval)
	r.clock.Advance(StatsInterval)
	events := parseAll(t, r.out.waitLines(t, 5))
	cancel()
	if code := waitCode(t, done); code != 0 {
		t.Fatalf("Run = %d, want 0 (stderr %q)", code, r.errw.String())
	}
	if ev := events[4]; ev.Event != ingressbundle.VIPEventForeignRouter || !ev.Active || ev.Counter != ingressbundle.VIPCounterInvalidHMAC {
		t.Fatalf("line 5 = %+v", ev)
	}
	// The final STOP is the last line: the watcher was finished before it was written.
	lines := parseAll(t, r.out.lines())
	if last := lines[len(lines)-1]; last.Event != ingressbundle.VIPEventState || last.State != ingressbundle.VIPStateStop {
		t.Fatalf("last line = %+v, want STOP", last)
	}
}

func TestClockSkewFromStats(t *testing.T) {
	t.Run("a rising Stale counter sets clock_skew and three quiet polls clear it", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll(statsText(authCounters{}))
		if l := r.out.lines(); len(l) != 0 {
			t.Fatalf("a clean poll wrote %q", l)
		}
		r.poll(statsText(authCounters{Stale: 2}))
		evs := r.events()
		if len(evs) != 1 || evs[0].Event != ingressbundle.VIPEventClockSkew || !evs[0].Active || evs[0].Counter != "" || evs[0].Addr != "" {
			t.Fatalf("events = %+v, want one active clock_skew", evs)
		}
		r.poll(statsText(authCounters{Stale: 5})) // still rising: not written again
		r.poll(statsText(authCounters{Stale: 5})) // quiet 1
		r.poll(statsText(authCounters{Stale: 5})) // quiet 2
		if n := len(r.out.lines()); n != 1 {
			t.Fatalf("%d lines before the third quiet poll, want 1", n)
		}
		r.poll(statsText(authCounters{Stale: 5})) // quiet 3
		evs = r.events()
		if len(evs) != 2 || evs[1].Event != ingressbundle.VIPEventClockSkew || evs[1].Active {
			t.Fatalf("events = %+v, want the clearing line", evs)
		}
	})

	t.Run("clock skew and a foreign router are separate conditions", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll(statsText(authCounters{Stale: 1, InvalidHMAC: 1}))
		evs := r.events()
		if len(evs) != 2 || evs[0].Event != ingressbundle.VIPEventForeignRouter || evs[1].Event != ingressbundle.VIPEventClockSkew {
			t.Fatalf("events = %+v", evs)
		}
		// Only the foreign router keeps rising: the clock clears on its own count.
		for i := range 3 {
			r.poll(statsText(authCounters{Stale: 1, InvalidHMAC: uint64(2 + i)}))
		}
		evs = r.events()
		if len(evs) != 3 || evs[2].Event != ingressbundle.VIPEventClockSkew || evs[2].Active {
			t.Fatalf("events = %+v, want the clock cleared and the foreign router still active", evs)
		}
	})

	t.Run("a missing or unreadable file is skipped without a line", func(t *testing.T) {
		r := newStatsRig(t)
		r.poll("")
		r.poll("VRRP Instance: " + InstanceName + "\n  Authentication Extension Errors:\n    Missing: 0\n") // cut off
		r.poll("not a statistics file")
		if l := r.out.lines(); len(l) != 0 {
			t.Fatalf("wrote %q", l)
		}
		if r.signals != 3 {
			t.Fatalf("%d signals for three polls", r.signals)
		}
	})
}
