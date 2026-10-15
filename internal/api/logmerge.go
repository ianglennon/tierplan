// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"container/heap"
	"context"
	"iter"
	"maps"
	"math"
	"slices"
	"sync"
	"time"
)

// The merge of a service's log (plan 06-29, OBS-02, D-37). A swarm manager answers a service
// log task by task and applies tail per task (RESEARCH E1), so the burst a stream opens with
// is sorted as a whole, and the lines that follow are held in a short window and released in
// time order.

const (
	// reorderHold is how long a live line waits for an older line from another task to
	// arrive before it is released.
	reorderHold = 500 * time.Millisecond
	// taskMetaTTL is how long what is known about a service's tasks is believed. A task ID the
	// cache does not hold asks for a read, but not within taskMetaMinGap of the last read
	// (a burst of unknown IDs shares one), and the same ID not again within taskMetaAskGap.
	taskMetaTTL    = 3 * time.Second
	taskMetaMinGap = 250 * time.Millisecond
	taskMetaAskGap = time.Second
	// maxAsked bounds the unknown IDs remembered.
	maxAsked = 1024
)

// svcRec is one record of a service log on its way to the browser: the engine's stamp in the
// fixed layout (so stamps sort as text), the stream, the text, and the task and node the
// manager named in the record's details.
type svcRec struct {
	T, S, M, TaskID, NodeID string
	// seq is the order the record arrived in; it breaks ties that time and slot leave.
	seq uint64
}

// taskInfo is what the stream knows of one task of the service.
type taskInfo struct {
	ID   string
	Slot int
	// NodeID is the swarm node, Node its host name (the ID while the swarm does not list it).
	NodeID, Node string
	// State is the state to show (taskStateUnknown for a task on a node that cannot vouch for
	// it), Ended says Docker's own state is one the task cannot leave.
	State string
	Ended bool
	// NodeState is the swarm's word for the node (empty when the swarm does not list it).
	NodeState string
	// Created is when the swarm made the task, Since when it reached its state (its last
	// change when it has no state time), ExitCode the exit code of its container when it has
	// one (plan 06-30, D-34).
	Created, Since time.Time
	ExitCode       *int
}

// slotOrLast is the slot of a task for ordering; a task nobody has listed goes after the
// ones that are.
func slotOrLast(tasks map[string]taskInfo, id string) int {
	if ti, ok := tasks[id]; ok {
		return ti.Slot
	}
	return math.MaxInt
}

// mergeBurst sorts a burst by time, then by the lower slot, then by arrival, and keeps the
// newest n. The slot of a task comes from tasks.
func mergeBurst(recs []svcRec, tasks map[string]taskInfo, n int) []svcRec {
	if n <= 0 || len(recs) == 0 {
		return nil
	}
	out := slices.Clone(recs)
	slices.SortStableFunc(out, func(a, b svcRec) int {
		return cmp.Or(
			cmp.Compare(a.T, b.T),
			cmp.Compare(slotOrLast(tasks, a.TaskID), slotOrLast(tasks, b.TaskID)),
			cmp.Compare(a.seq, b.seq),
		)
	})
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// held is a record in the reorder window.
type held struct {
	rec     svcRec
	slot    int
	arrived time.Time
}

type heldHeap []held

func (h heldHeap) Len() int { return len(h) }
func (h heldHeap) Less(i, j int) bool {
	return cmp.Or(cmp.Compare(h[i].rec.T, h[j].rec.T), cmp.Compare(h[i].slot, h[j].slot), cmp.Compare(h[i].rec.seq, h[j].rec.seq)) < 0
}
func (h heldHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *heldHeap) Push(x any) {
	if r, ok := x.(held); ok {
		*h = append(*h, r)
	}
}
func (h *heldHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = held{}
	*h = old[:n-1]
	return x
}

// reorderWindow puts live lines in time order. A line is held until it has waited the hold
// since it arrived, and lines go out smallest time first (the lower slot, then the arrival,
// when times are equal). A line older than the newest one already released cannot be put
// back in order, so it goes out at the next release, with its own time.
type reorderWindow struct {
	hold   time.Duration
	now    func() time.Time
	slotOf func(taskID string) int
	h      heldHeap
	seq    uint64
	// lastT is the newest stamp released.
	lastT string
}

func newReorderWindow(hold time.Duration, now func() time.Time, slotOf func(string) int) *reorderWindow {
	return &reorderWindow{hold: hold, now: now, slotOf: slotOf}
}

// seed says the lines up to t have been sent already, so an older line is a straggler.
func (w *reorderWindow) seed(t string) {
	if t > w.lastT {
		w.lastT = t
	}
}

// add holds a record.
func (w *reorderWindow) add(r svcRec) {
	w.seq++
	r.seq = w.seq
	heap.Push(&w.h, held{rec: r, slot: w.slotOf(r.TaskID), arrived: w.now()})
}

// due reports whether the head may go out now.
func (w *reorderWindow) due() bool {
	if len(w.h) == 0 {
		return false
	}
	head := w.h[0]
	if w.lastT != "" && head.rec.T < w.lastT {
		return true
	}
	return w.now().Sub(head.arrived) >= w.hold
}

// release returns the records that may go out now, in order.
func (w *reorderWindow) release() []svcRec {
	var out []svcRec
	for w.due() {
		out = append(out, w.pop())
	}
	return out
}

func (w *reorderWindow) pop() svcRec {
	top, _ := heap.Pop(&w.h).(held)
	r := top.rec
	if r.T > w.lastT {
		w.lastT = r.T
	}
	return r
}

// wait is how long until the head is due, and false when nothing is held.
func (w *reorderWindow) wait() (time.Duration, bool) {
	if len(w.h) == 0 {
		return 0, false
	}
	if w.due() {
		return 0, true
	}
	return max(0, w.hold-w.now().Sub(w.h[0].arrived)), true
}

// drain returns everything held, in order, due or not.
func (w *reorderWindow) drain() []svcRec {
	var out []svcRec
	for len(w.h) > 0 {
		out = append(out, w.pop())
	}
	return out
}

// taskMeta is what a stream knows of its service's tasks: where each runs and in what state.
// It is read again when it is older than taskMetaTTL, and when a record names a task it does
// not hold (a replacement the swarm has just made), with the limits above for those. A read
// that fails leaves what was known.
type taskMeta struct {
	load func(context.Context) ([]taskInfo, error)
	now  func() time.Time
	// ttl is how long what was read is believed; zero means taskMetaTTL.
	ttl time.Duration

	mu        sync.Mutex
	byID      map[string]taskInfo
	list      []taskInfo
	attempted time.Time
	// lastOK says the last read succeeded.
	lastOK bool
	// asked is when a read was last made for an unknown task ID.
	asked map[string]time.Time
}

func newTaskMeta(load func(context.Context) ([]taskInfo, error), now func() time.Time) *taskMeta {
	return &taskMeta{load: load, now: now}
}

// refresh reads the tasks; the caller holds m.mu.
func (m *taskMeta) refresh(ctx context.Context) {
	m.attempted = m.now()
	tasks, err := m.load(ctx)
	m.lastOK = err == nil
	if err != nil {
		return
	}
	m.list = tasks
	m.byID = make(map[string]taskInfo, len(tasks))
	for _, ti := range tasks {
		m.byID[ti.ID] = ti
	}
}

func (m *taskMeta) stale() bool {
	return m.attempted.IsZero() || m.now().Sub(m.attempted) >= cmp.Or(m.ttl, taskMetaTTL)
}

// lookup finds a task, reading the tasks first when they are stale or the task is unknown.
func (m *taskMeta) lookup(ctx context.Context, id string) (taskInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ti, known := m.byID[id]
	switch {
	case m.stale():
		m.refresh(ctx)
	case !known && m.now().Sub(m.attempted) >= taskMetaMinGap && m.now().Sub(m.asked[id]) >= taskMetaAskGap:
		if m.asked == nil || len(m.asked) >= maxAsked {
			m.asked = map[string]time.Time{}
		}
		m.asked[id] = m.now()
		m.refresh(ctx)
	default:
		return ti, known
	}
	ti, known = m.byID[id]
	return ti, known
}

// all returns every task held, reading them first when they are stale.
func (m *taskMeta) all(ctx context.Context) []taskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stale() {
		m.refresh(ctx)
	}
	return slices.Clone(m.list)
}

// fresh reads the tasks now, whatever is held, and returns them; false when the read failed,
// in which case what was known stays.
func (m *taskMeta) fresh(ctx context.Context) ([]taskInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh(ctx)
	return slices.Clone(m.list), m.lastOK
}

// byIDMap returns the tasks held by ID, without reading anything.
func (m *taskMeta) byIDMap() map[string]taskInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.byID)
}

// maxDownloadBytes is the most one merged download holds while it is merged (plan 06-31, D-35,
// T-06-91): past it the oldest lines are left out.
const maxDownloadBytes = 32 << 20

// taskLines are the lines of one task of a service in the order the engine gave them (time
// order), and the task's slot.
type taskLines struct {
	Slot  int
	Lines []svcRec
}

// mergeCursor is a place in one of the lists being merged.
type mergeCursor struct {
	list, pos int
	slot      int
	t         string
}

type mergeHeap []mergeCursor

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	return cmp.Or(cmp.Compare(h[i].t, h[j].t), cmp.Compare(h[i].slot, h[j].slot), cmp.Compare(h[i].list, h[j].list)) < 0
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any) {
	if c, ok := x.(mergeCursor); ok {
		*h = append(*h, c)
	}
}
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// kWayMerge yields the lines of lists, each already in time order, as one list in time order;
// lines with the same stamp go lower slot first, then in the order of lists. It holds one place
// in each list and nothing else, so a list of any length costs it no memory.
func kWayMerge(lists []taskLines) iter.Seq[svcRec] {
	return func(yield func(svcRec) bool) {
		h := make(mergeHeap, 0, len(lists))
		for i, l := range lists {
			if len(l.Lines) > 0 {
				h = append(h, mergeCursor{list: i, slot: l.Slot, t: l.Lines[0].T})
			}
		}
		heap.Init(&h)
		for len(h) > 0 {
			c := h[0]
			if !yield(lists[c.list].Lines[c.pos]) {
				return
			}
			if c.pos+1 < len(lists[c.list].Lines) {
				h[0].pos++
				h[0].t = lists[c.list].Lines[c.pos+1].T
				heap.Fix(&h, 0)
			} else {
				heap.Pop(&h)
			}
		}
	}
}

// recSize is what a record counts as in a holder or a page: its stamp and its text.
func recSize(r svcRec) int { return len(r.T) + len(r.M) }

// heldTask is the lines held of one task; off is how many of the oldest were dropped.
type heldTask struct {
	slot  int
	lines []svcRec
	off   int
}

// lineHolder holds a range of a service's log by task while it is merged, up to a size; past
// it the oldest line of any task is dropped (T-06-91). The engine gives each task's lines in
// time order, so a task's oldest line is its first.
type lineHolder struct {
	limit, size int
	byTask      map[string]*heldTask
	order       []string
	dropped     bool
}

func newLineHolder(limit int) *lineHolder {
	return &lineHolder{limit: limit, byTask: map[string]*heldTask{}}
}

// add holds a record of a task in the given slot, dropping the oldest lines held while the
// holder is over its size (which can be the record itself).
func (h *lineHolder) add(r svcRec, slot int) {
	ht, ok := h.byTask[r.TaskID]
	if !ok {
		ht = &heldTask{slot: slot}
		h.byTask[r.TaskID] = ht
		h.order = append(h.order, r.TaskID)
	}
	ht.lines = append(ht.lines, r)
	h.size += recSize(r)
	for h.size > h.limit && h.dropOldest() {
	}
}

// dropOldest drops the oldest line of all the tasks, and says whether there was one.
func (h *lineHolder) dropOldest() bool {
	var best *heldTask
	for _, id := range h.order {
		ht := h.byTask[id]
		if ht.off >= len(ht.lines) {
			continue
		}
		if best == nil {
			best = ht
			continue
		}
		a, b := ht.lines[ht.off], best.lines[best.off]
		if cmp.Or(cmp.Compare(a.T, b.T), cmp.Compare(ht.slot, best.slot)) < 0 {
			best = ht
		}
	}
	if best == nil {
		return false
	}
	h.size -= recSize(best.lines[best.off])
	best.lines[best.off] = svcRec{}
	best.off++
	// Give the dropped front back once it is most of the slice, so a task that goes on
	// being trimmed does not keep what it dropped.
	if best.off >= 256 && best.off*2 >= len(best.lines) {
		best.lines = slices.Clone(best.lines[best.off:])
		best.off = 0
	}
	h.dropped = true
	return true
}

// lists returns what is held, by task, each in time order.
func (h *lineHolder) lists() []taskLines {
	out := make([]taskLines, 0, len(h.order))
	for _, id := range h.order {
		ht := h.byTask[id]
		lines := ht.lines[ht.off:]
		if len(lines) == 0 {
			continue
		}
		if !slices.IsSortedFunc(lines, func(a, b svcRec) int { return cmp.Compare(a.T, b.T) }) {
			slices.SortStableFunc(lines, func(a, b svcRec) int { return cmp.Compare(a.T, b.T) })
		}
		out = append(out, taskLines{Slot: ht.slot, Lines: lines})
	}
	return out
}

// cut says lines were dropped for size.
func (h *lineHolder) cut() bool { return h.dropped }
