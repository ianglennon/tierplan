// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// JobKind is the kind of the job a janitor run is in the jobs hub.
const JobKind = "janitor_run"

// What started a run.
const (
	// TriggerSchedule is a run the environment's schedule started.
	TriggerSchedule = "schedule"
	// TriggerManual is a run a person started with Clean now.
	TriggerManual = "manual"
)

// Runner runs the janitor on an environment: it takes the environment's operation lock, plans
// again from a fresh read and removes the items that are removable now, each through the agent of
// its node, as a job in the jobs hub.
type Runner struct {
	Engine Engine
	// Locks is the lock the stack operations take too, so a clean-up never runs under a deploy.
	Locks stackops.OperationLock
	Jobs  *jobs.Hub
	// Clock tells the time a run judges ages at. Nil means the wall clock.
	Clock stackops.Clock
	// Policy gives the policy of an environment. Nil means DefaultPolicy.
	Policy func(ctx context.Context, envID string) (Policy, error)
	// Store keeps the record of the runs. It is required: a run is on record before it removes
	// anything, so a Runner with no Store starts nothing.
	Store RunStore
	// Networks judges and removes networks, after the run's containers are gone. Nil means the run
	// removes no network.
	Networks Networks
	// Audit receives the one summary row of each finished run. Nil writes none.
	Audit AuditSink
	// Log receives what a run could not tell its job. Nil means slog.Default().
	Log *slog.Logger
}

// AuditSink is where a finished run's summary row goes: the audit log's writer.
type AuditSink interface {
	Record(ctx context.Context, e audit.Entry) error
}

// SystemActor is who a scheduled run is in the audit log.
const SystemActor = "system:janitor"

// ItemRef names an item of a plan by its type, the node it is on and the engine's ID of it. A
// reference only selects: whether the item is removed is decided again by the run, so a
// reference never says what class or reason an item has.
type ItemRef struct {
	Type        Kind
	NodeID, Ref string
}

// key is how an item is matched against the references: its type, node and ID.
func (r ItemRef) key() string { return string(r.Type) + "|" + r.NodeID + "|" + r.Ref }

// RunRequest is a run to start.
type RunRequest struct {
	EnvID, EnvName string
	// Trigger is TriggerSchedule or TriggerManual.
	Trigger string
	// Actor names who started the run, as the lock shows it.
	Actor string
	// ActorID is who the record names as having started it: the user's ID. Empty means the record
	// names Actor, as it does for a scheduled run.
	ActorID string
	// Owner owns the job: the person for Clean now, system:janitor:{envID} for a scheduled run,
	// so the cap of running jobs of one owner never blocks the instance.
	Owner string
	// Principal is who the run removes networks as, which is whose name the network audit rows
	// carry: the user for Clean now, rbac.System(SystemActor) for the schedule. Nil means the system.
	Principal *rbac.Principal
	// Ticked are the items a person ticked in the preview. Only those that are removable on the
	// fresh plan are removed.
	Ticked []ItemRef
	// All removes everything that is removable on the fresh plan, for a scheduled run.
	All bool
}

// Started is the answer of Start: the run and the job it is in.
type Started struct {
	RunID, JobID string
}

// LockedError means the environment's operation lock is held, and by whom. Nothing was started.
type LockedError struct {
	Holder stackops.Holder
}

// Error implements error.
func (e *LockedError) Error() string {
	return "janitor: the environment's operation lock is held by " + e.Holder.Kind
}

// Start takes the environment's operation lock and starts a run as a job. It answers a
// *LockedError at once when the lock is held, and starts nothing. The job holds the lock only
// while it removes, and has freed it before the job reads finished. jobs.ErrTooMany is passed on
// when the owner has the most jobs running.
func (r *Runner) Start(ctx context.Context, req RunRequest) (Started, error) {
	if r.Store == nil {
		return Started{}, errors.New("janitor: the runner has no store to record a run in")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Started{}, fmt.Errorf("new run id: %w", err)
	}
	runID := id.String()
	release, held, ok := r.Locks.TryAcquire(req.EnvID, stackops.Holder{
		Kind: stackops.HolderKindJanitor, By: req.Actor, RunID: runID, Started: r.clock().Now().UTC(),
	})
	if !ok {
		return Started{}, &LockedError{Holder: held}
	}
	// The run is on record before anything can be removed (T-11-12). A run that cannot be
	// recorded is not started.
	actor := req.ActorID
	if actor == "" {
		actor = req.Actor
	}
	started := r.clock().Now().UTC()
	if err := r.Store.CreateRun(ctx, Run{
		ID: runID, EnvID: req.EnvID, Trigger: req.Trigger, Actor: actor, State: StateRunning, StartedAt: started, EstBytes: -1,
	}); err != nil {
		release()
		return Started{}, err
	}
	job, err := r.Jobs.Start(jobs.Spec{
		Kind: JobKind, EnvID: req.EnvID, Owner: req.Owner,
		Meta: map[string]string{"runId": runID, "trigger": req.Trigger},
	}, func(ctx context.Context, rep *jobs.Reporter) {
		r.execute(ctx, req, runID, release, rep)
	})
	if err != nil {
		// The job never started, so the run never worked: it is on record as failed.
		r.endRecord(ctx, req, Run{ID: runID, State: StateFailed, Reason: ReasonRunFailed, EstBytes: -1})
		release()
		return Started{}, err
	}
	return Started{RunID: runID, JobID: job.ID}, nil
}

// endRecord finishes a run's record at the current time and writes the run's one audit row. A store
// or an audit log that fails is logged: whatever the run did has been done by then.
func (r *Runner) endRecord(ctx context.Context, req RunRequest, run Run) {
	ctx = context.WithoutCancel(ctx)
	run.FinishedAt = r.clock().Now().UTC()
	if err := r.Store.FinishRun(ctx, run); err != nil {
		r.log().Error("a janitor run could not be finished on record", "run", run.ID, "error", err)
	}
	r.auditRun(ctx, req, run)
}

// auditRun writes the summary row of a finished run (AUTH-08, D-16): who started it, what it
// counted and how it ended, and no name of any item. A scheduled run is the system's.
func (r *Runner) auditRun(ctx context.Context, req RunRequest, run Run) {
	if r.Audit == nil {
		return
	}
	actor := audit.Actor{Type: audit.ActorUser, ID: req.ActorID, Name: req.Actor}
	if actor.ID == "" {
		actor.ID = req.Actor
	}
	if req.Trigger == TriggerSchedule {
		actor = rbac.System(SystemActor).AuditActor()
	}
	result := audit.ResultSuccess
	if run.State == StatePartial || run.State == StateFailed {
		result = audit.ResultFailure
	}
	est := "unknown"
	if run.EstBytes >= 0 {
		est = strconv.FormatInt(run.EstBytes, 10)
	}
	c := run.Counts
	err := r.Audit.Record(ctx, audit.Entry{
		Actor: actor, Action: audit.ActionJanitorCleaned, TargetType: "environment", TargetID: req.EnvID, EnvID: req.EnvID,
		Result: result, Reason: run.Reason,
		Details: map[string]string{
			"run": run.ID, "trigger": req.Trigger, "outcome": run.State, "reason": run.Reason,
			"removed_containers": strconv.Itoa(c.RemovedContainers), "removed_images": strconv.Itoa(c.RemovedImages),
			"removed_networks": strconv.Itoa(c.RemovedNetworks), "kept": strconv.Itoa(c.Kept), "failed": strconv.Itoa(c.Failed),
			"skipped_offline": strconv.Itoa(c.SkippedOffline), "est_bytes": est,
		},
	})
	if err != nil {
		r.log().Error("a janitor run could not be written to the audit log", "run", run.ID, "error", err)
	}
}

func (r *Runner) clock() stackops.Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return stackops.RealClock()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// batchSize is how many items a run records in one transaction: a write never waits on a Docker
// call, and a long run is on record as it goes (Pitfall 6).
const batchSize = 50

// runState is what a run counts and records as it goes.
type runState struct {
	runID string
	// ctx is the job's context without its cancellation: a run that is cut short still records
	// what it did.
	ctx context.Context
	// counts are the selected items by how their removal ended; gone counts the ones that were
	// already gone and notNow the ticked items that were not removable on the fresh plan.
	counts Counts
	gone   int
	// offline are the nodes the run will not send anything to, by ID: the ones the fresh read could
	// not reach, and the ones that stopped answering while the run went on.
	offline       map[string]string
	freed         int64
	freedKnown    bool
	seq           int
	buf           []RunItem
	planFailed    bool
	noNode        bool
	reachedTheEnd bool
	cut           bool
	// pol is the policy the run judged by, nodeNames the names of the nodes the first read saw, by
	// ID, and netRows the networks the run may remove, by the ID of their item.
	pol       Policy
	nodeNames map[string]string
	netRows   map[string]NetworkRow
	// netsUnread is true when the networks could not be judged, which leaves the run partial.
	netsUnread bool
}

// begin is the one place a run starts. It gives the run on record its job.
func (r *Runner) begin(ctx context.Context, req RunRequest, runID, jobID string) *runState {
	r.log().Info("a janitor run started", "environment", req.EnvID, "run", runID, "trigger", req.Trigger, "actor", req.Actor)
	s := &runState{runID: runID, ctx: context.WithoutCancel(ctx)}
	if err := r.Store.SetState(s.ctx, runID, StateRunning, jobID); err != nil {
		r.log().Error("a janitor run could not be given its job on record", "run", runID, "error", err)
	}
	return s
}

// record adds an attempted item to the batch, and writes the batch when it holds a batch of items
// or when the node changes.
func (s *runState) record(r *Runner, it Item, out outcome) {
	if len(s.buf) > 0 && s.buf[0].NodeID != it.NodeID {
		s.flush(r)
	}
	s.seq++
	s.buf = append(s.buf, RunItem{
		Seq: s.seq, Type: it.Type, NodeID: it.NodeID, Node: it.Node, Ref: it.Ref, Name: it.Name,
		Reason: it.Reason, Outcome: out.recorded, Code: out.code, Size: it.Size,
	})
	if len(s.buf) >= batchSize {
		s.flush(r)
	}
}

// flush writes the items waiting to be recorded.
func (s *runState) flush(r *Runner) {
	if len(s.buf) == 0 {
		return
	}
	if err := r.Store.AddItems(s.ctx, s.runID, s.buf); err != nil {
		r.log().Error("a janitor run could not record its items", "run", s.runID, "items", len(s.buf), "error", err)
	}
	s.buf = s.buf[:0]
}

// end is the one place a run ends, deferred so that it runs however the run ends. It records the
// items still waiting and the run's final state: completed, partial when an item errored, a node's
// items were skipped or the run was cut short, skipped when no node reported, or failed when it could
// not do its work.
func (s *runState) end(r *Runner, req RunRequest) {
	s.flush(r)
	run := Run{ID: s.runID, State: StateCompleted, Counts: s.counts, EstBytes: -1}
	switch {
	case s.planFailed || !s.reachedTheEnd:
		run.State, run.Reason = StateFailed, ReasonRunFailed
	case s.noNode:
		run.State, run.Reason = StateSkipped, ReasonNoNodeReporting
	case s.counts.Failed > 0 || s.counts.SkippedOffline > 0 || s.cut || s.netsUnread:
		run.State = StatePartial
	}
	if s.freedKnown {
		run.EstBytes = s.freed
	}
	r.endRecord(s.ctx, req, run)
	r.log().Info("a janitor run ended", "environment", req.EnvID, "run", s.runID, "state", run.State,
		"removed", s.counts.RemovedContainers+s.counts.RemovedImages+s.counts.RemovedNetworks, "gone", s.gone,
		"kept", s.counts.Kept, "skipped_offline", s.counts.SkippedOffline, "failed", s.counts.Failed)
}

// planItemID is the ID of a plan item in the job: unique across types and nodes.
func planItemID(it Item) string { return string(it.Type) + "/" + it.NodeID + "/" + it.Ref }

// The codes of a job item that did not end as a removal.
const (
	codeNoLongerStale = "no_longer_stale"
	codeInUse         = "in_use"
	codeGone          = "gone"
	codeNodeOffline   = "node_offline"
	codeActionFailed  = CodeActionFailed
	codePlanFailed    = "plan_failed"
)

// outcome is how one removal ended, as the job item shows it.
type outcome struct {
	state, code, message string
	// recorded is how the run's record says it ended.
	recorded Outcome
}

// execute is the body of the job. The lock is freed once the removals are done and before the last
// item is finished: a job finishes when its last item does, so a reader that sees it finished
// finds the environment free.
func (r *Runner) execute(ctx context.Context, req RunRequest, runID string, release func(), rep *jobs.Reporter) {
	free := sync.OnceFunc(release)
	defer free()
	r.Locks.Attach(req.EnvID, rep.JobID())
	run := r.begin(ctx, req, runID, rep.JobID())
	// The record is finished before the last job item is, so a reader that sees the job finished
	// finds the run finished. The deferred call covers a run that panics.
	finish := sync.OnceFunc(func() { run.end(r, req) })
	defer finish()

	selected, unsent, err := r.plan(ctx, req, run)
	if err != nil {
		r.log().Warn("a janitor run could not plan", "environment", req.EnvID, "run", runID, "error", err)
		run.planFailed = true
		rep.Add(jobs.ItemSpec{ID: "plan", Label: "Plan the clean-up", Kind: "step"})
		finish()
		free()
		rep.Finish("plan", jobs.ItemFailed, codePlanFailed, "TierPlan could not read the environment, so nothing was removed.", nil)
		return
	}
	// What was ticked and is not removable now is on record before the first removal: it asks
	// nothing of any engine.
	for _, l := range unsent {
		run.tally(l.item, l.out)
		run.record(r, l.item, l.out)
	}
	// Every item is added before the first is finished: a job whose items have all ended has ended.
	for _, it := range selected {
		rep.Add(jobs.ItemSpec{ID: planItemID(it), Label: it.Name, Kind: string(it.Type)})
	}
	// The last item to end is finished after the lock is freed, so it is held back until the run
	// knows it has no more items: a run's stages add their items before the last one ends.
	var tail func()
	removeAll := func(items []Item) {
		for i, it := range items {
			if ctx.Err() != nil {
				break // the hub ends the items that were never reached as cancelled
			}
			id := planItemID(it)
			rep.Update(id, func(x *jobs.Item) { x.State = jobs.ItemRunning })
			out := r.remove(ctx, req, it, run)
			run.tally(it, out)
			run.record(r, it, out)
			result := map[string]string{"nodeId": it.NodeID, "node": it.Node, "ref": it.Ref}
			fin := func() { rep.Finish(id, out.state, out.code, out.message, result) }
			if i == len(items)-1 {
				tail = fin
				continue
			}
			fin()
		}
	}
	removeAll(selected)
	// Networks come after the containers, and are judged afresh then: a network is only orphaned or
	// unused once the containers that used it are gone (Pitfall 3).
	if ctx.Err() == nil && !run.noNode {
		nets, netUnsent := r.planNetworks(ctx, req, run)
		for _, l := range netUnsent {
			run.tally(l.item, l.out)
			run.record(r, l.item, l.out)
		}
		for _, it := range nets {
			rep.Add(jobs.ItemSpec{ID: planItemID(it), Label: it.Name, Kind: string(it.Type)})
		}
		if len(nets) > 0 && tail != nil {
			tail() // the networks' items exist, so the job stays open without it
			tail = nil
		}
		removeAll(nets)
	}
	run.reachedTheEnd = true
	run.cut = ctx.Err() != nil
	finish()
	free()
	if tail != nil {
		tail()
	}
}

// unsentItem is a ticked item the run did not send to an engine, and how it ended.
type unsentItem struct {
	item Item
	out  outcome
}

// plan reads the environment fresh, plans with the policy and returns the items to remove: the
// ticked ones that are removable now, or all of them for a run that was not ticked, in the plan's
// order and each once. The ticked items that are not removable now are returned as unsent, in the
// order they were ticked and each once: no longer stale, or on a node that did not answer.
func (r *Runner) plan(ctx context.Context, req RunRequest, run *runState) ([]Item, []unsentItem, error) {
	pol := DefaultPolicy()
	if r.Policy != nil {
		p, err := r.Policy(ctx, req.EnvID)
		if err != nil {
			return nil, nil, fmt.Errorf("read the policy: %w", err)
		}
		pol = p
	}
	run.pol = pol
	snap, err := r.Engine.Snapshot(ctx, req.EnvID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the environment: %w", err)
	}
	run.offline = make(map[string]string, len(snap.Unreachable))
	run.nodeNames = make(map[string]string, len(snap.Nodes)+len(snap.Unreachable))
	for _, n := range snap.Unreachable {
		run.offline[n.ID] = n.Name
		run.nodeNames[n.ID] = n.Name
	}
	for _, n := range snap.Nodes {
		run.nodeNames[n.ID] = n.Name
	}
	if len(snap.Nodes) == 0 {
		// Not one node reported, so nothing can be judged and nothing is removed.
		run.noNode = true
		return nil, nil, nil
	}
	ends := InspectCandidates(ctx, r.Engine, req.EnvID, Candidates(snap, pol))
	plan := Build(Input{Snapshot: snap, Ends: ends}, pol, r.clock().Now().UTC())
	if req.All {
		return plan.Items, nil, nil
	}
	// This stage is the containers' and then the images' (a plan lists them in that order); the
	// networks are the next stage's, judged afresh once these are gone. An image a container of this
	// run was using was used when the run judged it, so it is judged again by the next run.
	ticked := make(map[string]bool, len(req.Ticked))
	for _, t := range req.Ticked {
		if t.Type != KindNetwork {
			ticked[t.key()] = true
		}
	}
	var out []Item
	for _, it := range plan.Items {
		k := ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}.key()
		if ticked[k] {
			out = append(out, it)
			ticked[k] = false
		}
	}
	var notNow []unsentItem
	for _, t := range req.Ticked {
		if !ticked[t.key()] {
			continue
		}
		ticked[t.key()] = false
		it := absentItem(snap, plan, t)
		o := noLongerStaleOutcome(t.Type)
		if _, down := run.offline[t.NodeID]; down {
			o = offlineOutcome()
		}
		notNow = append(notNow, unsentItem{item: it, out: o})
	}
	return out, notNow, nil
}

// absentItem is the item a ticked reference stands for when the fresh plan does not remove it: the
// planner's own item when it looked at the object and left it, else what the snapshot knows of the
// object, else the reference alone.
func absentItem(snap Snapshot, plan Plan, ref ItemRef) Item {
	for _, it := range plan.NotRemoved {
		if (ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}) == ref {
			return it
		}
	}
	it := Item{Type: ref.Type, NodeID: ref.NodeID, Ref: ref.Ref, Name: ref.Ref, Size: -1}
	switch ref.Type {
	case KindContainer:
		for _, c := range snap.Containers {
			if c.ID == ref.Ref && c.NodeID == ref.NodeID {
				it.Name, it.Node = strings.TrimPrefix(c.Name, "/"), c.Node
				return it
			}
		}
	case KindImage:
		for _, img := range snap.Images {
			if img.ID == ref.Ref && img.NodeID == ref.NodeID {
				it.Name, it.Node = shortImageID(img.ID), img.Node
				return it
			}
		}
	}
	for _, n := range snap.Unreachable {
		if n.ID == ref.NodeID {
			it.Node = n.Name
		}
	}
	for _, n := range snap.Nodes {
		if n.ID == ref.NodeID {
			it.Node = n.Name
		}
	}
	return it
}

// offlineOutcome is an item whose node does not answer: nothing was sent for it.
func offlineOutcome() outcome {
	return outcome{state: jobs.ItemFailed, code: codeNodeOffline, message: "Its node is not connected, so nothing was sent.", recorded: OutcomeSkippedNodeOffline}
}

// noLongerStaleOutcome is an item that is not what the janitor removes any more: nothing was sent.
func noLongerStaleOutcome(k Kind) outcome {
	msg := "It is no longer a stale container, so it was left alone."
	switch k {
	case KindNetwork:
		msg = "It is no longer an orphaned or unused network, so it was left alone."
	case KindImage:
		msg = "It is no longer a dangling image, so it was left alone."
	}
	return outcome{state: jobs.ItemFailed, code: codeNoLongerStale, message: msg, recorded: OutcomeSkippedProtected}
}

// remove removes one item of the plan, once, and says how it ended. An item on a node the run
// already found not answering is not sent at all.
func (r *Runner) remove(ctx context.Context, req RunRequest, it Item, run *runState) outcome {
	if _, down := run.offline[it.NodeID]; down {
		return offlineOutcome()
	}
	var err error
	switch it.Type {
	case KindContainer:
		err = r.Engine.RemoveContainer(ctx, req.EnvID, it.NodeID, it.Ref)
	case KindImage:
		err = r.Engine.RemoveImage(ctx, req.EnvID, it.NodeID, it.Ref)
	case KindNetwork:
		err = r.Networks.Remove(ctx, req.EnvID, run.principal(req), run.netRows[planItemID(it)])
	default:
		err = errUnsupported
	}
	var coded *CodedError
	switch {
	case err == nil:
		return outcome{state: jobs.ItemDone, message: "Removed.", recorded: OutcomeRemoved}
	case errors.Is(err, ErrGone):
		return outcome{state: jobs.ItemDone, code: codeGone, message: "It was already gone.", recorded: OutcomeSkippedProtected}
	case errors.Is(err, ErrNoLongerStale):
		return noLongerStaleOutcome(it.Type)
	case errors.Is(err, ErrInUse):
		return outcome{state: jobs.ItemFailed, code: codeInUse, message: "The engine says it is in use, so it was left alone.", recorded: OutcomeRefusedInUse}
	case errors.Is(err, ErrNodeOffline):
		// The rest of this node's items are not sent: the node does not answer.
		run.offline[it.NodeID] = it.Node
		return offlineOutcome()
	}
	code := CodeActionFailed
	if errors.As(err, &coded) && coded.Code != "" {
		code = coded.Code
	}
	r.log().Warn("a janitor run could not remove an item", "environment", req.EnvID, "type", it.Type, "node", it.NodeID, "ref", it.Ref, "code", code, "error", err)
	msg := "It could not be removed."
	if code == CodeInspectTooLarge {
		msg = "Its details are over 64 KB, too large to save in the audit log first, so it was not removed."
	}
	return outcome{state: jobs.ItemFailed, code: code, message: msg, recorded: OutcomeError}
}

// principal is who the run's network removals are recorded as.
func (s *runState) principal(req RunRequest) *rbac.Principal {
	if req.Principal != nil {
		return req.Principal
	}
	return rbac.System(SystemActor)
}

// planNetworks is the run's second stage: it judges the networks afresh, now that the run's
// containers are gone, and returns the networks to remove, the ticked ones that are removable now
// or all of them for a run that was not ticked, in the plan's order and each once. A ticked network
// that is not removable now is returned as unsent, and nothing is sent for it. The networks are not
// read at all by a run that ticked none and was not asked for everything.
func (r *Runner) planNetworks(ctx context.Context, req RunRequest, run *runState) ([]Item, []unsentItem) {
	if r.Networks == nil {
		return nil, nil
	}
	var tickedNets []ItemRef
	seen := map[string]bool{}
	for _, t := range req.Ticked {
		if t.Type == KindNetwork && !seen[t.key()] {
			seen[t.key()] = true
			tickedNets = append(tickedNets, t)
		}
	}
	if !req.All && len(tickedNets) == 0 {
		return nil, nil
	}
	report, err := r.Networks.Report(ctx, req.EnvID)
	if err != nil {
		// Nothing can be said of any network, so none is removed and the run says it was cut short.
		r.log().Warn("a janitor run could not judge the networks", "environment", req.EnvID, "run", run.runID, "error", err)
		run.netsUnread = true
		var unsent []unsentItem
		for _, t := range tickedNets {
			unsent = append(unsent, unsentItem{
				item: networkRef(t, run.nodeNames[t.NodeID]),
				out:  outcome{state: jobs.ItemFailed, code: codePlanFailed, message: "TierPlan could not read the networks, so none was removed.", recorded: OutcomeError},
			})
		}
		return nil, unsent
	}
	var plan Plan
	addNetworks(&plan, report, run.pol, r.clock().Now().UTC())
	sortItems(plan.Items)
	sortItems(plan.NotRemoved)

	run.netRows = make(map[string]NetworkRow, len(report.Rows))
	byRef := make(map[string]NetworkRow, len(report.Rows))
	for _, row := range report.Rows {
		byRef[ItemRef{Type: KindNetwork, NodeID: row.NodeID, Ref: row.ID}.key()] = row
	}
	withheld := make(map[string]bool, len(report.WithheldNodes))
	for _, n := range report.WithheldNodes {
		withheld[n] = true
	}

	var selected []Item
	if req.All {
		selected = plan.Items
	} else {
		for _, t := range tickedNets {
			for _, it := range plan.Items {
				if (ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}) == t {
					selected = append(selected, it)
					seen[t.key()] = false
				}
			}
		}
		sortItems(selected)
	}
	for _, it := range selected {
		run.netRows[planItemID(it)] = byRef[ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}.key()]
	}

	var unsent []unsentItem
	for _, t := range tickedNets {
		if !seen[t.key()] {
			continue
		}
		item := networkRef(t, run.nodeNames[t.NodeID])
		for _, it := range plan.NotRemoved {
			if (ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}) == t {
				item = it
			}
		}
		if row, ok := byRef[t.key()]; ok {
			item.Name, item.Node = row.Name, row.Node
		}
		out := noLongerStaleOutcome(KindNetwork)
		_, down := run.offline[t.NodeID]
		switch {
		case down, withheld[run.nodeNames[t.NodeID]] && t.NodeID != "":
			out = offlineOutcome()
		case item.Reason.Code == ReasonNetworksWithheld:
			// A node does not answer, so whether anything uses this network is not known.
			out = outcome{state: jobs.ItemFailed, code: codeNodeOffline, message: "A node is not connected, so TierPlan could not tell whether the network is in use. Nothing was sent.", recorded: OutcomeSkippedNodeOffline}
		}
		unsent = append(unsent, unsentItem{item: item, out: out})
	}
	return selected, unsent
}

// networkRef is the item a reference to a network stands for when nothing else is known of it.
func networkRef(ref ItemRef, node string) Item {
	return Item{Type: KindNetwork, NodeID: ref.NodeID, Node: node, Ref: ref.Ref, Name: ref.Ref, Size: -1}
}

// tally counts how an item ended.
func (s *runState) tally(it Item, out outcome) {
	switch out.recorded {
	case OutcomeRemoved:
		s.countRemoved(it)
	case OutcomeRefusedInUse:
		s.counts.Kept++
	case OutcomeSkippedProtected:
		if out.code == codeGone {
			s.gone++ // neither a removal of this run's nor a failure
		} else {
			s.counts.Kept++
		}
	case OutcomeSkippedNodeOffline:
		s.counts.SkippedOffline++
	case OutcomeError:
		s.counts.Failed++
	}
}

// countRemoved counts an item the engine removed, and the bytes it freed when they are known.
func (s *runState) countRemoved(it Item) {
	switch it.Type {
	case KindContainer:
		s.counts.RemovedContainers++
	case KindImage:
		s.counts.RemovedImages++
	case KindNetwork:
		s.counts.RemovedNetworks++
	}
	if it.Size >= 0 {
		s.freed += it.Size
		s.freedKnown = true
	}
}

// errUnsupported is the removal of a kind of object the janitor does not remove.
var errUnsupported = errors.New("janitor: removing this kind of object is not supported")
