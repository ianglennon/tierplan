// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package acme

import (
	"context"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
)

// leftOversOf is the left-over list the status shows.
func leftOversOf(t *testing.T, m *Manager) []LeftOverView {
	t.Helper()
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st.DNS.LeftOver
}

func TestDNSSweepInTheSchedulerRound(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	due := e.leftOver("_acme-challenge.due.example.com", "due", testNow.Add(-2*time.Hour))
	later := e.leftOver("_acme-challenge.later.example.com", "later", testNow.Add(-time.Hour))
	if err := e.store.MarkDNSRecordFailed(ctx, later.ID, 1, testNow.Add(-time.Minute), testNow.Add(time.Hour), dns.ClassUnavailable); err != nil {
		t.Fatal(err)
	}

	e.mgr.round(ctx)

	if got := e.deletes(); len(got) != 1 || got[0] != due.RecordID {
		t.Errorf("deletes = %v, want only the due record %s", got, due.RecordID)
	}
	left := leftOversOf(t, e.mgr)
	if len(left) != 1 || left[0].RecordID != later.RecordID {
		t.Fatalf("left-over = %+v, want only the record whose next try is in the future", left)
	}
	if !left[0].NextTryAt.Equal(testNow.Add(time.Hour)) {
		t.Errorf("next try = %v, want it kept at %v", left[0].NextTryAt, testNow.Add(time.Hour))
	}
}

func TestDNSSweepInTheSchedulerRoundBacksOff(t *testing.T) {
	ctx := context.Background()
	e := newDNSEnv(t, ChallengeDNS01)
	e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
	e.prov.FailDelete(-1, &dns.Error{Provider: "fake", Op: "delete record", Class: dns.ClassUnavailable})

	waits := []time.Duration{time.Minute, 10 * time.Minute, 100 * time.Minute, 24 * time.Hour, 24 * time.Hour}
	for i, wait := range waits {
		before := e.prov.CallCount("delete")
		e.mgr.round(ctx)
		if n := e.prov.CallCount("delete") - before; n != 1 {
			t.Fatalf("round %d made %d delete calls, want 1", i+1, n)
		}
		left := leftOversOf(t, e.mgr)
		if len(left) != 1 || left[0].Class != dns.ClassUnavailable || !left[0].NextTryAt.Equal(e.clock.Now().Add(wait)) {
			t.Fatalf("after round %d the status shows %+v, want class dns_unavailable and the next try %v on", i+1, left, wait)
		}
		// A round a second before the next try leaves the row alone.
		e.clock.Advance(wait - time.Second)
		before = e.prov.CallCount("delete")
		e.mgr.round(ctx)
		if n := e.prov.CallCount("delete") - before; n != 0 {
			t.Fatalf("a round a second early made %d delete calls, want 0", n)
		}
		e.clock.Advance(time.Second)
	}
}

func TestDNSSweepInTheSchedulerRoundWithHTTP01SavedOrStopped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *dnsEnv)
	}{
		{"HTTP-01 saved", func(e *dnsEnv) {}},
		{"Let's Encrypt stopped", func(e *dnsEnv) {
			st := e.state()
			st.Mode = ModeOff
			if err := e.store.SaveState(context.Background(), st); err != nil {
				e.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeHTTP01)
			tc.setup(e)
			row := e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)

			e.mgr.round(context.Background())

			if got := e.deletes(); len(got) != 1 || got[0] != row.RecordID {
				t.Errorf("deletes = %v, want the left-over record %s deleted", got, row.RecordID)
			}
			if left := leftOversOf(t, e.mgr); len(left) != 0 {
				t.Errorf("left-over = %+v, want none", left)
			}
		})
	}
}

func TestDNSSweepNeverWhilePausedInTheScheduler(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(e *dnsEnv) *Manager
	}{
		{"paused after a restore", PausedRestore, func(e *dnsEnv) *Manager {
			st := e.state()
			st.PausedReason = PausedRestore
			if err := e.store.SaveState(context.Background(), st); err != nil {
				e.t.Fatal(err)
			}
			return e.mgr
		}},
		{"certificate files set", PausedCertFiles, func(e *dnsEnv) *Manager { return e.build(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDNSEnv(t, ChallengeDNS01)
			e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
			mgr := tc.setup(e)
			before, built := len(e.prov.Calls()), e.built.Load()

			mgr.round(context.Background())

			if n := len(e.prov.Calls()) - before; n != 0 || e.built.Load() != built {
				t.Errorf("a round made %d provider calls and built %d providers while paused, want none", n, e.built.Load()-built)
			}
			left := leftOversOf(t, mgr)
			if len(left) != 1 {
				t.Fatalf("left-over = %+v, want the row kept", left)
			}
			if l := left[0]; !l.RetryPaused || l.PausedReason != tc.reason || !l.NextTryAt.IsZero() {
				t.Errorf("left-over = %+v, want retry paused for %q and no next try promised", l, tc.reason)
			}
		})
	}
}

func TestDNSSweepNeverWhileTheSlotIsBusyInTheScheduler(t *testing.T) {
	t.Run("a Test token holds the DNS slot", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
		if !e.mgr.claimDNS() {
			t.Fatal("the DNS slot was not free")
		}
		before := len(e.prov.Calls())

		e.mgr.round(context.Background())

		if n := len(e.prov.Calls()) - before; n != 0 {
			t.Errorf("a round made %d provider calls while the DNS slot was held, want 0", n)
		}
		e.mgr.releaseDNS()
		e.mgr.round(context.Background())
		if got := e.deletes(); len(got) != 1 {
			t.Errorf("deletes = %v after the slot was released, want the left-over deleted", got)
		}
	})

	t.Run("a run is active", func(t *testing.T) {
		e := newDNSEnv(t, ChallengeDNS01)
		e.leftOver("_acme-challenge.old.example.com", "old-value", testNow)
		e.mgr.mu.Lock()
		e.mgr.active = &activeRun{jobID: "job-1", kind: KindIssue, trigger: TriggerAdmin, challenge: ChallengeDNS01, startedAt: e.clock.Now()}
		e.mgr.mu.Unlock()
		before := len(e.prov.Calls())

		e.mgr.round(context.Background())

		if n := len(e.prov.Calls()) - before; n != 0 {
			t.Errorf("a round made %d provider calls while a run was active, want 0", n)
		}
	})
}

func TestDNSSweepOfAnEmptyLedgerInTheSchedulerBuildsNothing(t *testing.T) {
	e := newDNSEnv(t, ChallengeDNS01)

	e.mgr.round(context.Background())

	if e.built.Load() != 0 || len(e.prov.Calls()) != 0 {
		t.Errorf("an empty ledger built %d providers and made %d calls, want none", e.built.Load(), len(e.prov.Calls()))
	}
	if left := leftOversOf(t, e.mgr); left == nil || len(left) != 0 {
		t.Errorf("left-over = %#v, want an empty list that is not nil", left)
	}
}
