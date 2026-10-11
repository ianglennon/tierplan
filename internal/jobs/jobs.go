// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package jobs is a small in-memory hub for long-running work that a browser watches
// but does not own: an image pull on every node of an environment today, a stack deploy
// in Phase 7. A job is a set of items (one per node, or per step) whose progress a
// Reporter publishes; a subscriber reads a snapshot and then the events that follow,
// and may resume from the last event ID it saw.
//
// The hub is generic on purpose. It imports nothing from Docker, and Kind and Meta are
// free-form, so a later kind of job needs no change here (Phase 5, D-26).
//
// A job runs on a context the hub owns, not on the request that started it: closing
// the browser or the stream never stops it. State is in memory only. A finished job is
// kept for Options.Retention so a returning browser can read how it ended, a server
// restart ends every running job, and the audit log is the durable record.
//
// The hub stores exactly what a caller puts in Spec and Item. A caller must never put
// a credential, a registry header or a response body in Meta, Message or Result: every
// byte of a job is sent to the browsers that may see it.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The states of an item.
const (
	ItemWaiting   = "waiting"
	ItemRunning   = "running"
	ItemDone      = "done"
	ItemFailed    = "failed"
	ItemCancelled = "cancelled"
)

// The states of a job. It is running until every item has ended, then done when every
// item is done, partial when some are and some are not, failed when none is and one
// failed, and cancelled when none is done and none failed.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobPartial   = "partial"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// The types of an Event.
const (
	// EventItem carries the new state of one item.
	EventItem = "item"
	// EventDone carries the finished job, and is the last event of a job.
	EventDone = "done"
)

// CodeInterrupted is the code of an item the run ended without finishing.
const CodeInterrupted = "interrupted"

// Errors returned by the hub.
var (
	// ErrTooMany means the owner already has the most jobs running at once.
	ErrTooMany = errors.New("jobs: too many running jobs")
	// ErrNotFound means there is no job of that ID: it never existed, or it finished
	// longer ago than the hub keeps it.
	ErrNotFound = errors.New("jobs: no such job")
)

// ItemSpec names one item of a job.
type ItemSpec struct {
	ID, Label string
}

// Spec is a job to start.
type Spec struct {
	// Kind says what the job is: "pull" today.
	Kind string
	// EnvID is the environment the job works on, empty for none.
	EnvID string
	// Owner is who started the job, as an audit actor ("user:<id>"); only the owner and
	// an instance admin may read it.
	Owner string
	// Meta is free-form. Never put a credential or a header in it.
	Meta  map[string]string
	Items []ItemSpec
}

// Sub is one part of an item, a layer of an image for a pull.
type Sub struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Current int64  `json:"current"`
	Total   int64  `json:"total"`
}

// Item is one unit of a job and how far it has got.
type Item struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
	// Code is a stable word for how a failed item failed, empty otherwise.
	Code string `json:"code"`
	// Message is a sentence built from Code, never the raw answer of a remote system.
	Message string `json:"message"`
	// Current and Total count Unit ("bytes") across the item; Total is 0 until known.
	Current int64  `json:"current"`
	Total   int64  `json:"total"`
	Unit    string `json:"unit"`
	// Done of Count parts (layers) have finished.
	Done  int   `json:"done"`
	Count int   `json:"count"`
	Subs  []Sub `json:"subs"`
	// Result is what a finished item produced: a digest, for a pull.
	Result map[string]string `json:"result"`
}

// Job is a job as a reader sees it. A Job returned by the hub is a copy: changing it
// changes nothing in the hub.
type Job struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	EnvID      string            `json:"envId"`
	Owner      string            `json:"owner"`
	Meta       map[string]string `json:"meta"`
	State      string            `json:"state"`
	CreatedAt  time.Time         `json:"createdAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	Items      []Item            `json:"items"`
}

// Finished reports whether the job has ended.
func (j Job) Finished() bool { return j.State != JobRunning }

// Event is one change to a job. ID strictly increases within a job. An item event
// carries Item; a done event carries Job.
type Event struct {
	ID    uint64
	JobID string
	Type  string
	Item  *Item
	Job   *Job
}

// Options tune the hub. A zero field takes its default.
type Options struct {
	// Retention is how long a finished job is kept. Default 10 minutes.
	Retention time.Duration
	// MaxRetained is the most jobs kept, running and finished; the oldest finished is
	// evicted first. Default 200.
	MaxRetained int
	// MaxRunningPerUser is the most jobs one owner may have running. Default 5.
	MaxRunningPerUser int
	// Coalesce is the shortest gap between two events of one item that Reporter.Update
	// publishes; the latest state is always published once the gap has passed.
	// Default 250 ms.
	Coalesce time.Duration
	// Ring is how many events a job keeps for a subscriber that resumes. Default 1024.
	Ring int
	// Now is the clock for creation and retention. Default time.Now.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Retention <= 0 {
		o.Retention = 10 * time.Minute
	}
	if o.MaxRetained <= 0 {
		o.MaxRetained = 200
	}
	if o.MaxRunningPerUser <= 0 {
		o.MaxRunningPerUser = 5
	}
	if o.Coalesce <= 0 {
		o.Coalesce = 250 * time.Millisecond
	}
	if o.Ring <= 0 {
		o.Ring = 1024
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Hub holds the jobs.
type Hub struct {
	opt    Options
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	jobs   map[string]*entry
	closed bool
}

// New builds a hub.
func New(o Options) *Hub {
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{opt: o.withDefaults(), ctx: ctx, cancel: cancel, jobs: map[string]*entry{}}
}

// entry is a job with its event ring and subscribers. Every field is guarded by Hub.mu.
type entry struct {
	job    Job
	cancel context.CancelFunc
	nextID uint64
	ring   []Event
	subs   map[*sub]struct{}
	// last is when each item last published an event; pending are the items whose latest
	// state is not published yet, and timers the flush scheduled for them.
	last    map[string]time.Time
	pending map[string]bool
	timers  map[string]*time.Timer
}

// sub is one subscriber. Its channel is closed when the job ends, when the subscriber
// cancels, or when it falls so far behind that its buffer fills.
type sub struct {
	ch     chan Event
	closed bool
}

// Start registers the job and runs run on a goroutine of its own, with a context the hub
// cancels on Cancel and Close. The job is returned as it stands before run starts. It is
// ErrTooMany when the owner already has MaxRunningPerUser jobs running.
//
// run reports through r. When it returns, any item still waiting or running is ended
// as cancelled (the context was cancelled) or failed with CodeInterrupted, so a job
// always finishes.
func (h *Hub) Start(spec Spec, run func(ctx context.Context, r *Reporter)) (Job, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Job{}, fmt.Errorf("new job id: %w", err)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return Job{}, errors.New("jobs: the hub is closed")
	}
	now := h.opt.Now()
	h.sweepLocked(now)
	running := 0
	for _, e := range h.jobs {
		if e.job.Owner == spec.Owner && !e.job.Finished() {
			running++
		}
	}
	if running >= h.opt.MaxRunningPerUser {
		h.mu.Unlock()
		return Job{}, ErrTooMany
	}
	for len(h.jobs) >= h.opt.MaxRetained && h.evictOldestFinishedLocked() {
	}
	ctx, cancel := context.WithCancel(h.ctx)
	e := &entry{
		job: Job{
			ID: id.String(), Kind: spec.Kind, EnvID: spec.EnvID, Owner: spec.Owner,
			Meta: maps.Clone(spec.Meta), State: JobRunning, CreatedAt: now,
		},
		cancel:  cancel,
		subs:    map[*sub]struct{}{},
		last:    map[string]time.Time{},
		pending: map[string]bool{},
		timers:  map[string]*time.Timer{},
	}
	if e.job.Meta == nil {
		e.job.Meta = map[string]string{}
	}
	for _, it := range spec.Items {
		e.job.Items = append(e.job.Items, Item{ID: it.ID, Label: it.Label, State: ItemWaiting, Subs: []Sub{}, Result: map[string]string{}})
	}
	if len(e.job.Items) == 0 {
		e.job.Items = []Item{}
	}
	h.jobs[e.job.ID] = e
	snap := copyJob(e.job)
	h.wg.Add(1)
	h.mu.Unlock()

	r := &Reporter{h: h, e: e}
	go func() {
		defer h.wg.Done()
		defer cancel()
		run(ctx, r)
		h.settle(e, ctx.Err() != nil)
	}()
	return snap, nil
}

// Reporter publishes the progress of one job's items. It is safe to use from several
// goroutines.
type Reporter struct {
	h *Hub
	e *entry
}

// JobID is the ID of the job the reporter reports on, known to the run as soon as it
// starts.
func (r *Reporter) JobID() string { return r.e.job.ID }

// Update changes one item under the hub's lock and publishes it, at most once per
// Options.Coalesce: updates in between are folded into the one published when the gap
// has passed. fn must be quick and must not call the hub. An item that is already
// finished is not changed.
func (r *Reporter) Update(itemID string, fn func(*Item)) {
	h, e := r.h, r.e
	h.mu.Lock()
	defer h.mu.Unlock()
	it := e.item(itemID)
	if it == nil || terminal(it.State) {
		return
	}
	fn(it)
	if terminal(it.State) {
		// An update that ends the item is a finish; it is never delayed.
		h.publishItemLocked(e, itemID)
		h.finishIfAllLocked(e)
		return
	}
	now := h.opt.Now()
	if last, ok := e.last[itemID]; !ok || now.Sub(last) >= h.opt.Coalesce {
		h.publishItemLocked(e, itemID)
		return
	}
	if e.pending[itemID] {
		return
	}
	e.pending[itemID] = true
	wait := h.opt.Coalesce - now.Sub(e.last[itemID])
	e.timers[itemID] = time.AfterFunc(wait, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if e.pending[itemID] {
			h.publishItemLocked(e, itemID)
		}
	})
}

// Finish ends an item in a final state (done, failed or cancelled) with a code, a
// message and a result, and publishes it at once. The job finishes when its last item
// does. Finishing an item that already ended changes nothing.
func (r *Reporter) Finish(itemID, state, code, message string, result map[string]string) {
	h, e := r.h, r.e
	h.mu.Lock()
	defer h.mu.Unlock()
	it := e.item(itemID)
	if it == nil || terminal(it.State) {
		return
	}
	it.State, it.Code, it.Message = state, code, message
	if result != nil {
		it.Result = maps.Clone(result)
	}
	h.publishItemLocked(e, itemID)
	h.finishIfAllLocked(e)
}

func terminal(state string) bool {
	return state == ItemDone || state == ItemFailed || state == ItemCancelled
}

func (e *entry) item(id string) *Item {
	for i := range e.job.Items {
		if e.job.Items[i].ID == id {
			return &e.job.Items[i]
		}
	}
	return nil
}

// publishItemLocked publishes the current state of one item and clears its pending flush.
func (h *Hub) publishItemLocked(e *entry, itemID string) {
	if t := e.timers[itemID]; t != nil {
		t.Stop()
		delete(e.timers, itemID)
	}
	delete(e.pending, itemID)
	e.last[itemID] = h.opt.Now()
	it := e.item(itemID)
	if it == nil {
		return
	}
	cp := copyItem(*it)
	h.emitLocked(e, Event{Type: EventItem, Item: &cp})
}

// emitLocked numbers an event, keeps it in the ring and sends it to every subscriber. A
// subscriber whose buffer is full is dropped: it reconnects and is sent a snapshot.
func (h *Hub) emitLocked(e *entry, ev Event) {
	e.nextID++
	ev.ID, ev.JobID = e.nextID, e.job.ID
	e.ring = append(e.ring, ev)
	if over := len(e.ring) - h.opt.Ring; over > 0 {
		e.ring = slices.Delete(e.ring, 0, over)
	}
	for s := range e.subs {
		select {
		case s.ch <- ev:
		default:
			closeSub(e, s)
		}
	}
}

func closeSub(e *entry, s *sub) {
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	delete(e.subs, s)
}

// finishIfAllLocked ends the job when every item has ended.
func (h *Hub) finishIfAllLocked(e *entry) {
	if e.job.Finished() {
		return
	}
	var done, failed int
	for _, it := range e.job.Items {
		switch it.State {
		case ItemDone:
			done++
		case ItemFailed:
			failed++
		case ItemCancelled:
		default:
			return
		}
	}
	switch {
	case done == len(e.job.Items):
		e.job.State = JobDone
	case done > 0:
		e.job.State = JobPartial
	case failed > 0:
		e.job.State = JobFailed
	default:
		e.job.State = JobCancelled
	}
	e.job.FinishedAt = h.opt.Now()
	for id, t := range e.timers {
		t.Stop()
		delete(e.timers, id)
	}
	clear(e.pending)
	cp := copyJob(e.job)
	h.emitLocked(e, Event{Type: EventDone, Job: &cp})
	for s := range e.subs {
		closeSub(e, s)
	}
}

// settle ends the items the run left unfinished and so finishes the job.
func (h *Hub) settle(e *entry, cancelled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range e.job.Items {
		it := &e.job.Items[i]
		if terminal(it.State) {
			continue
		}
		if cancelled {
			it.State = ItemCancelled
		} else {
			it.State, it.Code, it.Message = ItemFailed, CodeInterrupted, "The job ended before this step finished."
		}
		h.publishItemLocked(e, it.ID)
	}
	if len(e.job.Items) == 0 && !e.job.Finished() {
		e.job.State = JobDone
		e.job.FinishedAt = h.opt.Now()
		cp := copyJob(e.job)
		h.emitLocked(e, Event{Type: EventDone, Job: &cp})
		for s := range e.subs {
			closeSub(e, s)
		}
		return
	}
	h.finishIfAllLocked(e)
}

// sweepLocked forgets every finished job older than the retention.
func (h *Hub) sweepLocked(now time.Time) {
	for id, e := range h.jobs {
		if e.job.Finished() && now.Sub(e.job.FinishedAt) >= h.opt.Retention {
			delete(h.jobs, id)
		}
	}
}

// evictOldestFinishedLocked forgets the finished job that finished first. It reports
// false when no job has finished.
func (h *Hub) evictOldestFinishedLocked() bool {
	var oldest *entry
	for _, e := range h.jobs {
		if e.job.Finished() && (oldest == nil || e.job.FinishedAt.Before(oldest.job.FinishedAt)) {
			oldest = e
		}
	}
	if oldest == nil {
		return false
	}
	delete(h.jobs, oldest.job.ID)
	return true
}

// Get returns a copy of the job, and false when the hub has none of that ID.
func (h *Hub) Get(id string) (Job, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked(h.opt.Now())
	e, ok := h.jobs[id]
	if !ok {
		return Job{}, false
	}
	return copyJob(e.job), true
}

// List returns the jobs keep accepts, newest first.
func (h *Hub) List(keep func(Job) bool) []Job {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked(h.opt.Now())
	var out []Job
	for _, e := range h.jobs {
		if keep == nil || keep(e.job) {
			out = append(out, copyJob(e.job))
		}
	}
	slices.SortFunc(out, func(a, b Job) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return -cmpString(a.ID, b.ID)
	})
	return out
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Subscribe returns the job as it stands, and a channel of the events that follow it.
// With after 0 the channel carries only events published from now on; with after n it
// first carries every retained event newer than n, so a reader that last saw n and
// takes the snapshot too ends in the same state. The channel is closed after the done
// event, when cancel is called, or when the reader falls too far behind to be kept up
// with; a reader that sees it closed before done reconnects. cancel is safe to call
// twice, and ctx ending cancels the subscription.
func (h *Hub) Subscribe(ctx context.Context, id string, after uint64) (Job, <-chan Event, func(), error) {
	h.mu.Lock()
	h.sweepLocked(h.opt.Now())
	e, ok := h.jobs[id]
	if !ok {
		h.mu.Unlock()
		return Job{}, nil, nil, ErrNotFound
	}
	snap := copyJob(e.job)
	var replay []Event
	if after > 0 {
		for _, ev := range e.ring {
			if ev.ID > after {
				replay = append(replay, ev)
			}
		}
	}
	s := &sub{ch: make(chan Event, len(replay)+h.opt.Ring)}
	for _, ev := range replay {
		s.ch <- ev
	}
	if e.job.Finished() {
		s.closed = true
		close(s.ch)
	} else {
		e.subs[s] = struct{}{}
	}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := e.subs[s]; ok {
			closeSub(e, s)
		}
	}
	if ctx != nil {
		stop := context.AfterFunc(ctx, cancel)
		inner := cancel
		cancel = func() { stop(); inner() }
	}
	return snap, s.ch, cancel, nil
}

// Cancel cancels the context of a running job. Its run decides how fast it stops; the
// items it leaves unfinished end as cancelled. Cancelling a finished job does nothing.
func (h *Hub) Cancel(id string) error {
	h.mu.Lock()
	e, ok := h.jobs[id]
	h.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	e.cancel()
	return nil
}

// Close cancels every running job, waits for their runs to return and closes every
// subscription. The hub starts nothing afterwards.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.jobs {
		for s := range e.subs {
			closeSub(e, s)
		}
	}
}

func copyItem(it Item) Item {
	it.Subs = slices.Clone(it.Subs)
	if it.Subs == nil {
		it.Subs = []Sub{}
	}
	it.Result = maps.Clone(it.Result)
	if it.Result == nil {
		it.Result = map[string]string{}
	}
	return it
}

func copyJob(j Job) Job {
	j.Meta = maps.Clone(j.Meta)
	items := make([]Item, len(j.Items))
	for i, it := range j.Items {
		items[i] = copyItem(it)
	}
	j.Items = items
	return j
}
