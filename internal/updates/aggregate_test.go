// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
)

func verdicts(states ...updates.State) []updates.Verdict {
	out := make([]updates.Verdict, len(states))
	for i, s := range states {
		out[i] = updates.Verdict{State: s}
	}
	return out
}

// The roll-up of a stack (plan 12-07, UI-SPEC "Stack aggregate", IMG-05).
func TestAggregateTable(t *testing.T) {
	t.Parallel()
	const (
		upd    = updates.StateUpdateAvailable
		ok     = updates.StateUpToDate
		stale  = updates.StateCheckStale
		failed = updates.StateCheckFailed
		pin    = updates.StatePinned
		local  = updates.StateNotFromRegistry
	)
	cases := []struct {
		name    string
		in      []updates.State
		want    updates.State
		count   int
		notable []updates.State
	}{
		{"no workload", nil, updates.StateNone, 0, nil},
		{"nothing checkable", []updates.State{pin, local}, updates.StateNone, 0, []updates.State{pin, local}},
		{"one pinned", []updates.State{pin}, updates.StateNone, 0, []updates.State{pin}},
		{"all up to date", []updates.State{ok, ok}, ok, 0, nil},
		{"one up to date", []updates.State{ok}, ok, 0, nil},
		{"up to date and stale", []updates.State{ok, stale}, stale, 0, nil},
		{"up to date and failed", []updates.State{ok, failed}, failed, 0, nil},
		{"update beats failed", []updates.State{upd, failed}, upd, 1, nil},
		{"three updates beside a pinned one", []updates.State{upd, upd, upd, pin}, upd, 3, []updates.State{pin}},
		{"up to date beside an uncheckable one", []updates.State{ok, local}, ok, 0, []updates.State{local}},
		{"skipped and ignored", []updates.State{updates.StateSkipped, updates.StateIgnored}, updates.StateIgnored, 0, nil},
		{"skipped alone", []updates.State{updates.StateSkipped}, updates.StateSkipped, 0, nil},
		{"skipped beside up to date", []updates.State{updates.StateSkipped, ok}, updates.StateSkipped, 0, nil},
		{"ignored beside up to date", []updates.State{ok, updates.StateIgnored}, updates.StateIgnored, 0, nil},
		{"skipped beside stale", []updates.State{updates.StateSkipped, stale}, stale, 0, nil},
		{"not checked beats stale", []updates.State{stale, updates.StateNotChecked}, updates.StateNotChecked, 0, nil},
		{"rollback failed beats failed", []updates.State{failed, updates.StateRollbackFailed}, updates.StateRollbackFailed, 0, nil},
		{"rolled back beats failed", []updates.State{failed, updates.StateRolledBack}, updates.StateRolledBack, 0, nil},
		{"updating beats an update", []updates.State{upd, updates.StateUpdating}, updates.StateUpdating, 0, nil},
		{"busy beats an update", []updates.State{upd, updates.StateBusy}, updates.StateBusy, 0, nil},
		{"updating beats busy", []updates.State{updates.StateBusy, updates.StateUpdating}, updates.StateUpdating, 0, nil},
		{"queued beats an update", []updates.State{upd, updates.StateQueued}, updates.StateQueued, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := updates.Aggregate(verdicts(c.in...))
			if got.State != c.want || got.Count != c.count || !slices.Equal(got.NotCheckable, c.notable) {
				t.Errorf("Aggregate(%v) = %+v, want state %s count %d notCheckable %v", c.in, got, c.want, c.count, c.notable)
			}
			// The order the verdicts arrive in never matters.
			rev := slices.Clone(c.in)
			slices.Reverse(rev)
			if again := updates.Aggregate(verdicts(rev...)); again.State != got.State || again.Count != got.Count {
				t.Errorf("reversed = %+v, want %+v", again, got)
			}
		})
	}
}

// A stack reads up_to_date only when it has a checkable workload and every one is up to date (T-12-17, IMG-05).
func TestAggregateNeverUpToDateUnlessAll(t *testing.T) {
	t.Parallel()
	all := []updates.State{
		updates.StateUpdateAvailable, updates.StateUpdating, updates.StateBusy, updates.StateQueued, updates.StateUpToDate,
		updates.StateCheckStale, updates.StateCheckFailed, updates.StateRolledBack, updates.StateRollbackFailed,
		updates.StateNotFromRegistry, updates.StatePinned, updates.StateNotChecked, updates.StateSkipped, updates.StateIgnored,
		"a_state_nobody_knows",
	}
	var walk func(prefix []updates.State, depth int)
	walk = func(prefix []updates.State, depth int) {
		if len(prefix) > 0 {
			got := updates.Aggregate(verdicts(prefix...))
			checkable, upToDate := 0, 0
			for _, s := range prefix {
				if s == updates.StatePinned || s == updates.StateNotFromRegistry {
					continue
				}
				checkable++
				if s == updates.StateUpToDate {
					upToDate++
				}
			}
			wantUpToDate := checkable > 0 && checkable == upToDate
			if (got.State == updates.StateUpToDate) != wantUpToDate {
				t.Fatalf("Aggregate(%v) = %s, up_to_date must be answered exactly when every checkable workload is up to date", prefix, got.State)
			}
			if checkable == 0 && got.State != updates.StateNone {
				t.Fatalf("Aggregate(%v) = %s, want none: nothing can be checked", prefix, got.State)
			}
		}
		if depth == 0 {
			return
		}
		for _, s := range all {
			walk(append(slices.Clone(prefix), s), depth-1)
		}
	}
	walk(nil, 3)
}

// A stack reads as pull-failed, and not as rolled back, only when every workload that is rolled back failed at the pull of its
// image: nothing was changed. One workload that really was put back makes it a rollback.
func TestAggregatePullFailed(t *testing.T) {
	t.Parallel()
	pull := updates.Verdict{State: updates.StateRolledBack, Failure: &updates.Failure{Reason: updates.ReasonPullFailed}}
	back := updates.Verdict{State: updates.StateRolledBack, Failure: &updates.Failure{Reason: updates.ReasonUnhealthy}}
	bare := updates.Verdict{State: updates.StateRolledBack}
	ok := updates.Verdict{State: updates.StateUpToDate}
	cases := []struct {
		name string
		in   []updates.Verdict
		want bool
	}{
		{"one failed pull", []updates.Verdict{pull}, true},
		{"a failed pull beside an up to date one", []updates.Verdict{pull, ok}, true},
		{"two failed pulls", []updates.Verdict{pull, pull}, true},
		{"a failed pull and a real rollback", []updates.Verdict{pull, back}, false},
		{"a real rollback", []updates.Verdict{back}, false},
		{"a rollback with no failure at hand", []updates.Verdict{bare}, false},
		{"a failed pull and a rollback with no failure at hand", []updates.Verdict{pull, bare}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := updates.Aggregate(c.in)
			if got.State != updates.StateRolledBack || got.PullFailed != c.want {
				t.Errorf("Aggregate = %+v, want rolled_back with PullFailed %v", got, c.want)
			}
		})
	}
	// Only a rolled_back roll-up can be pull-failed: a failed pull that something else outranks says nothing.
	upd := updates.Verdict{State: updates.StateUpdateAvailable}
	if got := updates.Aggregate([]updates.Verdict{pull, upd}); got.State != updates.StateUpdateAvailable || got.PullFailed {
		t.Errorf("Aggregate(pull, update) = %+v, want update_available, not PullFailed", got)
	}
}
