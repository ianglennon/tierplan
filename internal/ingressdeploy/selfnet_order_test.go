// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The publication order of the self network observation: a read that began before the one last published is dropped,
// so a stale Exists:true cannot put a freed subnet back on the trusted list.

// orderedService is a Service with a counter on the OnSelfObserved callback.
func orderedService(t *testing.T) (*Service, *atomic.Int32) {
	t.Helper()
	s := New(Deps{})
	var calls atomic.Int32
	s.OnSelfObserved(func(SelfObservation) { calls.Add(1) })
	return s, &calls
}

func existsObs(envID string) SelfObservation {
	return SelfObservation{EnvID: envID, Observed: true, Exists: true, Subnet: netip.MustParsePrefix("10.99.0.0/24")}
}

func TestSelfObservationFromAnOlderReadIsDropped(t *testing.T) {
	t.Parallel()
	s, calls := orderedService(t)
	older := s.selfTicket("env-1")
	newer := s.selfTicket("env-1")

	if !s.publishSelf(SelfObservation{EnvID: "env-1", Observed: true}, newer) {
		t.Fatalf("publishSelf of the newer read = false, want true")
	}
	if s.publishSelf(existsObs("env-1"), older) {
		t.Errorf("publishSelf of the read that began first = true, want false")
	}
	got, ok := s.SelfObservationOf("env-1")
	if !ok || got.Exists {
		t.Errorf("SelfObservationOf = %+v, %v, want the Exists:false observation of the newer read", got, ok)
	}
	if n := s.selfSeqOf("env-1"); n != 1 {
		t.Errorf("selfSeqOf = %d, want 1: a dropped observation is not counted", n)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("OnSelfObserved ran %d times, want 1", n)
	}
}

func TestSelfObservationsInOrderAllPublish(t *testing.T) {
	t.Parallel()
	s, calls := orderedService(t)

	first := s.selfTicket("env-1")
	if !s.publishSelf(SelfObservation{EnvID: "env-1", Observed: true}, first) {
		t.Fatalf("the first publish = false, want true")
	}
	second := s.selfTicket("env-1")
	if !s.publishSelf(existsObs("env-1"), second) {
		t.Fatalf("the second publish = false, want true")
	}
	if n := s.selfSeqOf("env-1"); n != 2 {
		t.Errorf("selfSeqOf = %d, want 2", n)
	}
	if got, _ := s.SelfObservationOf("env-1"); !got.Exists {
		t.Errorf("SelfObservationOf = %+v, want the second (Exists:true) observation", got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("OnSelfObserved ran %d times, want 2", n)
	}
}

func TestSelfObservationTicketsArePerEnvironment(t *testing.T) {
	t.Parallel()
	s, _ := orderedService(t)
	one := s.selfTicket("env-1")
	two := s.selfTicket("env-2")

	if !s.publishSelf(existsObs("env-2"), two) {
		t.Fatalf("publishSelf for env-2 = false, want true")
	}
	if !s.publishSelf(existsObs("env-1"), one) {
		t.Errorf("a later ticket of env-2 dropped the earlier ticket of env-1")
	}
}

// removedPass is a pass that removed the self network and knows where TierPlan runs.
func removedPass(envID string) *selfPass {
	return &selfPass{
		info:  EnvInfo{ID: envID},
		place: SelfPlace{Here: SelfHereYes, Kind: SelfKindStandalone},
		net:   selfNet{removed: true},
	}
}

// WR-01: a pass that removed the network and then fails its read still publishes the removal.
func TestSelfRemovalIsPublishedWhenTheReadFails(t *testing.T) {
	t.Parallel()
	s, _ := orderedService(t)
	read := func(func(*dockerx.Client) error) error { return errors.New("the engine did not answer") }

	gone, ok := s.observeSelfOn(testsupport.Context(t), removedPass("env-1"), read)
	if !ok {
		t.Fatalf("observeSelfOn = _, false, want the removal published")
	}
	if !gone.Observed || gone.Exists {
		t.Errorf("returned observation = %+v, want Observed and not Exists", gone)
	}
	got, have := s.SelfObservationOf("env-1")
	if !have || !got.Observed || got.Exists {
		t.Errorf("SelfObservationOf = %+v, %v, want Observed and not Exists", got, have)
	}
}

// WR-01 under ordering: a read that began after this pass's removal already reflects it, so when one of them has
// published, the removal of the failed read is dropped and does not undo it.
func TestSelfRemovalYieldsToAReadThatBeganLater(t *testing.T) {
	t.Parallel()
	s, _ := orderedService(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	read := func(func(*dockerx.Client) error) error {
		close(entered)
		<-proceed
		return errors.New("the engine did not answer")
	}
	type result struct {
		o  SelfObservation
		ok bool
	}
	done := make(chan result, 1)
	ctx := testsupport.Context(t)
	go func() {
		o, ok := s.observeSelfOn(ctx, removedPass("env-1"), read)
		done <- result{o, ok}
	}()

	testsupport.Await(t, "the failing read to start", entered)
	later := s.selfTicket("env-1")
	if !s.publishSelf(existsObs("env-1"), later) {
		t.Fatalf("publishSelf of the later read = false, want true")
	}
	close(proceed)

	if r := testsupport.Await(t, "observeSelfOn to return", done); r.ok {
		t.Errorf("observeSelfOn = %+v, true, want false: a read that began later has published", r.o)
	}
	if got, _ := s.SelfObservationOf("env-1"); !got.Exists {
		t.Errorf("SelfObservationOf = %+v, want the Exists:true observation of the later read", got)
	}
	if n := s.selfSeqOf("env-1"); n != 1 {
		t.Errorf("selfSeqOf = %d, want 1", n)
	}
}
