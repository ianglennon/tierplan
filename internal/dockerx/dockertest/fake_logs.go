// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockertest

import (
	"encoding/binary"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var _ = registerFakeHandler((*Fake).serveLogs)

var logsRoute = regexp.MustCompile(`^/containers/([^/]+)/logs$`)

// logTimeLayout is how the engine writes the timestamp in front of a log line: UTC
// with nine digits, so the text sorts as time does.
const logTimeLayout = "2006-01-02T15:04:05.000000000Z"

// FakeLogLine is one line a container wrote.
type FakeLogLine struct {
	// Stream is "stdout" (the default) or "stderr".
	Stream string
	// Time is when the line was written. Zero means a millisecond after the line
	// before it, from 2026-01-01T00:00:00Z.
	Time time.Time
	// Text is the line without its line ending.
	Text string
}

// fakeLogBuf is the log of one container.
type fakeLogBuf struct {
	lines []FakeLogLine
	ended bool
	// wake is closed, and replaced, whenever a line is added or the log ends, so
	// every open follow looks again.
	wake chan struct{}
}

// fakeLogs is the Fake's log state. The zero value is ready to use.
type fakeLogs struct {
	mu      sync.Mutex
	byID    map[string]*fakeLogBuf
	follows int
	queries []url.Values
}

func (l *fakeLogs) buf(id string) *fakeLogBuf {
	if l.byID == nil {
		l.byID = map[string]*fakeLogBuf{}
	}
	b := l.byID[id]
	if b == nil {
		b = &fakeLogBuf{wake: make(chan struct{})}
		l.byID[id] = b
	}
	return b
}

func (b *fakeLogBuf) signal() {
	close(b.wake)
	b.wake = make(chan struct{})
}

// AppendLogs adds lines to the log of a container, named by ID, name or an unambiguous
// ID prefix, and wakes every follow of it. A line without a time follows the one
// before it by a millisecond.
func (f *Fake) AppendLogs(container string, lines ...FakeLogLine) {
	id := container
	if c, ok := f.config().resolveContainer(container); ok {
		id = c.ID
	}
	l := &f.logs
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf(id)
	for _, line := range lines {
		if line.Time.IsZero() {
			line.Time = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if n := len(b.lines); n > 0 {
				line.Time = b.lines[n-1].Time.Add(time.Millisecond)
			}
		}
		b.lines = append(b.lines, line)
	}
	b.signal()
}

// EndLogs ends the log of a container the way its exit does: every follow of it
// returns, as the engine's stream does when the container stops.
func (f *Fake) EndLogs(container string) {
	id := container
	if c, ok := f.config().resolveContainer(container); ok {
		id = c.ID
	}
	l := &f.logs
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf(id)
	b.ended = true
	b.signal()
}

// StartLogs begins a new run of the log of a container after EndLogs, as its start
// does: a follow opened from now on stays open for the lines appended after.
func (f *Fake) StartLogs(container string) {
	id := container
	if c, ok := f.config().resolveContainer(container); ok {
		id = c.ID
	}
	l := &f.logs
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf(id)
	b.ended = false
	b.signal()
}

// LogFollows is how many follow requests for a log are open now.
func (f *Fake) LogFollows() int {
	f.logs.mu.Lock()
	defer f.logs.mu.Unlock()
	return f.logs.follows
}

// LogQueries are the query strings of every log request served, oldest first.
func (f *Fake) LogQueries() []url.Values {
	f.logs.mu.Lock()
	defer f.logs.mu.Unlock()
	return append([]url.Values(nil), f.logs.queries...)
}

// serveLogs answers GET /containers/{id}/logs: the lines of the container written so
// far, filtered by stdout, stderr, tail, since and until (in the order the engine applies them), each with the engine's
// timestamp when timestamps=1; and with follow=1 the lines appended after, until
// EndLogs, the client leaving or the daemon stopping. A container with a TTY
// (FakeInspect.Tty) is answered raw, with CR LF line ends; any other is multiplexed
// as stdcopy frames, one frame per line. An unknown container is the 404 an engine
// gives, and a container whose log driver is none (FakeInspect.LogType) the 501.
func (f *Fake) serveLogs(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	m := logsRoute.FindStringSubmatch(p)
	if m == nil || r.Method != http.MethodGet {
		return false
	}
	c, known := cfg.resolveContainer(m[1])
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: " + m[1]})
		return true
	}
	// Like the engine, a container whose log driver keeps nothing to read (none) answers
	// 501 to a read of its log.
	if c.Inspect.LogType == "none" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"message": "configured logging driver does not support reading"})
		return true
	}
	q := r.URL.Query()
	tty := c.Inspect.Tty
	l := &f.logs
	l.mu.Lock()
	l.queries = append(l.queries, q)
	l.mu.Unlock()

	want := func(stream string) bool {
		if stream == "stderr" {
			return q.Get("stderr") == "1"
		}
		return q.Get("stdout") == "1"
	}
	since, hasSince := parseLogTime(q.Get("since"))
	until, hasUntil := parseLogTime(q.Get("until"))
	timestamps := q.Get("timestamps") == "1"
	follow := q.Get("follow") == "1"

	if tty {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	} else {
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	}
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	flush()

	write := func(line FakeLogLine) bool {
		text := line.Text
		if timestamps {
			text = line.Time.UTC().Format(logTimeLayout) + " " + text
		}
		if tty {
			text += "\r\n"
			_, err := w.Write([]byte(text))
			return err == nil
		}
		text += "\n"
		kind := byte(1)
		if line.Stream == "stderr" {
			kind = 2
		}
		frame := make([]byte, 8, 8+len(text))
		frame[0] = kind
		binary.BigEndian.PutUint32(frame[4:], uint32(len(text))) //nolint:gosec // a test line is far below 4 GiB
		_, err := w.Write(append(frame, text...))
		return err == nil
	}

	l.mu.Lock()
	b := l.buf(c.ID)
	// Like the engine, the tail is taken from the end of the log first and since and
	// until are applied to those lines, so a tail with an until can answer fewer
	// lines than asked. Until is inclusive.
	var first []FakeLogLine
	for _, line := range b.lines {
		if want(line.Stream) {
			first = append(first, line)
		}
	}
	sent := len(b.lines)
	if t := q.Get("tail"); t != "" && t != "all" {
		if n, err := strconv.Atoi(t); err == nil && n >= 0 && n < len(first) {
			first = first[len(first)-n:]
		}
	}
	kept := first[:0:0]
	for _, line := range first {
		if (hasSince && line.Time.Before(since)) || (hasUntil && line.Time.After(until)) {
			continue
		}
		kept = append(kept, line)
	}
	first = kept
	if follow {
		l.follows++
	}
	l.mu.Unlock()
	if follow {
		f.await.notify()
		defer func() {
			l.mu.Lock()
			l.follows--
			l.mu.Unlock()
			f.await.notify()
		}()
	}

	for _, line := range first {
		if !write(line) {
			return true
		}
	}
	flush()
	for follow {
		l.mu.Lock()
		b := l.buf(c.ID)
		next := append([]FakeLogLine(nil), b.lines[sent:]...)
		sent = len(b.lines)
		ended, wake := b.ended, b.wake
		l.mu.Unlock()
		for _, line := range next {
			if !want(line.Stream) {
				continue
			}
			if !write(line) {
				return true
			}
		}
		flush()
		if ended {
			return true
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return true
		}
	}
	return true
}

// parseLogTime reads the since and until the Docker client sends: Unix seconds with
// an optional fraction.
func parseLogTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	sec, frac, _ := strings.Cut(s, ".")
	secs, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	var nanos int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		nanos, _ = strconv.ParseInt(frac, 10, 64)
	}
	return time.Unix(secs, nanos).UTC(), true
}
