// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// Recovery after a restart (plan 12-24, IMG-07, D-12, D-14, RESEARCH Pitfall 13). The jobs and the operation lock are memory only, so
// an update that was running when TierPlan stopped is not running now, whatever its run row says. On startup every run that is
// still queued or running is found and rolled back the way any failed update is: a plain service gets its previous digest back, a
// plain container gets its old container back (renamed back and started, the replacement removed) and a stack goes back to the
// revision before the update. The run then reads rolled_back, or rollback_failed, with the restart named as the reason.
//
// Recovery never starts or finishes an update (IMG-06): every write it makes restores what ran before the update, and nothing
// pulls or creates anything.

const (
	// ReasonServerRestarted is the reason of a run that was interrupted by a restart of TierPlan: "TierPlan restarted during this
	// update".
	ReasonServerRestarted = "server_restarted"
	// ReasonEnvironmentOffline is the reason of a run whose rollback never ran because its environment stayed out of reach.
	ReasonEnvironmentOffline = "environment_offline"

	// LockPoll is how often recovery looks again at an environment whose operation lock is held.
	LockPoll = 10 * time.Second

	// OfflineBackoffStart is the first wait before an environment that is out of reach is tried again; it doubles up to
	// OfflineBackoffMax. After OfflineGiveUp the rollback is given up as rollback_failed with the reason environment_offline.
	OfflineBackoffStart = time.Minute
	OfflineBackoffMax   = 15 * time.Minute
	OfflineGiveUp       = 2 * time.Hour
)

// Recovery rolls back the image updates a restart interrupted.
type Recovery struct {
	// Runs is where the runs are read; Applier does the rollback of a plain service or container, under the environment's lock.
	Runs    *RunStore
	Applier *Applier
	// Stack rolls an interrupted update of a stack back to the revision before it, in a stack job of its own. It returns nil when
	// the stack was put back (it ends the run itself, which Run leaves as it is), a *LockedError when the environment's lock is
	// held (Run waits and calls it again), and any other error when the stack could not be put back.
	Stack func(ctx context.Context, run Run) error
	// Boot is when this process started serving, taken before anything could start an update. A run that started at or after it is this
	// process's own and is never recovered, however late the list of unfinished runs is read. Zero recovers every unfinished run.
	Boot time.Time
	// Clock is the time every wait is on. Nil means the Applier's.
	Clock stackops.Clock
	// ActorOf names who a run was started by, as the audit log has them. Nil reads a system name as the system and anything else as a
	// user of that ID.
	ActorOf func(ctx context.Context, key string) audit.Actor
	Log     *slog.Logger
}

func (r *Recovery) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Recovery) clock() stackops.Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return r.Applier.clock()
}

// tried is how one try at a run ended.
type tried int

const (
	// triedDone: the run has been dealt with, rolled back or ended.
	triedDone tried = iota
	// triedLocked: the environment's lock is held, or the owner has no job slot; look again soon.
	triedLocked
	// triedOffline: the environment cannot be reached; look again after a backoff.
	triedOffline
)

// Run finds every run left queued or running and recovers it. The runs of one environment are taken one after the other; the
// environments go together, so one that is out of reach does not hold up the rest. It returns when every run has been dealt with or
// ctx ends; a run that was not reached stays running and is found again by the next start.
func (r *Recovery) Run(ctx context.Context) {
	if r.Runs == nil || r.Applier == nil {
		return
	}
	runs, err := r.Runs.Unfinished(ctx)
	if err != nil {
		r.log().Error("the image updates that a restart interrupted could not be read", "error", err)
		return
	}
	if !r.Boot.IsZero() {
		runs = slices.DeleteFunc(runs, func(run Run) bool { return !run.StartedAt.Before(r.Boot) })
	}
	if len(runs) == 0 {
		return
	}
	r.log().Warn("image updates that were running when the server stopped are being rolled back", "count", len(runs))
	byEnv := map[string][]Run{}
	var order []string
	for _, run := range runs {
		if _, seen := byEnv[run.EnvID]; !seen {
			order = append(order, run.EnvID)
		}
		byEnv[run.EnvID] = append(byEnv[run.EnvID], run)
	}
	var wg sync.WaitGroup
	for _, env := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, run := range byEnv[env] {
				if ctx.Err() != nil {
					return
				}
				r.recoverRun(ctx, run)
			}
		}()
	}
	wg.Wait()
}

// recoverRun deals with one run: a try, and, while the environment's lock is held or the environment is out of reach, another.
func (r *Recovery) recoverRun(ctx context.Context, run Run) {
	// reread takes the run as it is on record now: a try that waits can be long, and the run may have ended meanwhile (its own job was
	// not gone after all, or a stack's seam ended it). It reports false when there is nothing left to recover.
	reread := func() bool {
		fresh, err := r.Runs.Get(ctx, run.ID)
		if err != nil {
			return ctx.Err() == nil && !errors.Is(err, sql.ErrNoRows)
		}
		if fresh.State != RunQueued && fresh.State != RunRunning {
			return false
		}
		run = fresh
		return true
	}
	if !reread() {
		return
	}
	if run.State == RunQueued {
		// A run that never started has touched nothing.
		r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		return
	}
	start := r.clock().Now()
	delay := OfflineBackoffStart
	for {
		switch r.attempt(ctx, run) {
		case triedDone:
			return
		case triedLocked:
			if !r.wait(ctx, LockPoll) || !reread() {
				return
			}
		case triedOffline:
			if !r.clock().Now().Before(start.Add(OfflineGiveUp)) {
				r.log().Error("an interrupted image update could not be rolled back: its environment stayed out of reach", "environment", run.EnvID, "workload", run.Workload, "run", run.ID)
				r.finish(ctx, run, RunRollbackFailed, ReasonEnvironmentOffline, ReasonServerRestarted)
				return
			}
			if !r.wait(ctx, delay) || !reread() {
				return
			}
			delay = min(delay*2, OfflineBackoffMax)
		}
	}
}

// wait waits on the clock and reports whether ctx is still alive.
func (r *Recovery) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-r.clock().After(d):
		return true
	}
}

// attempt makes one try at a run, by its kind.
func (r *Recovery) attempt(ctx context.Context, run Run) tried {
	switch run.Kind {
	case "service":
		return r.recoverService(ctx, run)
	case "container":
		return r.recoverContainer(ctx, run)
	case "stack":
		return r.recoverStack(ctx, run)
	}
	r.log().Warn("an interrupted image update is of a kind recovery does not know", "run", run.ID, "kind", run.Kind)
	r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
	return triedDone
}

// actor is who the run was started by, as the audit log has them.
func (r *Recovery) actor(ctx context.Context, run Run) audit.Actor {
	if r.ActorOf != nil {
		return r.ActorOf(ctx, run.Actor)
	}
	if strings.HasPrefix(run.Actor, "system:") {
		return audit.Actor{Type: audit.ActorSystem, ID: run.Actor, Name: run.Actor}
	}
	return audit.Actor{Type: audit.ActorUser, ID: run.Actor, Name: run.Actor}
}

// owner is the job owner of the run's actor, as the hub keeps it.
func owner(run Run) string {
	if strings.HasPrefix(run.Actor, "system:") {
		return "system:" + run.Actor
	}
	return "user:" + run.Actor
}

// request is the update the run was, as the audit entries of its end need it.
func (r *Recovery) request(ctx context.Context, run Run) Request {
	return Request{EnvID: run.EnvID, Kind: run.Kind, Name: run.Workload, Actor: r.actor(ctx, run), Owner: owner(run), BulkID: run.BulkID}
}

// targetOf is the workload of a plain service's or container's run, as the end of an update names it.
func targetOf(run Run) (target, bool) {
	if len(run.Items) == 0 {
		return target{}, false
	}
	it := run.Items[0]
	t := target{
		kind: run.Kind, envID: run.EnvID, name: run.Workload, id: run.WorkloadID, image: it.Image, from: it.FromDigest, to: it.ToDigest,
		runStart: run.StartedAt,
	}
	if it.Image != "" {
		t.ref = it.Image + "@" + it.ToDigest
	}
	return t, true
}

// finish ends a run that is not rolled back by a job of recovery's own: one that never started, whose workload is gone, whose
// rollback was given up, or that the stack's seam left open. It writes once, so a run something else has ended is left as it is, and
// only then are the memory of the failure and the audit entries made.
func (r *Recovery) finish(ctx context.Context, run Run, st RunState, reason, cause string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	ok, err := r.Runs.Finish(wctx, run.ID, st, reason, run.RevisionSeq, run.RollbackSeq)
	if err != nil {
		r.log().Error("an interrupted image update could not be finished on record", "run", run.ID, "error", err)
		return
	}
	if !ok {
		return
	}
	req := r.request(wctx, run)
	if run.Kind == "stack" {
		r.auditStack(wctx, req, run, st, reason, cause)
		return
	}
	t, found := targetOf(run)
	if !found {
		return
	}
	r.Applier.rememberFailure(wctx, run, t, st, cause)
	r.Applier.auditEnd(wctx, req, t, st, reason, cause)
}

// auditStack writes the audit entries of the end of a stack's run that recovery ended itself, one set for each service the update
// changed, in the words endStackRun uses.
func (r *Recovery) auditStack(ctx context.Context, req Request, run Run, st RunState, reason, cause string) {
	if r.Applier.Audit == nil {
		return
	}
	for _, it := range run.Items {
		entry := func(action string, result audit.Result, why string) audit.Entry {
			return audit.Entry{
				Actor: req.Actor, TargetType: "service", TargetID: it.Service, EnvID: run.EnvID, Action: action, Result: result, Reason: why,
				Details: map[string]string{
					"kind": "service", "workload": it.Service, "image": it.Image, "from_digest": it.FromDigest, "to_digest": it.ToDigest, "reason": cause,
				},
			}
		}
		var entries []audit.Entry
		switch st {
		case RunRolledBack:
			entries = append(entries, entry(audit.ActionImageUpdateFailed, audit.ResultFailure, cause), entry(audit.ActionImageUpdateRolledBack, audit.ResultSuccess, ""))
		case RunRollbackFailed:
			entries = append(entries, entry(audit.ActionImageUpdateFailed, audit.ResultFailure, cause), entry(audit.ActionImageUpdateRolledBack, audit.ResultFailure, reason))
		default:
			entries = append(entries, entry(audit.ActionImageUpdateFailed, audit.ResultFailure, reason))
		}
		for _, e := range entries {
			if err := r.Applier.Audit.Record(ctx, e); err != nil {
				r.log().Error("an interrupted image update could not be written to the audit log", "run", run.ID, "action", e.Action, "error", err)
			}
		}
	}
}

// begin takes the environment's lock for the rollback of a run and starts the job that does it. It answers triedLocked when the
// lock is held or the owner has no job slot left, and triedDone otherwise (the job has ended by then). The job's body gets the
// function that ends the run, which also frees the lock.
func (r *Recovery) begin(ctx context.Context, run Run, t target, items []jobs.ItemSpec, body func(jctx context.Context, rep *jobs.Reporter, end endFunc)) tried {
	a := r.Applier
	req := r.request(ctx, run)
	release, held, ok := a.Locks.TryAcquire(run.EnvID, stackops.Holder{
		Kind: stackops.HolderKindImageUpdate, Workload: t.name, By: req.Actor.Name, Started: r.clock().Now().UTC(),
	})
	if !ok {
		r.log().Info("an interrupted image update waits for the environment's lock", "environment", run.EnvID, "holder", held.Kind)
		return triedLocked
	}
	meta := map[string]string{
		MetaWorkload: t.name, MetaKind: t.kind, MetaRunID: run.ID, MetaBy: req.Actor.Name, MetaFrom: t.from, MetaTo: t.to,
	}
	if run.BulkID != "" {
		meta[MetaBulk] = run.BulkID
	}
	// The body waits on ready until the job is attached to the lock: a body that ended first would free the lock, another operation
	// could take the environment, and Attach (which goes by the environment) would write this finished job into its holder.
	ready := make(chan struct{})
	job, err := a.Jobs.Start(jobs.Spec{Kind: JobKind, EnvID: run.EnvID, Owner: req.Owner, Meta: meta, Items: items},
		func(jctx context.Context, rep *jobs.Reporter) {
			end := a.newEnd(jctx, req, run, t, release)
			// Whatever ends the body without ending the run (a panic included) ends it failed, and frees the lock.
			defer end(RunFailed, ReasonServerRestarted, ReasonServerRestarted)
			select {
			case <-ready:
			case <-jctx.Done():
				if jobs.Closing(jctx) {
					end(runLeftOpen, "", "")
				}
				return
			}
			rep.Output("TierPlan restarted during this update. Putting " + t.name + " back to " + shortDigest(t.from))
			body(jctx, rep, end)
		})
	if err != nil {
		release()
		if errors.Is(err, jobs.ErrTooMany) {
			return triedLocked
		}
		r.log().Error("the rollback of an interrupted image update could not be started", "run", run.ID, "error", err)
		r.finish(ctx, run, RunRollbackFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	a.Locks.Attach(run.EnvID, job.ID)
	close(ready)
	if _, err := a.Jobs.Wait(ctx, job.ID); err != nil && ctx.Err() == nil {
		r.log().Warn("the rollback of an interrupted image update could not be waited for", "run", run.ID, "error", err)
	}
	return triedDone
}

// restartedEnd is end for a rollback of recovery's: a restore that failed keeps the restart as its reason, as a restore that worked
// does, so every run recovery ends reads the same.
func restartedEnd(end endFunc) endFunc {
	return func(st RunState, reason, cause string) {
		if st == RunRollbackFailed && reason == ReasonRestoreFailed {
			reason = ReasonServerRestarted
		}
		end(st, reason, cause)
	}
}

// recoverService puts a plain service back to the digest it ran before the update. It reads the service first: one that is gone is
// ended, one that cannot be read is tried again later, and the rest is the rollback of a failed update (rollbackService), which
// writes only when the service runs the new digest, waits when the service runs the old one again after the run wrote it, ends the run
// failed when the service shows no write since the run began (the restart came before it) and leaves a third image alone.
func (r *Recovery) recoverService(ctx context.Context, run Run) tried {
	a := r.Applier
	t, ok := targetOf(run)
	if !ok {
		r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	if a.Service == nil || a.Service.Clusters == nil || a.Jobs == nil || a.Locks == nil || a.Aggregator == nil {
		r.log().Error("the applier cannot roll an interrupted service update back: it is not wired", "run", run.ID)
		r.finish(ctx, run, RunRollbackFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	var detail dockerx.ServiceDetail
	err := a.Service.Clusters.ClusterRead(ctx, run.EnvID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ServiceInspect(ctx, run.WorkloadID)
		return e
	})
	switch {
	case ctx.Err() != nil:
		return triedDone
	case errors.Is(err, dockerx.ErrServiceNotFound):
		r.log().Warn("the service of an interrupted image update is gone, so there is nothing to put back", "run", run.ID, "service", run.Workload)
		r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	case err != nil:
		return triedOffline
	}
	// The image the service had is the one it has now with the previous digest: the same text the update wrote its digest into.
	previous := t.ref
	var spec swarm.ServiceSpec
	if json.Unmarshal(detail.Spec, &spec) == nil && spec.TaskTemplate.ContainerSpec != nil {
		base, _, _ := strings.Cut(spec.TaskTemplate.ContainerSpec.Image, "@")
		previous = base + "@" + t.from
		t.ref = previous
	}
	items := []jobs.ItemSpec{
		{ID: ItemRollback, Label: "Roll back to the previous version", Kind: stackops.KindStep},
		{ID: ItemRollbackWait, Label: "Wait for the previous version to run", Kind: stackops.KindStep},
	}
	return r.begin(ctx, run, t, items, func(jctx context.Context, rep *jobs.Reporter, end endFunc) {
		a.rollbackService(jctx, rep, t, previous, ReasonServerRestarted, restartedEnd(end))
	})
}

// recoverStack rolls an interrupted update of a stack back through the seam of the api package, which holds the stack's own
// machinery. Whatever the seam leaves open is ended here.
func (r *Recovery) recoverStack(ctx context.Context, run Run) tried {
	if r.Stack == nil {
		r.log().Error("an interrupted stack update cannot be rolled back: recovery has no stack seam", "run", run.ID)
		r.finish(ctx, run, RunRollbackFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	if r.Applier.Aggregator != nil {
		if _, _, err := r.Applier.Aggregator.FreshSnapshot(ctx, run.EnvID); err != nil {
			if ctx.Err() != nil {
				return triedDone
			}
			return triedOffline
		}
	}
	err := r.Stack(ctx, run)
	var locked *LockedError
	switch {
	case ctx.Err() != nil:
		return triedDone
	case errors.As(err, &locked):
		return triedLocked
	case errors.Is(err, jobs.ErrTooMany):
		return triedLocked
	case err != nil:
		r.log().Error("an interrupted stack update could not be rolled back", "run", run.ID, "stack", run.Workload, "error", err)
		r.finish(ctx, run, RunRollbackFailed, ReasonServerRestarted, ReasonServerRestarted)
	default:
		r.finish(ctx, run, RunRolledBack, ReasonServerRestarted, ReasonServerRestarted)
	}
	return triedDone
}

// containerScene is where the engine stands with a container's interrupted update: the node, the old container, the container that
// has the old one's name now (when it is not the old one) and the replacement still under its temporary name.
type containerScene struct {
	node        string
	old         envs.ContainerRow
	named, temp *envs.ContainerRow
}

// containerSceneOf finds, among the containers of the environment, the old container of the run and what stands beside it. Docker
// names are unique per host and not per environment, so the container that has the old one's name now and the replacement under
// its temporary name are only those on the old container's own node: a container of the same name on another node is not part of
// this update. When the old container is gone there is no node to go by, and the container holding the name is taken as it is.
func containerSceneOf(snap envs.Aggregate, run Run, tempName string) (containerScene, bool) {
	var sc containerScene
	var haveOld bool
	for _, c := range snap.Containers {
		if c.ID == run.WorkloadID {
			sc.old, haveOld = c, true
			break
		}
	}
	for i := range snap.Containers {
		c := snap.Containers[i]
		if c.ID == run.WorkloadID || (haveOld && c.NodeID != sc.old.NodeID) {
			continue
		}
		switch name := strings.TrimPrefix(c.Name, "/"); name {
		case run.Workload:
			sc.named = &c
		case tempName:
			sc.temp = &c
		}
	}
	sc.node = sc.old.NodeID
	return sc, haveOld
}

// recoverContainer puts the old container of an interrupted update back. It looks at what the node holds, and puts back only what
// the update displaced: the replacement is removed, the old container is given its name back and, if the update stopped it, started.
func (r *Recovery) recoverContainer(ctx context.Context, run Run) tried {
	a := r.Applier
	t, ok := targetOf(run)
	if !ok {
		r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	if a.Aggregator == nil || a.Jobs == nil || a.Locks == nil {
		r.log().Error("the applier cannot roll an interrupted container update back: it is not wired", "run", run.ID)
		r.finish(ctx, run, RunRollbackFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	snap, _, err := a.Aggregator.FreshSnapshot(ctx, run.EnvID)
	if err != nil {
		if ctx.Err() != nil {
			return triedDone
		}
		return triedOffline
	}
	short := run.WorkloadID
	if len(short) > shortIDLen {
		short = short[:shortIDLen]
	}
	tempName := run.Workload + "-tierplan-new-" + short
	sc, haveOld := containerSceneOf(snap, run, tempName)
	if !haveOld {
		if len(snap.NotReporting) > 0 {
			// The node the container is on may be one of those that did not answer.
			return triedOffline
		}
		if sc.named != nil && updateFinished(snap, *sc.named, run) {
			// The restart came after the update brought the replacement up and removed the old container, before it wrote its end.
			r.log().Info("an interrupted container update had finished: the replacement runs and the old container is gone", "run", run.ID, "container", run.Workload)
			r.finish(ctx, run, RunDone, "", "")
			return triedDone
		}
		r.log().Warn("the container of an interrupted image update is gone, so there is nothing to put back", "run", run.ID, "container", run.Workload)
		r.finish(ctx, run, RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		return triedDone
	}
	items := []jobs.ItemSpec{{ID: ItemRollback, Label: "Put the old container back", Kind: stackops.KindStep}}
	return r.begin(ctx, run, t, items, func(jctx context.Context, rep *jobs.Reporter, end endFunc) {
		r.restoreContainer(jctx, rep, run, t, sc, tempName, restartedEnd(end))
	})
}

// updateFinished reports whether c, the container that holds the workload's name, is the replacement of an update that had done
// its work: it runs, and the digest its image has for the repository the run names is the one the run was updating to.
func updateFinished(snap envs.Aggregate, c envs.ContainerRow, run Run) bool {
	if len(run.Items) == 0 || c.State != "running" {
		return false
	}
	it := run.Items[0]
	spec, err := registry.ParseRef(it.Image)
	if err != nil || it.ToDigest == "" {
		return false
	}
	for _, im := range snap.Images {
		if im.NodeID == c.NodeID && im.ID == c.ImageID {
			return runningDigest(im.RepoDigests, spec) == it.ToDigest
		}
	}
	return false
}

// restoreContainer does the work of recoverContainer under the lock, on what the node holds now.
func (r *Recovery) restoreContainer(ctx context.Context, rep *jobs.Reporter, run Run, t target, sc containerScene, tempName string, end endFunc) {
	a := r.Applier
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	name := run.Workload
	short := strings.TrimPrefix(tempName, name+"-tierplan-new-")
	oldName := name + "-tierplan-old-" + short

	fail := func(what string, err error) {
		r.log().Error("an interrupted container update could not be rolled back", "run", run.ID, "container", name, "step", what, "error", err)
		rep.Output("The old container could not be put back. It is " + oldName + " if it was renamed; check " + name + " on the node.")
		end(RunRollbackFailed, ReasonRestoreFailed, ReasonServerRestarted)
		rep.Finish(ItemRollback, jobs.ItemFailed, ReasonRestoreFailed, itemMessages[ReasonRestoreFailed], nil)
	}
	rep.Update(ItemRollback, func(it *jobs.Item) { it.State = jobs.ItemRunning })

	var old containerInspect
	if err := a.Aggregator.OnNode(rctx, run.EnvID, sc.node, func(c *dockerx.Client) error {
		doc, e := c.ContainerInspectFull(rctx, sc.old.ID)
		if e != nil {
			return e
		}
		old.name = strings.TrimPrefix(doc.Name, "/")
		if doc.State != nil {
			old.running = doc.State.Running
			old.finished, _ = time.Parse(time.RFC3339Nano, doc.State.FinishedAt)
		}
		return nil
	}); err != nil {
		if dockerx.IsNotFound(err) {
			end(RunFailed, ReasonServerRestarted, ReasonServerRestarted)
			rep.Finish(ItemRollback, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
			return
		}
		fail("read the old container", err)
		return
	}
	write := func(fn func(*dockerx.Client) error) error {
		return a.Aggregator.WriteOnNode(rctx, run.EnvID, sc.node, fn)
	}

	// The replacement that was still under its temporary name is TierPlan's own and goes first.
	if sc.temp != nil {
		rep.Output("Remove " + tempName)
		err := write(func(c *dockerx.Client) error {
			return c.ContainerRemove(rctx, sc.temp.ID, dockerx.RemoveOptions{Force: true})
		})
		if err != nil && !dockerx.IsNotFound(err) {
			fail("remove the temporary replacement", err)
			return
		}
	}
	recorded := run.OldContainer != ""
	if !recorded {
		// The old container was never stopped, so there is nothing to put back.
		end(RunFailed, ReasonServerRestarted, ReasonServerRestarted)
		rep.Finish(ItemRollback, jobs.ItemDone, "", "", nil)
		return
	}

	// The update stopped the old container when it is not running and stopped after the replacement was created. The replacement's
	// creation time is the node's own clock, as the old container's end is.
	since := run.StartedAt
	switch {
	case sc.named != nil:
		since = sc.named.Created
	case sc.temp != nil:
		since = sc.temp.Created
	}
	stoppedByUpdate := !old.running && !old.finished.IsZero() && !old.finished.Before(since)
	displaced := old.name != name
	switch {
	case !displaced:
	case old.name != oldName:
		// Somebody else renamed it: it is theirs now.
		r.log().Warn("the old container of an interrupted update was renamed by someone else, so it is left alone", "run", run.ID, "container", name)
		end(RunFailed, ReasonContainerChanged, ReasonContainerChanged)
		rep.Finish(ItemRollback, jobs.ItemFailed, ReasonContainerChanged, itemMessages[ReasonContainerChanged], nil)
		return
	default:
		if sc.named != nil {
			rep.Output("Remove the replacement " + name)
			err := write(func(c *dockerx.Client) error {
				return c.ContainerRemove(rctx, sc.named.ID, dockerx.RemoveOptions{Force: true})
			})
			if err != nil && !dockerx.IsNotFound(err) {
				fail("remove the replacement", err)
				return
			}
		}
		rep.Output("Rename " + oldName + " to " + name)
		if err := write(func(c *dockerx.Client) error { return c.ContainerRename(rctx, sc.old.ID, name) }); err != nil {
			fail("give the old container its name back", err)
			return
		}
	}
	if stoppedByUpdate {
		rep.Output("Start " + name)
		if err := write(func(c *dockerx.Client) error {
			return c.ContainerAction(rctx, sc.old.ID, dockerx.VerbStart, dockerx.ActionOptions{})
		}); err != nil {
			fail("start the old container", err)
			return
		}
	}
	if displaced || stoppedByUpdate {
		end(RunRolledBack, ReasonServerRestarted, ReasonServerRestarted)
	} else {
		// The update had not touched the old container yet.
		end(RunFailed, ReasonServerRestarted, ReasonServerRestarted)
	}
	rep.Output(name + " runs the version it ran before the update")
	rep.Finish(ItemRollback, jobs.ItemDone, "", "", nil)
}

// containerInspect is what the rollback reads of the old container.
type containerInspect struct {
	name     string
	running  bool
	finished time.Time
}
