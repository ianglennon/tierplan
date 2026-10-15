// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The restart-loop tracker (plan 06-34, RES-13, D-14, RESEARCH Pattern 8, E11, E12,
// Pitfalls 10 and 11). The clock is an argument: nothing here sleeps.

var rlNow = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

const (
	rlThreshold = 3
	rlWindow    = 10 * time.Minute
)

var rlServices = []dockerx.ServiceRow{
	{ID: "svc-web", Name: "web", Mode: "replicated"},
	{ID: "svc-job", Name: "nightly", Mode: "replicated-job"},
}

// rlFail is a failed task of service svc that failed ago before rlNow.
func rlFail(id, svc string, ago time.Duration, errText string) dockerx.FailedTaskRow {
	return dockerx.FailedTaskRow{ID: id, ServiceID: svc, NodeID: "node-1", Slot: 1, State: "failed", Err: errText, At: rlNow.Add(-ago)}
}

func observe(tr *RestartLoopTracker, at time.Time, failed ...dockerx.FailedTaskRow) map[string]RestartLoop {
	return tr.Observe(at, failed, rlServices, rlThreshold, rlWindow)
}

func TestRestartLoopFlagsAServiceWhoseTasksFailThresholdTimesInTheWindow(t *testing.T) {
	twoOnly := []dockerx.FailedTaskRow{rlFail("a", "svc-web", 3*time.Minute, "x"), rlFail("b", "svc-web", 2*time.Minute, "y")}
	if got := observe(NewRestartLoopTracker(), rlNow, twoOnly...); len(got) != 0 {
		t.Fatalf("two failures flagged %+v, want none at a threshold of %d", got, rlThreshold)
	}

	three := append([]dockerx.FailedTaskRow{rlFail("c", "svc-web", time.Minute, "task: non-zero exit (1)")}, twoOnly...)
	got := observe(NewRestartLoopTracker(), rlNow, three...)
	want := RestartLoop{
		ServiceID: "svc-web", Name: "web", Count: 3, Window: rlWindow,
		LatestError: "task: non-zero exit (1)", LatestAt: rlNow.Add(-time.Minute), Since: rlNow,
	}
	if len(got) != 1 || got["svc-web"] != want {
		t.Fatalf("flagged = %+v, want only %+v", got, want)
	}
}

func TestRestartLoopCountsTheFailuresOfEverySlotOfAServiceTogether(t *testing.T) {
	var rows []dockerx.FailedTaskRow
	for slot := 1; slot <= 3; slot++ {
		r := rlFail(fmt.Sprintf("t%d", slot), "svc-web", time.Duration(slot)*time.Minute, "boom")
		r.Slot = slot
		rows = append(rows, r)
	}
	if got := observe(NewRestartLoopTracker(), rlNow, rows...); got["svc-web"].Count != 3 {
		t.Fatalf("one failure in each of three slots = %+v, want a loop of 3", got)
	}
}

func TestRestartLoopNeverCountsShutdownCompleteOrRunningTasks(t *testing.T) {
	var rows []dockerx.FailedTaskRow
	for i, state := range []string{"shutdown", "complete", "running", "starting", ""} {
		for j := range 3 {
			r := rlFail(fmt.Sprintf("%s-%d-%d", state, i, j), "svc-web", time.Duration(j+1)*time.Minute, "exit 137")
			r.State = state
			rows = append(rows, r)
		}
	}
	if got := observe(NewRestartLoopTracker(), rlNow, rows...); len(got) != 0 {
		t.Fatalf("tasks that did not fail flagged %+v, want none (RESEARCH E12)", got)
	}
}

func TestRestartLoopCountsTheFailedTasksOfAJobButNotItsCompleteOnes(t *testing.T) {
	done := func(id string, ago time.Duration) dockerx.FailedTaskRow {
		r := rlFail(id, "svc-job", ago, "")
		r.State = "complete"
		return r
	}
	rows := []dockerx.FailedTaskRow{
		done("d1", time.Minute), done("d2", 2*time.Minute), done("d3", 3*time.Minute), done("d4", 4*time.Minute),
		rlFail("f1", "svc-job", 5*time.Minute, "e"), rlFail("f2", "svc-job", 6*time.Minute, "e"),
	}
	if got := observe(NewRestartLoopTracker(), rlNow, rows...); len(got) != 0 {
		t.Fatalf("four complete and two failed tasks of a job flagged %+v, want none", got)
	}
	rows = append(rows, rlFail("f3", "svc-job", 7*time.Minute, "e"))
	if got := observe(NewRestartLoopTracker(), rlNow, rows...); got["svc-job"].Count != 3 || got["svc-job"].Name != "nightly" {
		t.Fatalf("three failed tasks of a job = %+v, want a loop of 3 on nightly", got)
	}
}

func TestRestartLoopKeepsAFailureGoneFromDockersHistoryUntilItsWindowEnds(t *testing.T) {
	tr := NewRestartLoopTracker()
	// Two failures are seen, then Docker prunes them from its history; a third arrives.
	observe(tr, rlNow, rlFail("a", "svc-web", 2*time.Minute, "e"), rlFail("b", "svc-web", time.Minute, "e"))
	later := rlNow.Add(30 * time.Second)
	got := observe(tr, later, dockerx.FailedTaskRow{ID: "c", ServiceID: "svc-web", Slot: 1, State: "failed", Err: "newest", At: later.Add(-10 * time.Second)})
	if got["svc-web"].Count != 3 || got["svc-web"].LatestError != "newest" {
		t.Fatalf("after history was pruned = %+v, want 3 failures counted, the newest's error", got)
	}
}

func TestRestartLoopCountsATaskSeenInManyReadsOnce(t *testing.T) {
	tr := NewRestartLoopTracker()
	a, b := rlFail("a", "svc-web", 2*time.Minute, "e"), rlFail("b", "svc-web", time.Minute, "e")
	for i := range 5 {
		if got := observe(tr, rlNow.Add(time.Duration(i)*15*time.Second), a, b); len(got) != 0 {
			t.Fatalf("read %d flagged %+v: two tasks seen five times are still two", i, got)
		}
	}
	c := rlFail("c", "svc-web", 0, "e")
	if got := observe(tr, rlNow.Add(75*time.Second), a, b, c); got["svc-web"].Count != 3 {
		t.Fatalf("with a third task = %+v, want 3", got)
	}
}

func TestRestartLoopStaysFlaggedUntilAFullQuietWindowThenClears(t *testing.T) {
	tr := NewRestartLoopTracker()
	// Failures at -3m, -2m and -1m, then nothing new for the rest of the test.
	rows := []dockerx.FailedTaskRow{rlFail("a", "svc-web", 3*time.Minute, "e"), rlFail("b", "svc-web", 2*time.Minute, "e"), rlFail("c", "svc-web", time.Minute, "last")}
	if got := observe(tr, rlNow, rows...); got["svc-web"].Count != 3 {
		t.Fatalf("not flagged at the start: %+v", got)
	}
	steps := []struct {
		after     time.Duration
		wantCount int // 0 means no longer flagged
	}{
		{7*time.Minute - time.Second, 3}, // a is still in the window
		{7 * time.Minute, 2},             // a left it exactly a window after it failed; flagged still
		{8 * time.Minute, 1},
		{9*time.Minute - time.Second, 1},
		{9 * time.Minute, 0}, // c, the newest, is a full window old: cleared
	}
	for _, s := range steps {
		got := observe(tr, rlNow.Add(s.after))
		if s.wantCount == 0 {
			if len(got) != 0 {
				t.Fatalf("%v later still flagged %+v, want cleared after a quiet window", s.after, got)
			}
			continue
		}
		if got["svc-web"].Count != s.wantCount || got["svc-web"].LatestError != "last" {
			t.Fatalf("%v later = %+v, want still flagged with %d", s.after, got, s.wantCount)
		}
		if got["svc-web"].Since != rlNow {
			t.Fatalf("%v later the flag began %v, want it kept at %v", s.after, got["svc-web"].Since, rlNow)
		}
	}
	// After clearing, one new failure is one failure: the old ones are forgotten.
	if got := observe(tr, rlNow.Add(10*time.Minute), rlFail("z", "svc-web", -10*time.Minute, "e")); len(got) != 0 {
		t.Fatalf("one failure after a clear flagged %+v", got)
	}
}

func TestRestartLoopFlagIsRaisedAgainWithANewSinceAfterItCleared(t *testing.T) {
	tr := NewRestartLoopTracker()
	rows := func(prefix string, base time.Duration) []dockerx.FailedTaskRow {
		return []dockerx.FailedTaskRow{rlFail(prefix+"1", "svc-web", base, "e"), rlFail(prefix+"2", "svc-web", base, "e"), rlFail(prefix+"3", "svc-web", base, "e")}
	}
	observe(tr, rlNow, rows("a", 0)...)
	if got := observe(tr, rlNow.Add(rlWindow)); len(got) != 0 {
		t.Fatalf("still flagged after a quiet window: %+v", got)
	}
	again := rlNow.Add(time.Hour)
	got := tr.Observe(again, []dockerx.FailedTaskRow{
		{ID: "n1", ServiceID: "svc-web", State: "failed", At: again}, {ID: "n2", ServiceID: "svc-web", State: "failed", At: again}, {ID: "n3", ServiceID: "svc-web", State: "failed", At: again},
	}, rlServices, rlThreshold, rlWindow)
	if got["svc-web"].Since != again {
		t.Fatalf("flag raised again = %+v, want Since %v", got, again)
	}
}

func TestRestartLoopDropsAServiceThatNoLongerExists(t *testing.T) {
	tr := NewRestartLoopTracker()
	rows := []dockerx.FailedTaskRow{rlFail("a", "svc-web", 3*time.Minute, "e"), rlFail("b", "svc-web", 2*time.Minute, "e"), rlFail("c", "svc-web", time.Minute, "e")}
	if got := observe(tr, rlNow, rows...); len(got) != 1 {
		t.Fatalf("not flagged: %+v", got)
	}
	if got := tr.Observe(rlNow.Add(15*time.Second), nil, rlServices[1:], rlThreshold, rlWindow); len(got) != 0 {
		t.Fatalf("a removed service is still flagged: %+v", got)
	}
	// It does not come back from memory when a service with that ID appears again.
	if got := tr.Observe(rlNow.Add(30*time.Second), nil, rlServices, rlThreshold, rlWindow); len(got) != 0 {
		t.Fatalf("a removed service's failures were remembered: %+v", got)
	}
	// Failures reported for a service that is not listed are ignored.
	if got := tr.Observe(rlNow, []dockerx.FailedTaskRow{rlFail("x", "svc-gone", 0, "e"), rlFail("y", "svc-gone", 0, "e"), rlFail("z", "svc-gone", 0, "e")}, rlServices, rlThreshold, rlWindow); len(got) != 0 {
		t.Fatalf("failures of an unknown service flagged %+v", got)
	}
}

func TestRestartLoopUsesTheThresholdAndWindowItIsGiven(t *testing.T) {
	cases := []struct {
		name      string
		threshold int
		window    time.Duration
		ages      []time.Duration
		want      int // flagged count, 0 for not flagged
	}{
		{"two of two", 2, rlWindow, []time.Duration{time.Minute, 2 * time.Minute}, 2},
		{"one short of the threshold", 4, rlWindow, []time.Duration{0, time.Minute, 2 * time.Minute}, 0},
		{"the threshold itself flags", 3, rlWindow, []time.Duration{0, time.Minute, 2 * time.Minute}, 3},
		{"a failure a whole window old is out", 3, rlWindow, []time.Duration{0, time.Minute, rlWindow}, 0},
		{"a failure a second inside the window is in", 3, rlWindow, []time.Duration{0, time.Minute, rlWindow - time.Second}, 3},
		{"a short window", 2, time.Minute, []time.Duration{0, 59 * time.Second, 61 * time.Second}, 2},
		{"a long window", 3, 2 * time.Hour, []time.Duration{0, time.Hour, 119 * time.Minute}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var rows []dockerx.FailedTaskRow
			for i, age := range c.ages {
				rows = append(rows, rlFail(fmt.Sprintf("t%d", i), "svc-web", age, "e"))
			}
			got := NewRestartLoopTracker().Observe(rlNow, rows, rlServices, c.threshold, c.window)
			if c.want == 0 && len(got) != 0 {
				t.Fatalf("flagged %+v, want none", got)
			}
			if c.want != 0 && got["svc-web"].Count != c.want {
				t.Fatalf("flagged %+v, want a loop of %d", got, c.want)
			}
		})
	}
}

func TestRestartLoopFlagsEachServiceOnItsOwn(t *testing.T) {
	rows := []dockerx.FailedTaskRow{
		rlFail("w1", "svc-web", 1*time.Minute, "e"), rlFail("w2", "svc-web", 2*time.Minute, "e"), rlFail("w3", "svc-web", 3*time.Minute, "e"),
		rlFail("j1", "svc-job", 1*time.Minute, "e"), rlFail("j2", "svc-job", 2*time.Minute, "e"),
	}
	got := observe(NewRestartLoopTracker(), rlNow, rows...)
	if _, ok := got["svc-job"]; ok || len(got) != 1 || got["svc-web"].Count != 3 {
		t.Fatalf("flagged %+v, want web only: three and two failures are not five", got)
	}
}

func TestRestartLoopCountsAFailureWithNoTimeFromWhenItWasFirstSeen(t *testing.T) {
	tr := NewRestartLoopTracker()
	untimed := func(id string) dockerx.FailedTaskRow {
		return dockerx.FailedTaskRow{ID: id, ServiceID: "svc-web", State: "failed"}
	}
	if got := observe(tr, rlNow, untimed("a"), untimed("b"), untimed("c")); got["svc-web"].Count != 3 {
		t.Fatalf("untimed failures = %+v, want 3 counted from now", got)
	}
	if got := observe(tr, rlNow.Add(rlWindow-time.Second), untimed("a"), untimed("b"), untimed("c")); got["svc-web"].Count != 3 {
		t.Fatalf("seen again before the window ended = %+v, want them kept, not renewed or lost", got)
	}
	if got := observe(tr, rlNow.Add(rlWindow), untimed("a"), untimed("b"), untimed("c")); len(got) != 0 {
		t.Fatalf("seen again a window after the first sight = %+v, want cleared", got)
	}
}

func TestRestartLoopDefaultsASettingThatIsNotPositive(t *testing.T) {
	rows := []dockerx.FailedTaskRow{rlFail("a", "svc-web", time.Minute, "e"), rlFail("b", "svc-web", 2*time.Minute, "e"), rlFail("c", "svc-web", 3*time.Minute, "e")}
	got := NewRestartLoopTracker().Observe(rlNow, rows, rlServices, 0, 0)
	if got["svc-web"].Count != DefaultRestartLoopFailures || DefaultRestartLoopFailures != 3 || DefaultRestartLoopWindow != 10*time.Minute {
		t.Fatalf("with no setting = %+v, want the defaults of 3 failures in 10 minutes", got)
	}
}

// TestRestartLoopTrackerCannotReachDocker is the prohibition of plan 06-34: detection only
// flags. The tracker holds no client and no function, so it has nothing to restart, stop or
// scale a service with (RES-13 safety, T-06-99).
func TestRestartLoopTrackerCannotReachDocker(t *testing.T) {
	var walk func(ty reflect.Type, path string, seen map[reflect.Type]bool)
	walk = func(ty reflect.Type, path string, seen map[reflect.Type]bool) {
		if seen[ty] {
			return
		}
		seen[ty] = true
		switch ty.Kind() {
		case reflect.Func, reflect.Interface, reflect.Chan:
			t.Errorf("%s is a %s: the tracker may hold data only", path, ty.Kind())
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(ty.Elem(), path+"[]", seen)
		case reflect.Map:
			walk(ty.Key(), path+"{key}", seen)
			walk(ty.Elem(), path+"{}", seen)
		case reflect.Struct:
			if strings.Contains(ty.PkgPath(), "dockerx") || strings.Contains(ty.PkgPath(), "moby") {
				t.Errorf("%s is %s: the tracker must have no Docker type", path, ty)
			}
			for i := range ty.NumField() {
				f := ty.Field(i)
				if ty == reflect.TypeFor[time.Time]() {
					continue
				}
				walk(f.Type, path+"."+f.Name, seen)
			}
		}
	}
	walk(reflect.TypeFor[RestartLoopTracker](), "RestartLoopTracker", map[reflect.Type]bool{})
}
