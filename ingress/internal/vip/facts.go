// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// DiscoveryRescan is how often the helper looks for the interface again while it has none (D-09).
const DiscoveryRescan = 10 * time.Second

// FaultReasonGrace is how long a FAULT state waits for the check's failure line before it is reported without a
// reason. keepalived logs the failing script and then changes state, but the log and the FIFO are two streams
// the helper reads separately, so the state can be read first.
const FaultReasonGrace = 250 * time.Millisecond

// scriptName is the vrrp_script Render defines.
const scriptName = "chk_proxy"

// maxScanLine is the longest keepalived line the scanner looks at. A longer line is discarded whole.
const maxScanLine = 2048

// The texts keepalived logs about a vrrp_script (RESEARCH Pattern 1). They are matched, never copied on.
var (
	scriptFailed   = "VRRP_Script(" + scriptName + ") failed"
	scriptTimedOut = "VRRP_Script(" + scriptName + ") timed out"
	scriptOK       = "VRRP_Script(" + scriptName + ") succeeded"
)

// probeFacts is the one fact the helper keeps from keepalived's own output: whether the proxy check is
// failing now (D-12). Everything else keepalived prints is read and dropped (T-26-29).
type probeFacts struct {
	mu   sync.Mutex
	fail bool
	ch   chan struct{}
}

// writer returns the sink keepalived's standard output and error are given. Its Write never blocks and never
// fails, so it can never stall the child.
func (f *probeFacts) writer() io.Writer { return &lineSink{f: f} }

// failing reports whether the check has failed since it last succeeded or was reset.
func (f *probeFacts) failing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail
}

// reset forgets a failure: the node reached BACKUP or MASTER, so the check passed.
func (f *probeFacts) reset() {
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
}

// wait returns a channel that is closed the next time the check is seen to fail.
func (f *probeFacts) wait() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ch == nil {
		f.ch = make(chan struct{})
	}
	return f.ch
}

func (f *probeFacts) scan(line []byte) {
	s := string(line)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(s, scriptFailed), strings.Contains(s, scriptTimedOut):
		f.fail = true
		if f.ch != nil {
			close(f.ch)
			f.ch = nil
		}
	case strings.Contains(s, scriptOK):
		f.fail = false
	}
}

// lineSink splits what it is written into lines and hands each to the scanner.
type lineSink struct {
	f        *probeFacts
	mu       sync.Mutex
	buf      []byte
	skipping bool
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p)
			break
		}
		s.add(p[:i])
		if !s.skipping {
			s.f.scan(s.buf)
		}
		s.buf, s.skipping = s.buf[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (s *lineSink) add(chunk []byte) {
	if s.skipping {
		return
	}
	if len(s.buf)+len(chunk) > maxScanLine {
		s.buf, s.skipping = s.buf[:0], true
		return
	}
	s.buf = append(s.buf, chunk...)
}
