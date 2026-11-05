// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The update of a plain swarm service (plan 12-15, D-07, D-12, D-13). The service is read at its current version, one field of
// it changes (TaskTemplate.ContainerSpec.Image) and the whole spec is sent back, so the person's update_config and every other
// setting are kept. The image is pulled on the nodes that run the service's tasks first, so a pull that fails ends the update
// before the service is touched.

const (
	// pullTimeout is how long the pull on one node may take.
	pullTimeout = 30 * time.Minute

	// pollReadTimeout bounds one read of a service and its tasks while it is waited for.
	pollReadTimeout = 20 * time.Second

	// credentialTimeout bounds a write of a credential's bookkeeping after a pull.
	credentialTimeout = 10 * time.Second
)

// itemMessages are the sentences of the codes the update puts on a step. A code of a pull has its own, from stackops.
var itemMessages = map[string]string{
	ReasonPullFailed:     "The image could not be pulled, so nothing was changed.",
	ReasonDidntStart:     "The new version did not start.",
	ReasonTimeout:        "The service did not finish updating in time.",
	ReasonUnhealthy:      "The new version did not become healthy.",
	ReasonServiceChanged: "The service was changed by someone else while it was being updated. Nothing was changed.",
	ReasonRejected:       "The swarm refused the new version. Nothing was changed.",
	ReasonEngine:         "The swarm could not be reached.",
	ReasonCancelled:      "The update was cancelled.",
	"network_full":       "A network the service needs has no free address left.",

	// The reasons and codes of a container's update (plan 12-20).
	ReasonCreateRefused:    "The new container could not be created, so nothing was changed.",
	ReasonAgentHostPolicy:  "The node's agent doesn't let TierPlan create containers with host access. Nothing was changed.",
	ReasonContainerChanged: "The container was changed or removed by someone else while it was being updated. Nothing was changed.",
	ReasonRestoreFailed:    "The new container failed and the old one could not be put back.",
	"auto_remove":          "A container started with automatic removal is removed when it stops, so it can't be put back if the update fails. Nothing was changed.",
	"remove_old_failed":    "The new container is running. The old one could not be removed; it is stopped and keeps its temporary name.",

	// The wait of a service that a person cancelled once the swarm had taken the update (CR-01), and the restore of a run whose update
	// never changed its service (CR-02).
	CodeNothingToPutBack: "The update had not changed the service, so nothing was put back.",
	CodeNotRecorded:      "The update could not be recorded, so nothing was changed.",
	CodeWaitCancelled:    "Cancelled while waiting for the new version to run. The update had been applied and is left as it is; it was not confirmed running.",

	// The codes of the restore of a service (plan 12-22).
	CodeRestoreRefused:   "The swarm would not take the previous version back.",
	CodeRestoreUnsettled: "The previous version did not run in time.",
	CodeRestoreChanged:   "The service was changed by someone else, so the previous version was not put back.",
}

// The codes of the restore steps of a service's update.
const (
	CodeRestoreRefused   = "restore_refused"
	CodeRestoreUnsettled = "restore_unsettled"
	CodeRestoreChanged   = "restore_service_changed"
	CodeWaitCancelled    = "wait_cancelled"

	// CodeNotRecorded is the code of the update step of a run that could not record the version of its write, and so did not make it.
	CodeNotRecorded = "run_not_recorded"

	// CodeNothingToPutBack is the code of the restore step of a run whose update never changed its service (CR-02).
	CodeNothingToPutBack = "nothing_to_put_back"

	// updateRollbackCompleted is the UpdateStatus of a service the swarm rolled back by itself (the person's failure action).
	updateRollbackCompleted = "rollback_completed"
)

// updateService does the update of one plain service and ends the run through end. end is called once, before the last step of
// the job is finished, and frees the environment's lock. A failure before the swarm was written ends the run failed. A failure
// after it puts the previous image back (rollbackService) and ends the run rolled_back, or rollback_failed.
func (a *Applier) updateService(ctx context.Context, r *jobs.Reporter, run Run, t target, end endFunc) {
	// order is the steps still to come, so a failure can end the rest as skipped.
	order := []string{ItemPull, ItemUpdate, ItemWait}
	// finish ends the failing step and skips the rest, without ending the run.
	finish := func(item, code, reason string) {
		msg := itemMessages[code]
		if msg == "" {
			msg = itemMessages[reason]
		}
		if m := stackops.CodeMessage(code); m != "" {
			msg = m
		}
		r.Finish(item, jobs.ItemFailed, code, msg, nil)
		skip := false
		for _, id := range append(order, ItemHealth) {
			if id == item {
				skip = true
				continue
			}
			if skip {
				r.Finish(id, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
			}
		}
	}
	fail := func(item, code, reason string) {
		if reason == ReasonCancelled && jobs.Closing(ctx) {
			// The server is stopping, nobody cancelled: the run is left for recovery to find.
			end(runLeftOpen, "", "")
		} else {
			end(RunFailed, reason, reason)
		}
		finish(item, code, reason)
	}
	if ctx.Err() != nil {
		fail(ItemPull, ReasonCancelled, ReasonCancelled)
		return
	}

	// The pull, on the nodes the service's tasks run on.
	r.Update(ItemPull, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if code, reason := a.pullOnTaskNodes(ctx, r, t); code != "" {
		fail(ItemPull, code, reason)
		return
	}

	// The spec, read at its current version with only the image changed.
	if ctx.Err() != nil {
		fail(ItemPull, ReasonCancelled, ReasonCancelled)
		return
	}
	r.Update(ItemUpdate, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	var detail dockerx.ServiceDetail
	if err := a.Service.Clusters.ClusterRead(ctx, t.envID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ServiceInspect(ctx, t.id)
		return e
	}); err != nil {
		a.log().Warn("could not read a service to update it", "environment", t.envID, "service", t.name, "error", err)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			fail(ItemUpdate, ReasonServiceChanged, ReasonServiceChanged)
			return
		}
		fail(ItemUpdate, ReasonEngine, ReasonEngine)
		return
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(detail.Spec, &spec); err != nil || spec.TaskTemplate.ContainerSpec == nil {
		a.log().Warn("a service has no container spec to update", "environment", t.envID, "service", t.name)
		fail(ItemUpdate, ReasonRejected, ReasonRejected)
		return
	}
	// The image the service has now is the one a rollback puts back: the same text, or, when it names no digest, the digest it ran.
	previous := spec.TaskTemplate.ContainerSpec.Image
	if !strings.HasSuffix(previous, "@"+t.from) {
		base, _, _ := strings.Cut(t.ref, "@")
		previous = base + "@" + t.from
	}
	spec.TaskTemplate.ContainerSpec.Image = t.ref
	hasHealth := hasHealthcheck(spec.TaskTemplate.ContainerSpec)
	if hasHealth {
		r.Add(jobs.ItemSpec{ID: ItemHealth, Label: "Wait for a healthy check", Kind: stackops.KindStep})
		order = append(order, ItemHealth)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		fail(ItemUpdate, ReasonRejected, ReasonRejected)
		return
	}

	// Fresh credentials for the nodes that pull the image: a header is never stored, and when no credential applies the
	// write says so (registryAuthFrom=spec) instead of inventing one.
	auth, used, err := a.authFor(ctx, t)
	if err != nil {
		a.log().Error("could not resolve the registry credential of a service update", "environment", t.envID, "service", t.name, "error", err)
		fail(ItemUpdate, stackops.CodeRegistryUnresolved, ReasonEngine)
		return
	}

	// The version the write is made at goes on the run first. A write moves the service's version, so after a restart this is what
	// tells a service the run wrote from one it never touched. A write that could not be marked is not made: recovery could not tell
	// whether it had happened.
	if err := a.recordWriteVersion(ctx, run, detail.Version); err != nil {
		a.log().Error("the version of a service an update is about to write could not be recorded", "environment", t.envID, "service", t.name, "error", err)
		fail(ItemUpdate, CodeNotRecorded, ReasonEngine)
		return
	}
	werr := a.Aggregator.WriteOnManager(context.WithoutCancel(ctx), t.envID, func(c *dockerx.Client) error {
		var e error
		if auth == "" {
			_, e = c.ServiceUpdateAuthFromSpec(context.WithoutCancel(ctx), detail.ID, detail.Version, raw)
		} else {
			_, e = c.ServiceUpdate(context.WithoutCancel(ctx), detail.ID, detail.Version, raw, auth)
		}
		return e
	})
	if used != nil {
		a.noteCredential(ctx, used, "")
	}
	if werr != nil {
		code := ReasonEngine
		var rejected *dockerx.SpecRejectedError
		switch {
		case errors.Is(werr, dockerx.ErrServiceChanged), errors.Is(werr, dockerx.ErrServiceNotFound):
			code = ReasonServiceChanged
		case errors.As(werr, &rejected):
			code = ReasonRejected
		}
		a.log().Warn("a service update was not accepted", "environment", t.envID, "service", t.name, "reason", code, "error", werr)
		fail(ItemUpdate, code, code)
		return
	}
	r.Output("Updated " + t.name + " to " + shortDigest(t.to))
	r.Finish(ItemUpdate, jobs.ItemDone, "", "", map[string]string{"from": t.from, "to": t.to})

	// The wait for the new tasks (and, when the image has a health check, for them to be healthy: the swarm keeps a task in
	// starting until its check passes).
	r.Update(ItemWait, func(it *jobs.Item) { it.State = jobs.ItemRunning; it.Unit = "tasks" })
	if hasHealth {
		// The health step counts the window it is waited for in, so the console can show how long is left.
		r.Update(ItemHealth, func(it *jobs.Item) {
			it.State, it.Unit, it.Total = jobs.ItemRunning, "seconds", int64(a.convergeTimeout()/time.Second)
		})
	}
	var (
		last     stackops.Convergence
		starting bool
	)
	werr = a.waitServiceSettled(ctx, t, detail.ID, detail.UpdateStatus, false, func(c stackops.Convergence, rows []dockerx.TaskRow) {
		last = c
		starting = anyStarting(c, rows)
		r.Update(ItemWait, func(it *jobs.Item) { it.Current, it.Total = int64(c.Running), int64(c.Want) })
	})

	switch {
	case werr == nil:
		end(RunDone, "", "")
		r.Finish(ItemWait, jobs.ItemDone, "", "", map[string]string{"running": strconv.Itoa(last.Running)})
		if hasHealth {
			r.Finish(ItemHealth, jobs.ItemDone, "", "", nil)
		}
	case jobs.Closing(ctx):
		// The server is stopping. The swarm took the new version and it was never confirmed, so the run is left running: recovery
		// reads the service at the next start and puts the previous version back, as it does after a crash.
		end(runLeftOpen, "", "")
	case ctx.Err() != nil:
		// A person who stopped watching has not said the version is bad: it is left as the swarm has it. The swarm took the update,
		// so the run is not a failure: it ends done, with the cancel as its reason, and the step says the version was not confirmed.
		end(RunDone, ReasonCancelled, "")
		r.Finish(ItemWait, jobs.ItemCancelled, CodeWaitCancelled, itemMessages[CodeWaitCancelled], map[string]string{"running": strconv.Itoa(last.Running)})
	default:
		reason, code := convergeReason(werr, last, hasHealth && starting)
		a.log().Warn("a service update did not settle", "environment", t.envID, "service", t.name, "reason", reason, "error", werr)
		item := ItemWait
		if reason == ReasonUnhealthy && hasHealth {
			item = ItemHealth
		}
		// The steps of the restore are added while the failing step still runs, so the job does not end between the two.
		r.Add(jobs.ItemSpec{ID: ItemRollback, Label: "Roll back to the previous version", Kind: stackops.KindStep})
		r.Add(jobs.ItemSpec{ID: ItemRollbackWait, Label: "Wait for the previous version to run", Kind: stackops.KindStep})
		if text := firstText(last.DeadErr, last.Err, last.Message); text != "" {
			r.Detail(item, text)
		}
		if item == ItemHealth {
			r.Finish(ItemWait, jobs.ItemDone, "", "", map[string]string{"running": strconv.Itoa(last.Running)})
		}
		finish(item, code, reason)
		a.rollbackService(ctx, r, t, previous, reason, end)
	}
}

// anyStarting reports whether a task the swarm means to run, on the service's current template, is still starting: the state a task
// with a health check stays in until the check passes.
func anyStarting(c stackops.Convergence, rows []dockerx.TaskRow) bool {
	for _, row := range rows {
		if row.DesiredState == stackops.TaskRunning && row.State == taskStarting && row.SpecHash == c.Template {
			return true
		}
	}
	return false
}

// taskStarting is the state of a task whose container started and whose health check has not passed.
const taskStarting = "starting"

// waitServiceSettled polls the service until it has settled on its spec. prior is the update status it had before the write the
// wait follows, which is not this write's. With restoring set, the rolled-back status of the swarm's own rollback is no
// stop either: the wait is for the previous version to run.
func (a *Applier) waitServiceSettled(ctx context.Context, t target, id string, prior *dockerx.ServiceUpdateStatus, restoring bool, on func(stackops.Convergence, []dockerx.TaskRow)) error {
	return stackops.WaitConverged(ctx, a.clock(), a.convergeTimeout(), func(ctx context.Context) (stackops.Convergence, []dockerx.TaskRow, error) {
		rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
		defer cancel()
		var (
			d    dockerx.ServiceDetail
			rows []dockerx.TaskRow
		)
		if err := a.Service.Clusters.ClusterRead(rctx, t.envID, func(c *dockerx.Client) error {
			var e error
			if d, _, e = c.ServiceInspect(rctx, id); e != nil {
				return e
			}
			rows, e = c.Tasks(rctx, dockerx.TaskFilter{ServiceIDs: []string{d.ID}})
			return e
		}); err != nil {
			return stackops.Convergence{}, nil, err
		}
		c := stackops.Progress(d, rows).IgnoringUpdate(prior)
		if restoring && c.State == updateRollbackCompleted {
			c = c.IgnoringUpdate(d.UpdateStatus)
		}
		return c, rows, nil
	}, nil, on)
}

// rollbackService puts the previous image back after an update failed for cause, and ends the run: rolled_back when the previous
// version runs again, rollback_failed when it does not (D-12). The service is read at its current version and only the image is
// changed, so the person's own update_config and every other setting is kept. If the swarm already rolled the service back by its
// own failure action, nothing is written and the wait is for that rollback. After a restart (t.resumed) a service that runs the
// previous version and that the run did not write, because the run recorded no version for its write or the service is still at it,
// is left as it is, and the run ends failed with nothing rolled back. A service that now runs neither of the two images was changed by
// someone else and is left alone. The registry credential is chosen again and goes to the engine only.
func (a *Applier) rollbackService(ctx context.Context, r *jobs.Reporter, t target, previous, cause string, end endFunc) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.convergeTimeout()+restoreTimeout)
	defer cancel()

	failed := func(item, code string) {
		a.log().Error("a service could not be put back to its previous version", "environment", t.envID, "service", t.name, "cause", cause, "code", code)
		end(RunRollbackFailed, ReasonRestoreFailed, cause)
		r.Finish(item, jobs.ItemFailed, code, itemMessages[code], nil)
		if item == ItemRollback {
			r.Finish(ItemRollbackWait, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		}
	}
	r.Update(ItemRollback, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	r.Output("Rolling " + t.name + " back to " + shortDigest(t.from))

	var detail dockerx.ServiceDetail
	if err := a.Service.Clusters.ClusterRead(rctx, t.envID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ServiceInspect(rctx, t.id)
		return e
	}); err != nil {
		a.log().Warn("could not read a service to put its previous version back", "environment", t.envID, "service", t.name, "error", err)
		failed(ItemRollback, CodeRestoreRefused)
		return
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(detail.Spec, &spec); err != nil || spec.TaskTemplate.ContainerSpec == nil {
		failed(ItemRollback, CodeRestoreRefused)
		return
	}
	prior := detail.UpdateStatus
	switch image := spec.TaskTemplate.ContainerSpec.Image; {
	case strings.HasSuffix(image, "@"+t.from) && t.resumed && (t.writeVersion == nil || *t.writeVersion == detail.Version):
		// The service runs the previous version and the run did not write it: it never recorded the version of its write (the restart
		// came during the pull), or the service is still at that version, which any write moves. Nothing was changed and nothing is
		// put back, so nothing is claimed rolled back or remembered. This is only a question for a run recovery takes up: an update
		// that is running has written the service.
		a.log().Info("an image update never changed its service, so there is nothing to put back", "environment", t.envID, "service", t.name, "cause", cause)
		end(RunFailed, cause, cause)
		r.Output(t.name + " still runs " + shortDigest(t.from) + ". The update had not changed it, so there is nothing to put back")
		r.Finish(ItemRollback, jobs.ItemDone, CodeNothingToPutBack, itemMessages[CodeNothingToPutBack], nil)
		r.Finish(ItemRollbackWait, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		return
	case strings.HasSuffix(image, "@"+t.from):
		// The swarm put the previous version back itself: its own rollback is what is waited for.
		r.Output("The swarm has already put the previous version back")
		prior = nil
	case strings.HasSuffix(image, "@"+t.to):
		if !digestGrammar.MatchString(t.from) {
			failed(ItemRollback, CodeRestoreRefused)
			return
		}
		spec.TaskTemplate.ContainerSpec.Image = previous
		raw, err := json.Marshal(spec)
		if err != nil {
			failed(ItemRollback, CodeRestoreRefused)
			return
		}
		auth, used, aerr := a.authFor(rctx, t)
		if aerr != nil {
			a.log().Error("could not resolve the registry credential of a service rollback", "environment", t.envID, "service", t.name, "error", aerr)
			failed(ItemRollback, CodeRestoreRefused)
			return
		}
		werr := a.Aggregator.WriteOnManager(rctx, t.envID, func(c *dockerx.Client) error {
			var e error
			if auth == "" {
				_, e = c.ServiceUpdateAuthFromSpec(rctx, detail.ID, detail.Version, raw)
			} else {
				_, e = c.ServiceUpdate(rctx, detail.ID, detail.Version, raw, auth)
			}
			return e
		})
		if used != nil {
			a.noteCredential(rctx, used, "")
		}
		if werr != nil {
			a.log().Warn("the swarm did not take a service's previous version back", "environment", t.envID, "service", t.name, "error", werr)
			failed(ItemRollback, CodeRestoreRefused)
			return
		}
	default:
		// Neither version: somebody else changed the service. Putting an image back over that would undo their change.
		a.log().Warn("a service was changed by someone else while its update failed, so it is left alone", "environment", t.envID, "service", t.name)
		end(RunFailed, cause, cause)
		r.Finish(ItemRollback, jobs.ItemFailed, CodeRestoreChanged, itemMessages[CodeRestoreChanged], nil)
		r.Finish(ItemRollbackWait, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		return
	}
	r.Output("Put " + t.name + " back to " + shortDigest(t.from))
	r.Finish(ItemRollback, jobs.ItemDone, "", "", map[string]string{"to": t.from})

	r.Update(ItemRollbackWait, func(it *jobs.Item) { it.State = jobs.ItemRunning; it.Unit = "tasks" })
	if err := a.waitServiceSettled(rctx, t, detail.ID, prior, true, func(c stackops.Convergence, _ []dockerx.TaskRow) {
		r.Update(ItemRollbackWait, func(it *jobs.Item) { it.Current, it.Total = int64(c.Running), int64(c.Want) })
	}); err != nil {
		a.log().Warn("a service's previous version did not run after it was put back", "environment", t.envID, "service", t.name, "error", err)
		failed(ItemRollbackWait, CodeRestoreUnsettled)
		return
	}
	end(RunRolledBack, cause, cause)
	r.Output(t.name + " runs " + shortDigest(t.from) + " again")
	r.Finish(ItemRollbackWait, jobs.ItemDone, "", "", nil)
}

// recordWriteVersion puts the version a write to the service will be made at on the run, before the write. A run that has ended, or
// that has no item to put it on, is an error: the write must not be made.
func (a *Applier) recordWriteVersion(ctx context.Context, run Run, version uint64) error {
	if len(run.Items) == 0 {
		return errors.New("the run has no item to record the version on")
	}
	items := slices.Clone(run.Items)
	items[0].WriteVersion = &version
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	ok, err := a.Runs.SetItems(wctx, run.ID, items)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the run is over")
	}
	return nil
}

// rememberFailure keeps what the next judgement needs of an update that ended (IMG-07, D-14). A good update forgets the failure the
// workload had. A rolled-back or rollback-failed update, and one whose pull failed (the digest could not even be fetched), remember
// the digest that failed, so the badge says so and stops offering it until the tag points at another. An update that was cancelled
// says nothing about the version, and one that was refused before anything changed, other than by its pull, is not remembered
// either. A failed write is logged and changes nothing else.
func (a *Applier) rememberFailure(ctx context.Context, run Run, t target, st RunState, cause string) {
	if a.Failures == nil {
		return
	}
	var err error
	switch {
	case st == RunDone:
		_, err = a.Failures.Clear(ctx, t.envID, t.kind, t.name)
	case cause == ReasonCancelled:
		return
	case st == RunRolledBack, st == RunRollbackFailed, st == RunFailed && cause == ReasonPullFailed:
		err = a.Failures.Record(ctx, Failure{
			EnvID: t.envID, Kind: t.kind, Workload: t.name, Digest: t.to, Reason: cause, RunID: run.ID,
			RollbackFailed: st == RunRollbackFailed,
		})
	default:
		return
	}
	if err != nil {
		a.log().Error("the outcome of an image update could not be remembered", "environment", t.envID, "workload", t.name, "error", err)
	}
}

// convergeReason words how a wait that did not settle ended: the reason the run keeps and the code of the step.
//
// A wait that ran out of time reads unhealthy, not timeout, when the image defines a health check and the tasks it was waiting for
// were still starting: the swarm keeps a task in that state until its check passes (starting says so).
func convergeReason(err error, last stackops.Convergence, starting bool) (reason, code string) {
	switch {
	case errors.Is(err, stackops.ErrConvergeTimeout):
		if starting {
			return ReasonUnhealthy, ReasonUnhealthy
		}
		return ReasonTimeout, ReasonTimeout
	case errors.Is(err, stackops.ErrNetworkFull):
		return ReasonDidntStart, "network_full"
	case errors.Is(err, stackops.ErrUpdateStopped), errors.Is(err, stackops.ErrTasksExhausted):
		if strings.Contains(strings.ToLower(last.DeadErr+" "+last.Err+" "+last.Message), "unhealthy") {
			return ReasonUnhealthy, ReasonUnhealthy
		}
		return ReasonDidntStart, ReasonDidntStart
	}
	return ReasonEngine, ReasonEngine
}

// firstText is the first of the texts that says anything.
func firstText(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

// hasHealthcheck reports whether the container spec defines a health check that runs.
func hasHealthcheck(cs *swarm.ContainerSpec) bool {
	h := cs.Healthcheck
	return h != nil && len(h.Test) > 0 && h.Test[0] != "NONE"
}

// authFor is the registry header the update carries, and the credential it was made from; "" and nil when no credential
// applies.
func (a *Applier) authFor(ctx context.Context, t target) (string, *registry.Candidate, error) {
	if a.Resolver == nil {
		return "", nil, nil
	}
	ref, err := registry.ParseRef(t.ref)
	if err != nil {
		return "", nil, err
	}
	return a.Resolver.AuthFor(ctx, t.envID, ref)
}

// noteCredential keeps the credential's bookkeeping after it was used: a registry that refused the sign-in records the failure
// with its code, and a pull that worked clears it. A failed write is logged and changes nothing else.
func (a *Applier) noteCredential(ctx context.Context, used *registry.Candidate, code string) {
	if used == nil || a.Resolver == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialTimeout)
	defer cancel()
	var err error
	switch code {
	case dockerx.PullCodeUnauthorized:
		err = a.Resolver.RecordFailure(wctx, used.ID, code)
	case "":
		err = a.Resolver.ClearFailure(wctx, used.ID)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		a.log().Error("could not update a credential's last failure", "credential", used.ID, "error", err)
	}
}

// pullOnTaskNodes pulls the new image on each node that holds a task the swarm means to run, one node at a time, with the
// credential the environment resolves for it. It answers the code of the step and the reason of the run when a pull failed, and
// empty strings when every pull worked or there was nothing to pull on: a node nothing reaches is left to the swarm, which pulls
// the image itself where the task lands. Nothing is retried.
func (a *Applier) pullOnTaskNodes(ctx context.Context, r *jobs.Reporter, t target) (code, reason string) {
	nodes, err := a.taskNodes(ctx, t)
	if err != nil {
		a.log().Warn("could not find the nodes a service runs on to pull its new image there", "environment", t.envID, "service", t.name, "error", err)
		// The swarm pulls the image itself where the tasks land, so the step is not run and the update goes on.
		r.Finish(ItemPull, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		return "", ""
	}
	if len(nodes) == 0 {
		r.Finish(ItemPull, jobs.ItemDone, "", "", map[string]string{"nodes": "0"})
		return "", ""
	}
	header, used, aerr := a.authFor(ctx, t)
	if aerr != nil {
		a.log().Error("could not resolve the registry credential of a service image", "environment", t.envID, "service", t.name, "error", aerr)
		return stackops.CodeRegistryUnresolved, ReasonPullFailed
	}
	index := map[string]int{}
	for _, node := range nodes {
		if code, reason := a.pullOn(ctx, r, t, node, header, used, index); code != "" {
			return code, reason
		}
	}
	a.noteCredential(ctx, used, "")
	r.Finish(ItemPull, jobs.ItemDone, "", "", map[string]string{"nodes": strconv.Itoa(len(nodes))})
	return "", ""
}

// pullOn pulls the new image on one node with the header the credential made, reporting its progress on the pull item (index maps a
// layer to its place in the item and belongs to the one step). It answers
// the code of the step and the reason of the run when the pull failed, empty strings when it worked. Nothing is retried.
func (a *Applier) pullOn(ctx context.Context, r *jobs.Reporter, t target, node taskNode, header string, used *registry.Candidate, index map[string]int) (code, reason string) {
	r.Output("Pull " + t.image + " on " + node.name)
	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	err := a.Aggregator.WriteOnNode(pctx, t.envID, node.id, func(c *dockerx.Client) error {
		_, e := c.ImagePull(pctx, t.pullRef, dockerx.PullOptions{EncodedAuth: header}, func(ev dockerx.PullEvent) {
			r.Update(ItemPull, func(it *jobs.Item) { stackops.ApplyPullEvent(it, index, ev) })
		})
		return e
	})
	if err == nil {
		return "", ""
	}
	c := stackops.PullErrorCode(err)
	if c == dockerx.PullCodeFailed || c == stackops.CodeNodeUnreachable {
		a.log().Warn("the pull of an image failed", "environment", t.envID, "workload", t.name, "node", node.id, "code", c, "error", err)
	}
	a.noteCredential(ctx, used, c)
	if c == dockerx.PullCodeCancelled || ctx.Err() != nil {
		return ReasonCancelled, ReasonCancelled
	}
	return c, ReasonPullFailed
}

// taskNode is a node the service has a task on that TierPlan can reach.
type taskNode struct{ id, name string }

// taskNodes are the reachable nodes that hold a task of the service the swarm means to run, sorted by name.
func (a *Applier) taskNodes(ctx context.Context, t target) ([]taskNode, error) {
	var rows []dockerx.TaskRow
	if err := a.Service.Clusters.ClusterRead(ctx, t.envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: []string{t.id}, DesiredState: stackops.TaskRunning})
		return e
	}); err != nil {
		return nil, err
	}
	targets, err := a.Aggregator.SwarmNodeTargets(ctx, t.envID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []taskNode
	for _, row := range rows {
		ref, ok := targets[row.NodeID]
		if !ok || seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		name := ref.Name
		if name == "" {
			name = ref.ID
		}
		out = append(out, taskNode{id: ref.ID, name: name})
	}
	slices.SortFunc(out, func(x, y taskNode) int { return strings.Compare(x.name+"\x00"+x.id, y.name+"\x00"+y.id) })
	return out, nil
}
