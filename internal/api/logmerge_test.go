// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/coaming/proto/testsupport"
)

// The merge of a service's log (plan 06-29, OBS-02, D-37, RESEARCH E1): the engine answers
// task by task, so the burst is sorted as a whole and live lines are held in a short
// window to put them in time order.

var mergeT0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// rec is a record at an offset in milliseconds from mergeT0 on a task.
func rec(task string, ms int, text string) svcRec {
	return svcRec{T: mergeT0.Add(time.Duration(ms) * time.Millisecond).Format(logTimeLayout), S: streamOut, M: text, TaskID: task}
}

func texts(rs []svcRec) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.M)
	}
	return out
}

func slotsOf(slots map[string]int) map[string]taskInfo {
	out := map[string]taskInfo{}
	for id, s := range slots {
		out[id] = taskInfo{ID: id, Slot: s}
	}
	return out
}

func TestMergeBurstSortsTheWholeBurstByTimeAcrossTasks(t *testing.T) {
	t.Parallel()
	// The engine's answer: task by task, each in time order, tasks not interleaved.
	burst := []svcRec{
		rec("t1", 10, "a1"), rec("t1", 40, "a2"), rec("t1", 70, "a3"),
		rec("t2", 20, "b1"), rec("t2", 50, "b2"),
		rec("t3", 30, "c1"), rec("t3", 60, "c2"),
	}
	got := texts(mergeBurst(burst, slotsOf(map[string]int{"t1": 1, "t2": 2, "t3": 3}), 500))
	if want := []string{"a1", "b1", "c1", "a2", "b2", "c2", "a3"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestMergeBurstKeepsTheNewestN(t *testing.T) {
	t.Parallel()
	burst := []svcRec{rec("t1", 10, "a1"), rec("t1", 40, "a2"), rec("t2", 20, "b1"), rec("t2", 50, "b2"), rec("t2", 60, "b3")}
	got := texts(mergeBurst(burst, slotsOf(map[string]int{"t1": 1, "t2": 2}), 3))
	if want := []string{"a2", "b2", "b3"}; !slices.Equal(got, want) {
		t.Fatalf("newest 3 = %v, want %v (cut after sorting, not per task)", got, want)
	}
	if got := mergeBurst(nil, nil, 500); len(got) != 0 {
		t.Errorf("an empty burst merged to %v", got)
	}
	if got := mergeBurst(burst, nil, 0); len(got) != 0 {
		t.Errorf("n = 0 kept %v", got)
	}
}

func TestMergeBurstOrdersTheSameTimestampByLowerSlotThenByArrival(t *testing.T) {
	t.Parallel()
	// Three tasks write at the same instant; the slot decides, and two lines of one task keep
	// the order they arrived in.
	burst := []svcRec{
		rec("t3", 10, "slot3"), rec("t3", 10, "slot3 second"),
		rec("t1", 10, "slot1"),
		rec("t2", 10, "slot2"),
	}
	got := texts(mergeBurst(burst, slotsOf(map[string]int{"t1": 1, "t2": 2, "t3": 3}), 500))
	if want := []string{"slot1", "slot2", "slot3", "slot3 second"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestMergeBurstPutsATaskTheListDoesNotKnowAfterTheKnownOnesAtTheSameTime(t *testing.T) {
	t.Parallel()
	burst := []svcRec{rec("gone", 10, "unknown"), rec("t1", 10, "known")}
	got := texts(mergeBurst(burst, slotsOf(map[string]int{"t1": 1}), 500))
	if want := []string{"known", "unknown"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestReorderWindowReleasesALineOnlyAfterTheWindowAndInTimeOrder(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	w := newReorderWindow(500*time.Millisecond, clk.now, func(string) int { return 0 })

	w.add(rec("t1", 200, "late-time")) // arrives first, has the larger time
	clk.t = clk.t.Add(100 * time.Millisecond)
	w.add(rec("t2", 100, "early-time"))
	if got := w.release(); len(got) != 0 {
		t.Fatalf("released %v before any line had waited 500 ms", texts(got))
	}
	if d, ok := w.wait(); !ok || d != 500*time.Millisecond {
		t.Fatalf("wait = %v, %v, want 500 ms: the head is the smaller time, which arrived just now", d, ok)
	}

	// 500 ms after the first arrival the head (smaller time, arrived at +100) is not yet due.
	clk.t = mergeT0.Add(500 * time.Millisecond)
	if got := w.release(); len(got) != 0 {
		t.Fatalf("released %v while the head had waited 400 ms: order would break", texts(got))
	}
	clk.t = mergeT0.Add(600 * time.Millisecond)
	got := texts(w.release())
	if want := []string{"early-time", "late-time"}; !slices.Equal(got, want) {
		t.Fatalf("released %v, want %v in time order", got, want)
	}
	if _, ok := w.wait(); ok {
		t.Error("an empty window still asks to be woken")
	}
}

func TestReorderWindowReleasesALineOlderThanTheLastReleasedNextWithItsOwnTime(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	w := newReorderWindow(500*time.Millisecond, clk.now, func(string) int { return 0 })
	w.add(rec("t1", 300, "newer"))
	clk.t = clk.t.Add(time.Second)
	if got := texts(w.release()); !slices.Equal(got, []string{"newer"}) {
		t.Fatalf("released %v, want the first line", got)
	}
	// A line from before the one already released arrives: it cannot be put back in order,
	// so it goes next, at once, still carrying its own time.
	w.add(rec("t2", 100, "straggler"))
	out := w.release()
	if len(out) != 1 || out[0].M != "straggler" || out[0].T != rec("t2", 100, "").T {
		t.Fatalf("released %+v, want the straggler with its own time", out)
	}
}

func TestReorderWindowBreaksTiesByLowerSlotThenArrivalAndDrainsEverything(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	slots := map[string]int{"t1": 1, "t2": 2}
	w := newReorderWindow(500*time.Millisecond, clk.now, func(id string) int { return slots[id] })
	w.add(rec("t2", 10, "two"))
	w.add(rec("t1", 10, "one"))
	w.add(rec("t1", 10, "one again"))
	w.add(rec("t1", 5, "first"))
	got := texts(w.drain())
	if want := []string{"first", "one", "one again", "two"}; !slices.Equal(got, want) {
		t.Fatalf("drained %v, want %v", got, want)
	}
	if len(w.drain()) != 0 {
		t.Error("a drained window still holds lines")
	}
}

func TestTaskMetaRefreshesWhenStaleAndOnAnUnknownTaskButNotInABurst(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	calls := 0
	tasks := []taskInfo{{ID: "t1", Slot: 1, Node: "alpha", State: "running"}}
	m := newTaskMeta(func(context.Context) ([]taskInfo, error) { calls++; return slices.Clone(tasks), nil }, clk.now)
	ctx := context.Background()

	if ti, ok := m.lookup(ctx, "t1"); !ok || ti.Slot != 1 || ti.Node != "alpha" {
		t.Fatalf("lookup t1 = %+v, %v", ti, ok)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want the first lookup to load once", calls)
	}
	m.lookup(ctx, "t1")
	if calls != 1 {
		t.Errorf("calls = %d after a lookup of a known task inside 3 s, want 1", calls)
	}

	// A task the cache does not know is loaded for, but not right after a read, and the same one
	// not more than once a second.
	tasks = append(tasks, taskInfo{ID: "t2", Slot: 2, Node: "beta"})
	clk.t = clk.t.Add(1500 * time.Millisecond)
	if ti, ok := m.lookup(ctx, "t2"); !ok || ti.Slot != 2 || calls != 2 {
		t.Fatalf("lookup t2 = %+v, %v after %d calls, want a refresh that finds it", ti, ok, calls)
	}
	if _, ok := m.lookup(ctx, "t-gone"); ok || calls != 2 {
		t.Fatalf("lookup of a task nobody has right after a refresh: found %v after %d calls, want 2", ok, calls)
	}
	clk.t = clk.t.Add(1100 * time.Millisecond)
	if _, ok := m.lookup(ctx, "t-gone"); ok || calls != 3 {
		t.Fatalf("lookup of a task nobody has a second later: found %v after %d calls, want a refresh (3)", ok, calls)
	}
	m.lookup(ctx, "t-gone")
	m.lookup(ctx, "t-gone")
	if calls != 3 {
		t.Errorf("calls = %d, want the unknown task not to hammer the manager", calls)
	}

	// Stale after 3 s even for a known task.
	clk.t = clk.t.Add(3100 * time.Millisecond)
	m.all(ctx)
	if calls != 4 {
		t.Errorf("calls = %d, want a refresh of a stale cache", calls)
	}
}

func TestTaskMetaKeepsWhatItKnewWhenARefreshFails(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	fail := false
	m := newTaskMeta(func(context.Context) ([]taskInfo, error) {
		if fail {
			return nil, errors.New("manager gone")
		}
		return []taskInfo{{ID: "t1", Slot: 1}}, nil
	}, clk.now)
	m.lookup(context.Background(), "t1")
	fail = true
	clk.t = clk.t.Add(10 * time.Second)
	if ti, ok := m.lookup(context.Background(), "t1"); !ok || ti.Slot != 1 {
		t.Fatalf("lookup after a failed refresh = %+v, %v, want the old answer", ti, ok)
	}
	if got := fmt.Sprint(len(m.all(context.Background()))); got != "1" {
		t.Errorf("all = %s tasks, want 1", got)
	}
}

// The k-way merge and the holder of a service's merged download (plan 06-31, D-35, T-06-91).

func mergedTexts(lists []taskLines) []string {
	var out []string
	for r := range kWayMerge(lists) {
		out = append(out, r.M)
	}
	return out
}

func TestKWayMergeInterleavesOrderedTaskListsInTimeOrder(t *testing.T) {
	t.Parallel()
	lists := []taskLines{
		{Slot: 1, Lines: []svcRec{rec("t1", 10, "a1"), rec("t1", 40, "a2"), rec("t1", 70, "a3")}},
		{Slot: 2, Lines: []svcRec{rec("t2", 20, "b1"), rec("t2", 50, "b2")}},
		{Slot: 3, Lines: []svcRec{rec("t3", 30, "c1")}},
	}
	if got, want := mergedTexts(lists), []string{"a1", "b1", "c1", "a2", "b2", "a3"}; !slices.Equal(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

func TestKWayMergeBreaksTiesByTheLowerSlotWhateverTheOrderOfTheLists(t *testing.T) {
	t.Parallel()
	lists := []taskLines{
		{Slot: 3, Lines: []svcRec{rec("t3", 10, "c1")}},
		{Slot: 1, Lines: []svcRec{rec("t1", 10, "a1"), rec("t1", 10, "a2")}},
		{Slot: 2, Lines: []svcRec{rec("t2", 10, "b1")}},
	}
	if got, want := mergedTexts(lists), []string{"a1", "a2", "b1", "c1"}; !slices.Equal(got, want) {
		t.Errorf("merged = %v, want %v: same stamp, lower slot first, a task's own order kept", got, want)
	}
}

func TestKWayMergeOfNothingOrOneListIsTheListAsItIs(t *testing.T) {
	t.Parallel()
	if got := mergedTexts(nil); len(got) != 0 {
		t.Errorf("merged nothing = %v", got)
	}
	if got := mergedTexts([]taskLines{{Slot: 1}, {Slot: 2}}); len(got) != 0 {
		t.Errorf("merged empty lists = %v", got)
	}
	one := []taskLines{{Slot: 1, Lines: []svcRec{rec("t1", 5, "x"), rec("t1", 6, "y")}}}
	if got, want := mergedTexts(one), []string{"x", "y"}; !slices.Equal(got, want) {
		t.Errorf("merged one list = %v, want %v", got, want)
	}
}

func TestKWayMergeStopsWhenTheConsumerDoes(t *testing.T) {
	t.Parallel()
	lists := []taskLines{{Slot: 1, Lines: []svcRec{rec("t1", 1, "a"), rec("t1", 3, "c")}}, {Slot: 2, Lines: []svcRec{rec("t2", 2, "b")}}}
	var got []string
	for r := range kWayMerge(lists) {
		got = append(got, r.M)
		if len(got) == 2 {
			break
		}
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
}

func TestLineHolderKeepsEverythingUnderItsSizeGroupedByTaskInTheOrderGiven(t *testing.T) {
	t.Parallel()
	h := newLineHolder(1 << 20)
	h.add(rec("t1", 10, "a1"), 1)
	h.add(rec("t1", 40, "a2"), 1)
	h.add(rec("t2", 20, "b1"), 2)
	if h.cut() {
		t.Error("the holder says it cut lines though it is far under its size")
	}
	if got, want := mergedTexts(h.lists()), []string{"a1", "b1", "a2"}; !slices.Equal(got, want) {
		t.Errorf("held = %v, want %v", got, want)
	}
}

func TestLineHolderDropsTheOldestLineOfAnyTaskWhenOverItsSizeAndSaysSo(t *testing.T) {
	t.Parallel()
	limit := 3 * recSize(rec("t1", 10, "a1"))
	h := newLineHolder(limit)
	// The engine gives a service's log task by task, so the oldest line is not the first.
	for _, r := range []svcRec{rec("t1", 10, "a1"), rec("t1", 40, "a2"), rec("t1", 70, "a3")} {
		h.add(r, 1)
	}
	for _, r := range []svcRec{rec("t2", 20, "b1"), rec("t2", 50, "b2")} {
		h.add(r, 2)
	}
	h.add(rec("t3", 30, "c1"), 3)
	h.add(rec("t3", 5, "c0"), 3)
	if !h.cut() {
		t.Error("the holder does not say it cut lines")
	}
	if got, want := mergedTexts(h.lists()), []string{"a2", "b2", "a3"}; !slices.Equal(got, want) {
		t.Errorf("held = %v, want the newest three %v", got, want)
	}
}

func TestLineHolderNeverHoldsMoreThanItsSize(t *testing.T) {
	t.Parallel()
	limit := 5 * recSize(rec("t1", 10, "aa"))
	h := newLineHolder(limit)
	for i := range 200 {
		h.add(rec(fmt.Sprintf("t%d", i%4), i, "aa"), i%4)
		size := 0
		for _, l := range h.lists() {
			for _, r := range l.Lines {
				size += recSize(r)
			}
		}
		if size > limit {
			t.Fatalf("after %d records the holder holds %d bytes, over %d", i+1, size, limit)
		}
	}
}

// ---- the merge of several sources (plan 07-29, STK-10, D-22) ----

// srcOf is a source with the records given, already closed: the engine gave all it had.
func srcOf(name string, recs ...svcRec) logSource {
	ch := make(chan svcRec, len(recs))
	for _, r := range recs {
		ch <- r
	}
	close(ch)
	return logSource{Name: name, Records: ch}
}

// mergeSourcesTexts runs mergeSources over sources and returns the texts it handed out, in order.
func mergeSourcesTexts(t *testing.T, sources []logSource, opts ...mergeOption) []string {
	t.Helper()
	var got []svcRec
	err := mergeSources(testsupport.Context(t), sources, reorderHold, func(r svcRec) bool {
		got = append(got, r)
		return true
	}, opts...)
	if err != nil {
		t.Fatalf("mergeSources: %v", err)
	}
	return texts(got)
}

func TestMergeSourcesPutsThreeSourcesWithInterleavedTimesInTimeOrder(t *testing.T) {
	t.Parallel()
	// Each service gave its lines task by task; the times of the three interleave.
	got := mergeSourcesTexts(t, []logSource{
		srcOf("web", rec("w1", 10, "web a"), rec("w1", 40, "web b"), rec("w2", 25, "web c")),
		srcOf("api", rec("a1", 20, "api a"), rec("a1", 50, "api b")),
		srcOf("db", rec("d1", 5, "db a"), rec("d1", 30, "db b"), rec("d1", 60, "db c")),
	})
	want := []string{"db a", "web a", "api a", "web c", "db b", "web b", "api b", "db c"}
	if !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestMergeSourcesBreaksATieByServiceNameThenByTheLowerSlot(t *testing.T) {
	t.Parallel()
	tasks := map[string]taskInfo{
		"w1": {ID: "w1", Slot: 1}, "w2": {ID: "w2", Slot: 2},
		"a1": {ID: "a1", Slot: 3},
	}
	// Four lines at one instant: api sorts before web whatever the slot, and web's slot 1 before 2.
	got := mergeSourcesTexts(t, []logSource{
		srcOf("web", rec("w2", 10, "web.2"), rec("w1", 10, "web.1")),
		srcOf("api", rec("a1", 10, "api.3")),
	}, withTasks(func() map[string]taskInfo { return tasks }))
	if want := []string{"api.3", "web.1", "web.2"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestMergeSourcesKeepsTheNewestLinesOfTheBurstAcrossAllSources(t *testing.T) {
	t.Parallel()
	got := mergeSourcesTexts(t, []logSource{
		srcOf("web", rec("w1", 10, "web old"), rec("w1", 50, "web new")),
		srcOf("api", rec("a1", 20, "api old"), rec("a1", 60, "api new")),
	}, withBurstKeep(2))
	if want := []string{"web new", "api new"}; !slices.Equal(got, want) {
		t.Fatalf("kept = %v, want the newest two of all %v", got, want)
	}
}

func TestMergeSourcesLeavesOutWhatTheFilterRefuses(t *testing.T) {
	t.Parallel()
	got := mergeSourcesTexts(t, []logSource{
		srcOf("web", rec("w1", 10, "keep"), rec("hidden", 20, "drop")),
	}, withAccept(func(r svcRec) bool { return r.TaskID != "hidden" }))
	if want := []string{"keep"}; !slices.Equal(got, want) {
		t.Fatalf("merged = %v, want %v", got, want)
	}
}

func TestMergeSourcesOfNoSourcesEndsAtOnce(t *testing.T) {
	t.Parallel()
	if got := mergeSourcesTexts(t, nil); len(got) != 0 {
		t.Fatalf("merged = %v", got)
	}
}

func TestMergeSourcesHoldsLiveLinesToPutThemInOrderAndEndsWhenTheSourcesDo(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: mergeT0}
	live := make(chan svcRec)
	ended := make(chan struct{}, 1)
	out := make(chan string, 16)
	done := make(chan error, 1)
	ctx := testsupport.Context(t)
	go func() {
		done <- mergeSources(ctx, []logSource{{Name: "web", Records: live}}, 500*time.Millisecond, func(r svcRec) bool {
			out <- r.M
			return true
		}, withClock(func() time.Time { return clk.t }), withBurstEnded(func() { ended <- struct{}{} }))
	}()
	// Nothing came before the burst ended on its quiet period; from then on lines are live.
	testsupport.Await(t, "the burst to end", ended)

	// The later line arrives first, the earlier one just after: the window puts them in order.
	live <- rec("w1", 200, "later")
	live <- rec("w1", 100, "earlier")
	close(live)
	// Closing the source drains the window, whatever the clock says.
	if err := testsupport.Await(t, "the merge to end", done); err != nil {
		t.Fatalf("mergeSources: %v", err)
	}
	close(out)
	var got []string
	for s := range out {
		got = append(got, s)
	}
	if want := []string{"earlier", "later"}; !slices.Equal(got, want) {
		t.Fatalf("live lines = %v, want %v", got, want)
	}
}
