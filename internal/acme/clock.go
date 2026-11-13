// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/stackops"
)

const (
	// ClockWarnAbove is how far the host clock may be from the CA's before the bell says so. It is the
	// same five minutes as tunnel.MaxClockOffset, the skew beyond which an agent is refused: past it a
	// CA starts to see requests and certificates as from the past or the future. Only an offset strictly
	// greater than this is reported.
	ClockWarnAbove = 5 * time.Minute
	// ClockMeasureTTL is how long a sample is believed. A clock can be put right without any request to
	// the CA following, so an old sample is dropped rather than shown as current.
	ClockMeasureTTL = 48 * time.Hour
)

// ClockSample is one measurement of the host clock against the CA's. Like every use of it, it is
// display-only: preflight, Issue, the renewal time, the backoff and the expiry fallback never read it
// (D-07), so a wrong clock changes what the bell says and nothing TierPlan does.
type ClockSample struct {
	// Offset is how far TierPlan's clock is ahead of the CA's, in whole seconds: positive when TierPlan
	// is ahead (as the agent-side clock_skew wording), negative when it is behind.
	Offset time.Duration
	// At is when the response that showed it arrived, on the injected clock; zero for no sample.
	At time.Time
}

// Off reports whether the sample shows the clock more than ClockWarnAbove out and is no older than
// ClockMeasureTTL at now. The zero sample is never off.
func (s ClockSample) Off(now time.Time) bool {
	if s.At.IsZero() || now.Sub(s.At) > ClockMeasureTTL {
		return false
	}
	off := s.Offset
	if off < 0 {
		off = -off
	}
	return off > ClockWarnAbove
}

// skewRecorder remembers the latest ClockSample, taken from the Date header of the responses the ACME
// client already receives (D-06, D-29). It asks the CA for nothing of its own: no NTP, no extra host.
type skewRecorder struct {
	clock stackops.Clock

	mu     sync.Mutex
	latest ClockSample
}

func newSkewRecorder(clock stackops.Clock) *skewRecorder { return &skewRecorder{clock: clock} }

// sample is the latest measurement, the zero sample when none was made.
func (r *skewRecorder) sample() ClockSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest
}

// wrap is a RoundTripper that notes the clock of every response rt returns.
func (r *skewRecorder) wrap(rt http.RoundTripper) http.RoundTripper {
	return skewTransport{rec: r, next: rt}
}

type skewTransport struct {
	rec  *skewRecorder
	next http.RoundTripper
}

func (t skewTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t0 := t.rec.clock.Now()
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil {
		t.rec.note(resp.Header, t0, t.rec.clock.Now())
	}
	return resp, err
}

// note takes a sample from the headers of a response that was asked for at t0 and arrived at t1. A
// copy from a cache (Age above zero) shows the cache's clock, not the CA's, and an unreadable Date shows
// nothing; both leave the previous sample as it was.
//
// The Date header has a resolution of one second and is cut, not rounded, so the CA's moment lies
// within the second that follows it. The offset is taken between the request's midpoint and the middle
// of that second (Date plus 500 ms), rounded half up to whole seconds. That is the midpoint minus Date,
// rounded down: a whole-second midpoint and Date differ by exactly their difference, and one second of
// jitter in a single sample moves the result by at most that second.
func (r *skewRecorder) note(h http.Header, t0, t1 time.Time) {
	if age, err := strconv.Atoi(strings.TrimSpace(h.Get("Age"))); err == nil && age > 0 {
		return
	}
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return
	}
	mid := t0.Add(t1.Sub(t0) / 2)
	off := floorSecond(mid.Sub(date.Add(500*time.Millisecond)) + 500*time.Millisecond)
	r.mu.Lock()
	r.latest = ClockSample{Offset: off, At: t1}
	r.mu.Unlock()
}

// floorSecond rounds d down to a whole number of seconds (toward negative infinity, unlike Truncate).
func floorSecond(d time.Duration) time.Duration {
	q := d.Truncate(time.Second)
	if q > d {
		q -= time.Second
	}
	return q
}
