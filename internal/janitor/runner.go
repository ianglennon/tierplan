// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/jobs"
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
	// Log receives what a run could not tell its job. Nil means slog.Default().
	Log *slog.Logger
}

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
	// Owner owns the job: the person for Clean now, system:janitor:{envID} for a scheduled run,
	// so the cap of running jobs of one owner never blocks the instance.
	Owner string
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
func (r *Runner) Start(_ context.Context, req RunRequest) (Started, error) {
	release, held, ok := r.Locks.TryAcquire(req.EnvID, stackops.Holder{
		Kind: stackops.HolderKindJanitor, By: req.Actor, Started: r.clock().Now().UTC(),
	})
	if !ok {
		return Started{}, &LockedError{Holder: held}
	}
	id, err := uuid.NewV7()
	if err != nil {
		release()
		return Started{}, fmt.Errorf("new run id: %w", err)
	}
	runID := id.String()
	job, err := r.Jobs.Start(jobs.Spec{
		Kind: JobKind, EnvID: req.EnvID, Owner: req.Owner,
		Meta: map[string]string{"runId": runID, "trigger": req.Trigger},
	}, func(ctx context.Context, rep *jobs.Reporter) {
		r.execute(ctx, req, runID, release, rep)
	})
	if err != nil {
		release()
		return Started{}, err
	}
	return Started{RunID: runID, JobID: job.ID}, nil
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

// runState is what a run counts as it goes, for the record plan 11-06 keeps of it.
type runState struct {
	runID string
	// removed, gone, refused and failed count the selected items by how their removal ended;
	// notNow counts the ticked items that were not removable on the fresh plan.
	removed, gone, refused, failed, notNow int
}

// begin is the one place a run starts.
func (r *Runner) begin(req RunRequest, runID string) *runState {
	r.log().Info("a janitor run started", "environment", req.EnvID, "run", runID, "trigger", req.Trigger, "actor", req.Actor)
	return &runState{runID: runID}
}

// end is the one place a run ends, deferred so that it runs however the run ends.
func (s *runState) end(r *Runner, req RunRequest) {
	r.log().Info("a janitor run ended", "environment", req.EnvID, "run", s.runID,
		"removed", s.removed, "gone", s.gone, "refused", s.refused, "failed", s.failed, "not_removable_now", s.notNow)
}

// planItemID is the ID of a plan item in the job: unique across types and nodes.
func planItemID(it Item) string { return string(it.Type) + "/" + it.NodeID + "/" + it.Ref }

// The codes of a job item that did not end as a removal.
const (
	codeNoLongerStale = "no_longer_stale"
	codeInUse         = "in_use"
	codeGone          = "already_gone"
	codeNodeOffline   = "node_offline"
	codeFailed        = "remove_failed"
	codePlanFailed    = "plan_failed"
)

// outcome is how one removal ended, as the job item shows it.
type outcome struct {
	state, code, message string
}

// execute is the body of the job. The lock is freed once the removals are done and before the last
// item is finished: a job finishes when its last item does, so a reader that sees it finished
// finds the environment free.
func (r *Runner) execute(ctx context.Context, req RunRequest, runID string, release func(), rep *jobs.Reporter) {
	free := sync.OnceFunc(release)
	defer free()
	r.Locks.Attach(req.EnvID, rep.JobID())
	run := r.begin(req, runID)
	defer run.end(r, req)

	selected, err := r.plan(ctx, req, run)
	if err != nil {
		r.log().Warn("a janitor run could not plan", "environment", req.EnvID, "run", runID, "error", err)
		rep.Add(jobs.ItemSpec{ID: "plan", Label: "Plan the clean-up", Kind: "step"})
		free()
		rep.Finish("plan", jobs.ItemFailed, codePlanFailed, "TierPlan could not read the environment, so nothing was removed.", nil)
		return
	}
	// Every item is added before the first is finished: a job whose items have all ended has ended.
	for _, it := range selected {
		rep.Add(jobs.ItemSpec{ID: planItemID(it), Label: it.Name, Kind: string(it.Type)})
	}
	var tail func()
	for i, it := range selected {
		if ctx.Err() != nil {
			break // the hub ends the items that were never reached as cancelled
		}
		id := planItemID(it)
		rep.Update(id, func(x *jobs.Item) { x.State = jobs.ItemRunning })
		out := r.remove(ctx, req.EnvID, it, run)
		result := map[string]string{"nodeId": it.NodeID, "node": it.Node, "ref": it.Ref}
		finish := func() { rep.Finish(id, out.state, out.code, out.message, result) }
		if i == len(selected)-1 {
			tail = finish
			continue
		}
		finish()
	}
	free()
	if tail != nil {
		tail()
	}
}

// plan reads the environment fresh, plans with the policy and returns the items to remove: the
// ticked ones that are removable now, or all of them for a run that was not ticked, in the plan's
// order and each once.
func (r *Runner) plan(ctx context.Context, req RunRequest, run *runState) ([]Item, error) {
	pol := DefaultPolicy()
	if r.Policy != nil {
		p, err := r.Policy(ctx, req.EnvID)
		if err != nil {
			return nil, fmt.Errorf("read the policy: %w", err)
		}
		pol = p
	}
	snap, err := r.Engine.Snapshot(ctx, req.EnvID)
	if err != nil {
		return nil, fmt.Errorf("read the environment: %w", err)
	}
	ends := InspectCandidates(ctx, r.Engine, req.EnvID, Candidates(snap, pol))
	plan := Build(Input{Snapshot: snap, Ends: ends}, pol, r.clock().Now().UTC())
	if req.All {
		return plan.Items, nil
	}
	ticked := make(map[string]bool, len(req.Ticked))
	for _, t := range req.Ticked {
		ticked[t.key()] = true
	}
	var out []Item
	for _, it := range plan.Items {
		if ticked[ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}.key()] {
			out = append(out, it)
			delete(ticked, ItemRef{Type: it.Type, NodeID: it.NodeID, Ref: it.Ref}.key())
		}
	}
	run.notNow = len(ticked)
	return out, nil
}

// remove removes one item of the plan and says how it ended.
func (r *Runner) remove(ctx context.Context, envID string, it Item, run *runState) outcome {
	var err error
	switch it.Type {
	case KindContainer:
		err = r.Engine.RemoveContainer(ctx, envID, it.NodeID, it.Ref)
	default:
		err = errUnsupported
	}
	switch {
	case err == nil:
		run.removed++
		return outcome{state: jobs.ItemDone, message: "Removed."}
	case errors.Is(err, ErrGone):
		run.gone++
		return outcome{state: jobs.ItemDone, code: codeGone, message: "It was already gone."}
	case errors.Is(err, ErrNoLongerStale):
		run.refused++
		return outcome{state: jobs.ItemFailed, code: codeNoLongerStale, message: "It is no longer a stale container, so it was left alone."}
	case errors.Is(err, ErrInUse):
		run.refused++
		return outcome{state: jobs.ItemFailed, code: codeInUse, message: "The engine says it is in use, so it was left alone."}
	case errors.Is(err, ErrNodeOffline):
		run.failed++
		return outcome{state: jobs.ItemFailed, code: codeNodeOffline, message: "Its node is not connected, so nothing was sent."}
	default:
		run.failed++
		r.log().Warn("a janitor run could not remove an item", "environment", envID, "type", it.Type, "node", it.NodeID, "ref", it.Ref, "error", err)
		return outcome{state: jobs.ItemFailed, code: codeFailed, message: "It could not be removed."}
	}
}

// errUnsupported is the removal of a kind of object the janitor does not remove yet.
var errUnsupported = errors.New("janitor: removing this kind of object is not supported")
