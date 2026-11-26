// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// The statistics poll (A-04, D-06). keepalived logs each authentication failure once per state change, so the
// log cannot say a foreign router went away; its statistics counters can. The helper sends SIGUSR2, reads the
// file keepalived writes and compares the counters with the last poll.
const (
	// StatsPath is where keepalived writes its statistics on SIGUSR2. It is a tmpfs in the service (A-01).
	StatsPath = "/tmp/keepalived.stats"
	// StatsInterval is the time between two polls.
	StatsInterval = 12 * time.Second
	// quietPolls is how many polls without a rise clear a foreign router or a clock fault.
	quietPolls = 3
	// statsReadEvery and statsReadTries bound the wait for keepalived to write the file after the signal: the
	// parent forwards it to the VRRP child, which writes when its loop gets to it.
	statsReadEvery = 100 * time.Millisecond
	statsReadTries = 20
)

// authCounters are the six authentication extension counters of one instance.
type authCounters struct {
	Missing, Malformed, UnknownKey, InvalidHMAC, Stale, Replay uint64
}

// errStats is what parseStats returns for a file that has not the block yet or holds a counter it cannot read.
var errStats = errors.New("no complete authentication counters")

// maxStatsLine bounds one line of the statistics file; keepalived's own lines are far shorter.
const maxStatsLine = 512

// parseStats reads the counters of the instance from the text of keepalived's statistics file. A file holds one
// `VRRP Instance:` section per instance, each with a block of the six authentication extension counters. When the instance appears more than once the last section counts, so an appended dump reads as the
// newest. A section with a missing or unreadable counter is an error: a half-written file must not read as zero.
func parseStats(r io.Reader, instance string) (authCounters, error) {
	var (
		got      authCounters
		found    bool
		inst     bool
		inBlock  bool
		blockInd int
		seen     map[string]bool
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, maxStatsLine), maxStatsLine)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		ind := len(line) - len(strings.TrimLeft(line, " \t"))
		if name, ok := strings.CutPrefix(trimmed, "VRRP Instance:"); ok && ind == 0 {
			inst = strings.TrimSpace(name) == instance
			inBlock = false
			if inst {
				got, seen, found = authCounters{}, map[string]bool{}, false
			}
			continue
		}
		if !inst {
			continue
		}
		if trimmed == "Authentication Extension Errors:" {
			inBlock, blockInd = true, ind
			continue
		}
		if !inBlock {
			continue
		}
		if ind <= blockInd {
			inBlock = false
			continue
		}
		name, val, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return authCounters{}, errStats
		}
		var dst *uint64
		switch name {
		case "Missing":
			dst = &got.Missing
		case "Malformed":
			dst = &got.Malformed
		case "Unknown Key":
			dst = &got.UnknownKey
		case "Invalid HMAC":
			dst = &got.InvalidHMAC
		case "Stale":
			dst = &got.Stale
		case "Replay":
			dst = &got.Replay
		default:
			continue
		}
		*dst = n
		seen[name] = true
		if len(seen) == 6 {
			found = true
		}
	}
	if sc.Err() != nil || !found {
		return authCounters{}, errStats
	}
	return got, nil
}

// statsWatcher turns the counters into foreign_router and clock_skew events. Its flags change only when a line
// was written, so a line the rate limit dropped is tried again at the next poll.
type statsWatcher struct {
	clock Clock
	em    *Emitter
	facts *probeFacts
	// signal asks keepalived for its statistics (SIGUSR2).
	signal func() error
	// remove deletes the stale statistics file before the signal, so a file that exists afterwards is a fresh one.
	remove func(path string) error
	// open opens the statistics file.
	open func(path string) (io.ReadCloser, error)

	// last are the counters of the previous poll. keepalived starts them at zero, so the first poll compares
	// with zero and a foreign router already there when the helper starts is seen.
	last authCounters

	foreign, clockSkew           bool
	foreignQuiet, clockSkewQuiet int
}

// run polls every StatsInterval until ctx ends.
func (w *statsWatcher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(StatsInterval):
		}
		w.poll(ctx)
	}
}

// poll asks for one dump, reads it and reports what changed. A signal that cannot be sent or a file that does
// not appear or does not read is skipped without a line: the next poll is the retry.
func (w *statsWatcher) poll(ctx context.Context) {
	_ = w.remove(StatsPath)
	if err := w.signal(); err != nil {
		return
	}
	var c authCounters
	var err error
	for try := 0; ; try++ {
		if c, err = w.read(); err == nil {
			break
		}
		if try+1 >= statsReadTries {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-w.clock.After(statsReadEvery):
		}
	}
	w.observe(c)
}

func (w *statsWatcher) read() (authCounters, error) {
	f, err := w.open(StatsPath)
	if err != nil {
		return authCounters{}, err
	}
	defer f.Close()
	return parseStats(f, InstanceName)
}

// observe compares c with the last poll. A counter that went down (keepalived restarted its own count) is a new
// baseline, never a rise.
func (w *statsWatcher) observe(c authCounters) {
	prev := w.last
	w.last = c

	counter := ""
	switch {
	case c.InvalidHMAC > prev.InvalidHMAC:
		counter = ingressbundle.VIPCounterInvalidHMAC
	case c.Missing > prev.Missing:
		counter = ingressbundle.VIPCounterMissing
	case c.UnknownKey > prev.UnknownKey:
		counter = ingressbundle.VIPCounterUnknownKey
	}
	switch {
	case counter != "":
		w.foreignQuiet = 0
		if !w.foreign {
			w.foreign = w.em.Emit(ingressbundle.VIPEvent{
				Event: ingressbundle.VIPEventForeignRouter, Active: true, Counter: counter, Addr: w.facts.sender(counter),
			})
		}
	case w.foreign:
		if w.foreignQuiet++; w.foreignQuiet >= quietPolls {
			if w.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventForeignRouter}) {
				w.foreign, w.foreignQuiet = false, 0
			}
		}
	}

	switch {
	case c.Stale > prev.Stale:
		w.clockSkewQuiet = 0
		if !w.clockSkew {
			w.clockSkew = w.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventClockSkew, Active: true})
		}
	case w.clockSkew:
		if w.clockSkewQuiet++; w.clockSkewQuiet >= quietPolls {
			if w.em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventClockSkew}) {
				w.clockSkew, w.clockSkewQuiet = false, 0
			}
		}
	}
}
