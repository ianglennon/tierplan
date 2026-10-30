// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
)

// Framing limits of a log stream.
const (
	// maxLogLine is the longest line sent whole. The engine's logger cuts a long
	// line into pieces of 16 KB that arrive as separate frames; they are joined
	// here, up to this size, and a line past it is sent in pieces of this size, so
	// one line that never ends cannot grow the buffer without end.
	maxLogLine = 256 << 10
	// batchLines and batchBytes are how much a batch holds before it is sent
	// without waiting for the interval; batchInterval is the longest a line waits.
	batchLines    = 100
	batchBytes    = 32 << 10
	batchInterval = 100 * time.Millisecond
	// logCopyBuffer is the size of the buffer a raw (TTY) stream is read with.
	logCopyBuffer = 32 << 10

	streamOut = "o"
	streamErr = "e"
)

// LogLine is one line of a container's log as the stream carries it: the time the
// engine stamped it with, the stream it came on ("o" for stdout, "e" for stderr)
// and the text, without its line ending and without the timestamp.
type LogLine struct {
	T string `json:"t"`
	S string `json:"s"`
	M string `json:"m"`
}

// pumpLogs reads a container's log stream to its end and calls emit for every line,
// in the order the lines were read. A stream that is not from a TTY is multiplexed
// by the engine into frames of stdout and stderr, which are split back into lines per
// stream, so a line that crosses two frames comes out whole and a frame with several
// lines comes out as several. A TTY stream is raw and every line of it is stdout.
// The last line is sent even if it has no line ending, once the stream ends.
//
// It reads with fixed buffers and keeps at most one unfinished line per stream, so it
// never holds more than a log's current line. Cancelling ctx does not interrupt a
// read: the caller closes r to do that. It returns nil at a clean end of the stream.
func pumpLogs(ctx context.Context, r io.Reader, tty bool, emit func(LogLine)) error {
	out := &lineSplitter{stream: streamOut, emit: emit, trimCR: tty}
	errs := &lineSplitter{stream: streamErr, emit: emit, trimCR: tty}
	var err error
	if tty {
		buf := make([]byte, logCopyBuffer)
		_, err = io.CopyBuffer(out, readerOnly{r}, buf)
	} else {
		_, err = stdcopy.StdCopy(out, errs, r)
	}
	out.flush()
	errs.flush()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// readerOnly hides any WriteTo of the source, so io.CopyBuffer uses its buffer.
type readerOnly struct{ io.Reader }

// lineSplitter is an io.Writer that turns the bytes written to it into LogLines.
type lineSplitter struct {
	stream string
	emit   func(LogLine)
	buf    []byte
	// last is the time of the line before, for a piece of a long line that comes
	// without a stamp of its own.
	last string
	// cont is true when buf holds the rest of a line that was cut at maxLogLine, which
	// has no timestamp in front of it.
	cont bool
	// trimCR drops the carriage return before a line feed: a TTY ends its lines with
	// CR LF. Output that is not from a TTY is shown exactly as the program wrote it.
	trimCR bool
}

// Write takes bytes of the stream, sends every complete line and keeps the rest.
func (s *lineSplitter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			room := maxLogLine - len(s.buf)
			if len(p) <= room {
				s.buf = append(s.buf, p...)
				return n, nil
			}
			s.buf = append(s.buf, p[:room]...)
			p = p[room:]
			s.send(true)
			continue
		}
		if room := maxLogLine - len(s.buf); i > room {
			s.buf = append(s.buf, p[:room]...)
			p = p[room:]
			s.send(true)
			continue
		}
		s.buf = append(s.buf, p[:i]...)
		p = p[i+1:]
		s.send(false)
	}
	return n, nil
}

// flush sends the unfinished line, if there is one.
func (s *lineSplitter) flush() {
	if len(s.buf) > 0 {
		s.send(false)
	}
}

// send emits the buffered line. cut says the line goes on in the next write.
func (s *lineSplitter) send(cut bool) {
	line := s.buf
	if s.cont && !cut && len(line) == 0 {
		// The line feed that ends a line the cut already sent whole.
		s.cont = false
		return
	}
	if s.trimCR && !cut {
		line = bytes.TrimSuffix(line, []byte{'\r'})
	}
	t, msg := s.last, string(line)
	if !s.cont {
		if ts, rest, ok := splitStamp(line); ok {
			t, msg = ts, string(rest)
			s.last = ts
		}
	}
	s.cont = cut
	s.buf = s.buf[:0]
	s.emit(LogLine{T: t, S: s.stream, M: msg})
}

// splitStamp separates the timestamp the engine puts in front of a line ("2026-10-09T
// 12:58:00.123456789Z text") from the text. A line that does not start with one is
// not split.
func splitStamp(line []byte) (ts string, rest []byte, ok bool) {
	i := bytes.IndexByte(line, ' ')
	if i < len("2006-01-02T15:04:05Z") || i > 40 {
		return "", line, false
	}
	if _, err := time.Parse(time.RFC3339Nano, string(line[:i])); err != nil {
		return "", line, false
	}
	return string(line[:i]), line[i+1:], true
}

// batcher collects lines for one event: it is full at batchLines lines or batchBytes
// bytes of text, and the caller sends what it holds at least every batchInterval.
type batcher struct {
	lines []LogLine
	size  int
}

// add appends a line and reports whether the batch is now full.
func (b *batcher) add(l LogLine) bool {
	b.lines = append(b.lines, l)
	b.size += len(l.M) + len(l.T) + 8
	return len(b.lines) >= batchLines || b.size >= batchBytes
}

// take returns the lines and empties the batch.
func (b *batcher) take() []LogLine {
	out := b.lines
	b.lines, b.size = nil, 0
	return out
}

func (b *batcher) empty() bool { return len(b.lines) == 0 }
