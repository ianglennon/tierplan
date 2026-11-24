// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func TestVIPEventsRateLimit(t *testing.T) {
	cfg, err := Load(testEnv)
	if err != nil {
		t.Fatal(err)
	}
	clock := newFakeClock()
	out := newSyncBuf()
	em := NewEmitter(out, cfg, clock)
	fault := ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault, Reason: ingressbundle.VIPReasonNoInterface, Active: true}
	state := ingressbundle.VIPEvent{Event: ingressbundle.VIPEventState, State: ingressbundle.VIPStateMaster}

	// 100 faults in one fake-clock second: the bucket's 20.
	for range 100 {
		em.Emit(fault)
	}
	if n := len(out.lines()); n != MaxLinesPerSecond {
		t.Fatalf("%d fault lines written in one second, want %d", n, MaxLinesPerSecond)
	}
	if em.Dropped() != 80 {
		t.Fatalf("dropped = %d, want 80", em.Dropped())
	}

	// A state line is never dropped, even with the bucket empty.
	for range 100 {
		if !em.Emit(state) {
			t.Fatal("a state line was dropped")
		}
	}
	if n := len(out.lines()); n != MaxLinesPerSecond+100 {
		t.Fatalf("%d lines, want %d", n, MaxLinesPerSecond+100)
	}

	// Half a second later half the bucket is back.
	clock.Advance(500 * time.Millisecond)
	written := 0
	for range 100 {
		if em.Emit(fault) {
			written++
		}
	}
	if written != MaxLinesPerSecond/2 {
		t.Fatalf("%d fault lines after half a second, want %d", written, MaxLinesPerSecond/2)
	}

	// Every call took a sequence number, so what was dropped is a gap the server can see; the numbers only rise.
	events := parseAll(t, out.lines())
	var prev uint64
	gaps := 0
	for _, ev := range events {
		if ev.Seq <= prev {
			t.Fatalf("seq %d after %d: the sequence did not rise", ev.Seq, prev)
		}
		if ev.Seq != prev+1 {
			gaps++
		}
		prev = ev.Seq
	}
	if gaps == 0 {
		t.Fatal("no gap in the sequence although lines were dropped")
	}
}

func TestEmitterRefusesWhatTheFormatRefuses(t *testing.T) {
	cfg, err := Load(testEnv)
	if err != nil {
		t.Fatal(err)
	}
	out := newSyncBuf()
	em := NewEmitter(out, cfg, newFakeClock())
	// A fault without a reason is not a line the server would accept.
	if em.Emit(ingressbundle.VIPEvent{Event: ingressbundle.VIPEventFault}) {
		t.Fatal("an invalid event was written")
	}
	if em.Refused() != 1 || len(out.lines()) != 0 {
		t.Fatalf("refused = %d, lines = %q", em.Refused(), out.String())
	}
}
