// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Updating many workloads of one environment in one action (plan 12-30, IMG-06, D-11). A bulk update is one job per environment. The
// job takes the environment's operation lock once, records every item as a queued run, and works through the items one at a time:
// each is judged again at its turn exactly as a single update is (the registry is asked, a digest that moved is refused), and one
// that fails after the engine was written is put back by the path it runs on while the job goes on with the next. The lock is freed
// after the last item. One bulk ID ties the audit entries of the whole selection together, and one image.bulk_updated entry per job
// says how it went.

// The reasons an item was not updated by a bulk update, as StartBulk and the job name them.
const (
	// BulkLocked: the environment's operation lock is held by someone else, so no item was started.
	BulkLocked = "locked"
	// BulkMoved: the registry's digest moved since the person confirmed.
	BulkMoved = "moved"
	// BulkNotUpdatable: the workload is one a bulk update does not update, or has nothing to update; Code says why.
	BulkNotUpdatable = "not_updatable"
	// BulkNoPermission: the item is a service of a stack TierPlan manages and the caller may not deploy stacks there.
	BulkNoPermission = "no_permission"
)

// The codes of a BulkSkip beyond those of a NotUpdatableError.
const (
	CodeWorkloadGone    = "workload_gone"
	CodeNothingToUpdate = "nothing_to_update"
	CodePreviouslyFail  = "previously_failed"
	CodeCheckFailed     = "check_failed"
	CodeNotRun          = "not_run"
	CodeUnsupported     = "unsupported_kind"
)

// auditTargetEnvironment is the audit target type of an entry about a whole environment.
const auditTargetEnvironment = "environment"

// ItemSummary is the ID of the last item of a bulk update's job: the summary, finished once the lock is free.
const ItemSummary = "summary"

// BulkItem is one workload chosen for a bulk update.
type BulkItem struct {
	// Kind is "service" or "container". A service or container of a stack TierPlan manages is updated as a revision of that stack.
	Kind, Name string
	// Expect maps a service name to the digest the confirm showed. It is only compared with the one the registry gives at the
	// item's turn; it is never deployed.
	Expect map[string]string
}

// BulkRequest asks for the update of several workloads of one environment.
type BulkRequest struct {
	EnvID, EnvName string
	// BulkID ties the audit entries of the whole selection together, across the environments it spans.
	BulkID string
	Items  []BulkItem
	// Actor is who asked; IP and UserAgent are only recorded in the audit log. Owner is who the job belongs to ("user:<id>").
	Actor     audit.Actor
	IP        string
	UserAgent string
	Owner     string
	// StackDeploy is true when the caller may deploy stacks in the environment, which updating a service of a stack TierPlan manages
	// is. Without it such an item is skipped no_permission.
	StackDeploy bool
	// StackItem deploys the share of the update that is services of a stack TierPlan manages (D-06): one revision of the stack with
	// those services' images changed, inside this job, under the lock the job already holds, reporting on r (a reporter scoped to the
	// item). It is the caller's, because it acts as the caller. It begins and ends the run it is given and answers nil when the
	// stack's update ended, whatever came of it, and a *BulkRefusal when it refused before it wrote anything. Nil means a stack's
	// services are skipped, not_updatable managed_by_stack.
	StackItem func(ctx context.Context, r *jobs.Reporter, in StackItemInput) error
}

// BulkSkip is an item StartBulk did not take, and why: Reason is BulkLocked, BulkMoved or BulkNotUpdatable, and Code is the
// NotUpdatableError code or one of the Code constants above.
type BulkSkip struct {
	Kind, Name, Reason, Code string
}

// StackItemInput is one stack's share of a bulk update, for BulkRequest.StackItem to deploy.
type StackItemInput struct {
	EnvID, EnvName string
	// StackID and Stack identify the stack TierPlan manages; Services are the selected services, by the name the stack's pins use.
	StackID, Stack string
	Services       []string
	// Expect is what the confirm showed, by service name.
	Expect map[string]string
	// Run is the run of this item. It is queued: the seam begins it (RunStore.Begin) with its items and the revision it wrote, and
	// ends it, as a stack's update does. It carries the bulk ID.
	Run Run
	// Actor, IP, UserAgent and BulkID are as in the BulkRequest.
	Actor     audit.Actor
	IP        string
	UserAgent string
	BulkID    string
}

// BulkRefusal is what BulkRequest.StackItem answers when it refused the item before it wrote anything: the item is then refused, and the
// job goes on. Any other error ends the item failed.
type BulkRefusal struct {
	// Reason is BulkLocked, BulkMoved or BulkNotUpdatable and Code says more.
	Reason, Code string
}

func (e *BulkRefusal) Error() string {
	return "updates: the item was refused: " + e.Reason + " " + e.Code
}

// bulkWork is one item of the job.
type bulkWork struct {
	// itemID is the ID of the job's item for it; the steps of its update are under that ID.
	itemID string
	req    Request
	// run is its run, queued from the start. weight is how many of the selected workloads it stands for.
	run    Run
	weight int
	// stack is set for the update of services of a stack TierPlan manages, which is deployed as a revision of the stack.
	stack *StackItemInput
}

// bulkCounts add up how the items went.
type bulkCounts struct {
	done, rolledBack, failed, skipped, total int
}

// StartBulk starts the update of the items of one environment as one job. It reads each item (an item that is gone, that is a kind
// a bulk update does not update, or that is one of an agent, a job or a Compose project TierPlan does not manage, is skipped and
// named), takes the environment's operation lock (when it is held, every item is skipped locked and nothing is started), records
// each remaining item as a queued run and starts the job that updates them in order. It answers the job's ID, empty when no job was
// started, and the items it skipped. The registry is not asked here: each item is judged again at its turn, in the job, where a
// digest that moved or a version that failed before skips the item and is named in the job and in the summary. jobs.ErrTooMany is
// passed on when the owner has the most jobs running.
func (a *Applier) StartBulk(ctx context.Context, req BulkRequest) (string, []BulkSkip, error) {
	if !a.wired() {
		return "", nil, errors.New("updates: the applier is not wired")
	}
	works, skipped, err := a.bulkWorks(ctx, req)
	if err != nil {
		return "", nil, err
	}
	if len(works) == 0 {
		return "", skipped, nil
	}
	label := works[0].run.Workload
	if len(works) > 1 {
		label = strconv.Itoa(len(works)) + " workloads"
	}
	release, _, ok := a.Locks.TryAcquire(req.EnvID, stackops.Holder{
		Kind: stackops.HolderKindImageUpdate, Workload: label, By: req.Actor.Name, Started: a.clock().Now().UTC(),
	})
	if !ok {
		for _, w := range works {
			skipped = append(skipped, BulkSkip{Kind: w.req.Kind, Name: w.req.Name, Reason: BulkLocked})
		}
		return "", skipped, nil
	}

	items := make([]jobs.ItemSpec, 0, len(works)+1)
	for _, w := range works {
		items = append(items, jobs.ItemSpec{ID: w.itemID, Label: "Update " + w.run.Workload, Kind: w.run.Kind})
	}
	items = append(items, jobs.ItemSpec{ID: ItemSummary, Label: "Summary", Kind: stackops.KindStep})
	meta := map[string]string{
		MetaWorkload: label, MetaKind: "bulk", MetaBy: req.Actor.Name, MetaBulk: req.BulkID, MetaEnvName: req.EnvName,
	}

	// The job needs the runs to be on record first, and each run records the job: the job waits on ready until the rows are written.
	ready := make(chan struct{})
	failed := false
	total := len(skipped)
	for _, w := range works {
		total += w.weight
	}
	job, err := a.Jobs.Start(jobs.Spec{
		Kind: JobKind, EnvID: req.EnvID, Owner: req.Owner, Meta: meta, Items: items,
	}, func(jctx context.Context, r *jobs.Reporter) {
		defer release()
		select {
		case <-ready:
		case <-jctx.Done():
			a.refuseQueued(jctx, works)
			return
		}
		if failed {
			return
		}
		a.runBulk(jctx, r, req, works, len(skipped), total, release)
	})
	if err != nil {
		release()
		return "", nil, err
	}
	a.Locks.Attach(req.EnvID, job.ID)
	started := a.clock().Now().UTC()
	for i := range works {
		works[i].run.JobID, works[i].run.StartedAt = job.ID, started
		if err := a.Runs.Insert(context.WithoutCancel(ctx), works[i].run); err != nil {
			failed = true
			_ = a.Jobs.Cancel(job.ID)
			a.refuseQueued(context.WithoutCancel(ctx), works[:i])
			close(ready)
			return "", nil, err
		}
	}
	close(ready)
	return job.ID, skipped, nil
}

// bulkWorks reads the items of a request and makes the work of those that can be updated, each with its queued run. An item that
// cannot be is a BulkSkip. Items that name the same workload are one. A read that fails fails the request.
func (a *Applier) bulkWorks(ctx context.Context, req BulkRequest) ([]*bulkWork, []BulkSkip, error) {
	var (
		works   []*bulkWork
		skipped []BulkSkip
		seen    = map[string]bool{}
		stackOf = map[string]*bulkWork{}
	)
	// The workloads are read once for the whole request.
	var all []Workload
	var haveAll bool
	workloads := func() ([]Workload, error) {
		if !haveAll {
			var err error
			if all, err = a.Service.Workloads(ctx, req.EnvID); err != nil {
				return nil, err
			}
			haveAll = true
		}
		return all, nil
	}
	skip := func(it BulkItem, code string) {
		skipped = append(skipped, BulkSkip{Kind: it.Kind, Name: it.Name, Reason: BulkNotUpdatable, Code: code})
	}
	deny := func(it BulkItem, reason, code string) {
		skipped = append(skipped, BulkSkip{Kind: it.Kind, Name: it.Name, Reason: reason, Code: code})
	}
	newRun := func(kind, workload, stackID string) (Run, error) {
		id, err := uuid.NewV7()
		if err != nil {
			return Run{}, fmt.Errorf("new run id: %w", err)
		}
		return Run{
			ID: id.String(), EnvID: req.EnvID, BulkID: req.BulkID, Kind: kind, Workload: workload, StackID: stackID,
			State: RunQueued, Actor: actorKey(req.Actor),
		}, nil
	}
	for _, it := range req.Items {
		it.Name = strings.TrimPrefix(it.Name, "/")
		key := it.Kind + "/" + it.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		var (
			stackID, stackName, service string
			workloadName                string
		)
		switch it.Kind {
		case "service":
			row, found, err := a.serviceRow(ctx, req.EnvID, it.Name)
			if err != nil {
				return nil, nil, err
			}
			if !found {
				skip(it, CodeWorkloadGone)
				continue
			}
			if stackID, err = a.managedStackID(ctx, req.EnvID, row); err != nil {
				return nil, nil, err
			}
			if stackID == "" {
				if err := a.updatable(ctx, req.EnvID, row); err != nil {
					skip(it, codeOfRefusal(err))
					continue
				}
			}
			stackName, service, workloadName = row.Stack, it.Name, row.Name
		case "container":
			ws, err := workloads()
			if err != nil {
				return nil, nil, err
			}
			w, found, err := containerNamed(ws, it.Name)
			var nu *NotUpdatableError
			if errors.As(err, &nu) {
				// The name is on more than one node: this item is refused and the others go on.
				skip(it, nu.Code)
				continue
			} else if err != nil {
				return nil, nil, err
			}
			if !found {
				skip(it, CodeWorkloadGone)
				continue
			}
			if w.Labels[stacks.LabelComposeProject] != "" {
				if stackID, err = a.managedStackIDOf(ctx, req.EnvID, w.Name, w.Labels); err != nil {
					return nil, nil, err
				}
			}
			if stackID != "" && w.Labels[stacks.LabelComposeService] != "" {
				stackName, service, workloadName = w.Stack, w.Labels[stacks.LabelComposeService], it.Name
			} else if err := a.containerUpdatable(ctx, req.EnvID, w); err != nil {
				skip(it, codeOfRefusal(err))
				continue
			} else {
				stackID, workloadName = "", it.Name
			}
		default:
			skip(it, CodeUnsupported)
			continue
		}

		if stackID != "" {
			switch {
			case !req.StackDeploy:
				deny(it, BulkNoPermission, NotUpdatableManaged)
				continue
			case req.StackItem == nil:
				skip(it, NotUpdatableManaged)
				continue
			}
			// Services of one stack are one revision of it, and one item of the job.
			w := stackOf[stackID]
			if w == nil {
				run, err := newRun("stack", stackName, stackID)
				if err != nil {
					return nil, nil, err
				}
				run.WorkloadID = stackID
				w = &bulkWork{
					run: run,
					stack: &StackItemInput{
						EnvID: req.EnvID, EnvName: req.EnvName, StackID: stackID, Stack: stackName, Expect: map[string]string{},
						Actor: req.Actor, IP: req.IP, UserAgent: req.UserAgent, BulkID: req.BulkID,
					},
				}
				stackOf[stackID] = w
				works = append(works, w)
			}
			w.weight++
			w.stack.Services = append(w.stack.Services, service)
			for k, v := range it.Expect {
				w.stack.Expect[k] = v
			}
			if it.Kind == "container" {
				if want, ok := it.Expect[it.Name]; ok {
					w.stack.Expect[service] = want
				}
			}
			continue
		}
		run, err := newRun(it.Kind, workloadName, "")
		if err != nil {
			return nil, nil, err
		}
		works = append(works, &bulkWork{
			weight: 1, run: run,
			req: Request{
				EnvID: req.EnvID, EnvName: req.EnvName, Kind: it.Kind, Name: it.Name, Expect: it.Expect,
				Actor: req.Actor, IP: req.IP, UserAgent: req.UserAgent, Owner: req.Owner, BulkID: req.BulkID,
			},
		})
	}
	for i, w := range works {
		w.itemID = "i" + strconv.Itoa(i)
		if w.stack != nil {
			slices.Sort(w.stack.Services)
			w.stack.Services = slices.Compact(w.stack.Services)
			w.req = Request{
				EnvID: req.EnvID, EnvName: req.EnvName, Kind: "stack", Name: w.run.Workload, Expect: w.stack.Expect,
				Actor: req.Actor, IP: req.IP, UserAgent: req.UserAgent, Owner: req.Owner, BulkID: req.BulkID,
			}
		}
	}
	return works, skipped, nil
}

// codeOfRefusal is the code a NotUpdatableError carries, or the reason an error stands for.
func codeOfRefusal(err error) string {
	var nu *NotUpdatableError
	switch {
	case errors.As(err, &nu):
		return nu.Code
	case errors.Is(err, ErrWorkloadGone):
		return CodeWorkloadGone
	}
	return CodeNotRun
}

// refusalOf says why an item was not updated at its turn: the reason (BulkMoved, BulkLocked or BulkNotUpdatable) and a code. ok is
// false for an error that is not a refusal, one of the engine or the registry that left the item undone.
func refusalOf(err error) (reason, code string, ok bool) {
	var (
		moved    *MovedError
		locked   *LockedError
		refusal  *BulkRefusal
		nu       *NotUpdatableError
		checkErr *CheckFailedError
	)
	switch {
	case errors.As(err, &refusal):
		return refusal.Reason, refusal.Code, true
	case errors.As(err, &moved):
		return BulkMoved, BulkMoved, true
	case errors.As(err, &locked):
		return BulkLocked, BulkLocked, true
	case errors.As(err, &nu):
		return BulkNotUpdatable, nu.Code, true
	case errors.Is(err, ErrNothingToUpdate):
		return BulkNotUpdatable, CodeNothingToUpdate, true
	case errors.Is(err, ErrPreviouslyFailed):
		return BulkNotUpdatable, CodePreviouslyFail, true
	case errors.As(err, &checkErr):
		return BulkNotUpdatable, CodeCheckFailed, true
	case errors.Is(err, ErrWorkloadGone):
		return BulkNotUpdatable, CodeWorkloadGone, true
	}
	return "", "", false
}

// itemSentences are the words of the codes an item of a bulk update was skipped for.
var itemSentences = map[string]string{
	BulkMoved:                    "A newer version appeared since you confirmed, so this was skipped.",
	BulkLocked:                   "Another operation holds the environment, so this was skipped.",
	CodeNothingToUpdate:          "It already runs the newest version.",
	CodePreviouslyFail:           "An update to this version failed before, so this was skipped.",
	CodeCheckFailed:              "The registry could not be asked for the newest version, so nothing was changed.",
	CodeWorkloadGone:             "It is not there any more.",
	ReasonCancelled:              "The bulk update was cancelled before this was started.",
	NotUpdatableManaged:          "It belongs to a stack, which is updated through a new revision of the stack.",
	NotUpdatableComposeUnmanaged: "It belongs to a Compose project TierPlan does not manage. Adopt the stack first.",
	NotUpdatablePinned:           "Its image is pinned to a digest in its file.",
	NotUpdatableNotChecked:       "Its image has not been checked yet.",
	NotUpdatableAmbiguousName:    "More than one node has a container of this name, so TierPlan cannot tell which to update. Rename one of them.",
}

// refuse ends the run of an item that was not updated, without anything having been written, and returns the sentence for the job.
func (a *Applier) refuse(ctx context.Context, w *bulkWork, code string) string {
	wctx := context.WithoutCancel(ctx)
	if _, err := a.Runs.Finish(wctx, w.run.ID, RunRefused, code, 0, 0); err != nil {
		a.log().Error("a refused image update could not be finished on record", "run", w.run.ID, "error", err)
	}
	msg := itemSentences[code]
	if msg == "" {
		msg = "This was not updated, and nothing was changed."
	}
	return msg
}

// refuseQueued ends the runs of the items that never started, as refused because the bulk update did not go on.
func (a *Applier) refuseQueued(ctx context.Context, works []*bulkWork) {
	for _, w := range works {
		a.refuse(ctx, w, ReasonCancelled)
	}
}

// runBulk is the job of a bulk update: the items in order, then the summary, with the lock freed before the summary ends.
func (a *Applier) runBulk(ctx context.Context, r *jobs.Reporter, req BulkRequest, works []*bulkWork, alreadySkipped, total int, release func()) {
	c := bulkCounts{total: total, skipped: alreadySkipped}
	// Whatever ends the function early (a panic included) ends the runs that did not start, and the lock is freed.
	defer func() {
		a.refuseQueuedStill(ctx, works)
		release()
	}()
	for _, w := range works {
		a.bulkItem(ctx, r, req, w, &c)
	}
	summary := fmt.Sprintf("Updated %d of %d.", c.done, c.total)
	if c.rolledBack > 0 {
		summary += fmt.Sprintf(" %d rolled back.", c.rolledBack)
	}
	if c.failed > 0 {
		summary += fmt.Sprintf(" %d failed.", c.failed)
	}
	if c.skipped > 0 {
		summary += fmt.Sprintf(" %d skipped.", c.skipped)
	}
	a.auditBulk(ctx, req, c)
	r.Output(summary)
	// The lock is free before the job reads as finished, so whoever sees it end can start the next operation at once.
	release()
	r.Finish(ItemSummary, jobs.ItemDone, "", "", map[string]string{
		"done": strconv.Itoa(c.done), "total": strconv.Itoa(c.total), "rolled_back": strconv.Itoa(c.rolledBack), "skipped": strconv.Itoa(c.skipped),
	})
}

// refuseQueuedStill ends the runs of the items whose run is still queued (Finish writes once, so an item that ended is untouched).
func (a *Applier) refuseQueuedStill(ctx context.Context, works []*bulkWork) {
	if jobs.Closing(ctx) {
		// The server is stopping: the runs stay as they are and restart recovery ends each by what it finds (CR-01).
		return
	}
	wctx := context.WithoutCancel(ctx)
	for _, w := range works {
		if _, err := a.Runs.Finish(wctx, w.run.ID, RunFailed, ReasonInterrupted, 0, 0); err != nil {
			a.log().Error("an image update of a bulk run could not be finished on record", "run", w.run.ID, "error", err)
		}
	}
}

// bulkItem does one item at its turn.
func (a *Applier) bulkItem(ctx context.Context, r *jobs.Reporter, req BulkRequest, w *bulkWork, c *bulkCounts) {
	skipItem := func(state, code string) {
		msg := a.refuse(ctx, w, code)
		r.Finish(w.itemID, state, code, msg, nil)
		c.skipped += w.weight
	}
	if ctx.Err() != nil {
		skipItem(jobs.ItemCancelled, ReasonCancelled)
		return
	}
	r.Update(w.itemID, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	steps := r.Scoped(w.itemID+"/", w.itemID)

	if w.stack != nil {
		a.bulkStackItem(ctx, r, steps, req, w, c, skipItem)
		return
	}
	t, err := a.prepare(WithoutLiveState(ctx), w.req, true)
	if err != nil {
		if _, code, ok := refusalOf(err); ok {
			skipItem(jobs.ItemSkipped, code)
			return
		}
		if ctx.Err() != nil {
			skipItem(jobs.ItemCancelled, ReasonCancelled)
			return
		}
		a.log().Warn("an item of a bulk image update could not be judged", "environment", w.req.EnvID, "workload", w.req.Name, "error", err)
		skipItem(jobs.ItemSkipped, ReasonEngine)
		return
	}
	run := w.run
	run.Workload, run.WorkloadID = t.name, t.id
	run.Items = []RunItem{{Service: t.name, Image: t.image, FromDigest: t.from, ToDigest: t.to}}
	run.StartedAt = a.clock().Now().UTC()
	begun, err := a.Runs.Begin(context.WithoutCancel(ctx), run)
	if err != nil || !begun {
		a.log().Error("an item of a bulk image update could not be started on record", "run", run.ID, "begun", begun, "error", err)
		skipItem(jobs.ItemFailed, ReasonEngine)
		return
	}
	for _, spec := range a.itemSpecs(t) {
		steps.Add(spec)
	}
	// The lock is the bulk update's: the item frees nothing, and its run ends before the next item starts.
	a.runOne(ctx, steps, w.req, run, t, func() {})
	a.countEnd(ctx, r, w, c)
}

// bulkStackItem does the item of a stack: one revision with the selected services' images changed, deployed inside this job.
func (a *Applier) bulkStackItem(ctx context.Context, r, steps *jobs.Reporter, req BulkRequest, w *bulkWork, c *bulkCounts, skipItem func(state, code string)) {
	in := *w.stack
	in.Run = w.run
	in.Run.StartedAt = a.clock().Now().UTC()
	if err := req.StackItem(WithoutLiveState(ctx), steps, in); err != nil {
		if _, code, ok := refusalOf(err); ok {
			skipItem(jobs.ItemSkipped, code)
			return
		}
		if ctx.Err() != nil {
			skipItem(jobs.ItemCancelled, ReasonCancelled)
			return
		}
		a.log().Warn("an item of a bulk image update could not be deployed", "environment", w.req.EnvID, "stack", w.run.Workload, "error", err)
		// A run the seam did not begin never started: it is refused. One it began and did not end failed.
		if got, gerr := a.Runs.Get(context.WithoutCancel(ctx), w.run.ID); gerr == nil && got.State == RunRunning {
			_, _ = a.Runs.Finish(context.WithoutCancel(ctx), w.run.ID, RunFailed, ReasonEngine, 0, 0)
			c.failed += w.weight
			r.Finish(w.itemID, jobs.ItemFailed, ReasonEngine, itemMessages[ReasonEngine], nil)
			return
		}
		skipItem(jobs.ItemFailed, ReasonEngine)
		return
	}
	a.countEnd(ctx, r, w, c)
}

// countEnd counts how an item ended, by the run it left, and finishes its item in the job.
func (a *Applier) countEnd(ctx context.Context, r *jobs.Reporter, w *bulkWork, c *bulkCounts) {
	got, err := a.Runs.Get(context.WithoutCancel(ctx), w.run.ID)
	state, reason := RunFailed, ReasonInterrupted
	if err == nil {
		state, reason = got.State, got.Reason
	} else {
		a.log().Error("the end of an item of a bulk image update could not be read", "run", w.run.ID, "error", err)
	}
	switch state {
	case RunDone:
		c.done += w.weight
		r.Finish(w.itemID, jobs.ItemDone, "", "", nil)
	case RunRolledBack, RunRollbackFailed:
		c.rolledBack += w.weight
		r.Finish(w.itemID, jobs.ItemFailed, reason, firstText(itemMessages[reason], "The update failed and the previous version was put back."), nil)
	default:
		// A run that has not ended is ended now, so nothing is left queued or running behind a finished item.
		if state == RunQueued || state == RunRunning {
			// When the server is stopping the row stays as it is for restart recovery (CR-01); the item still reads as interrupted.
			if !jobs.Closing(ctx) {
				_, _ = a.Runs.Finish(context.WithoutCancel(ctx), w.run.ID, RunFailed, ReasonInterrupted, 0, 0)
			}
			reason = ReasonInterrupted
		}
		if state == RunRefused {
			c.skipped += w.weight
		} else {
			c.failed += w.weight
		}
		r.Finish(w.itemID, jobs.ItemFailed, reason, itemMessages[reason], nil)
	}
}

// auditBulk writes the summary entry of a bulk update's part in one environment.
func (a *Applier) auditBulk(ctx context.Context, req BulkRequest, c bulkCounts) {
	if a.Audit == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	e := audit.Entry{
		Actor: req.Actor, TargetType: auditTargetEnvironment, TargetID: req.EnvID, EnvID: req.EnvID, IP: req.IP, UserAgent: req.UserAgent,
		Action: audit.ActionImageBulkUpdated, Result: audit.ResultSuccess,
		Details: map[string]string{
			"bulk": req.BulkID, "done": strconv.Itoa(c.done), "total": strconv.Itoa(c.total),
			"rolled_back": strconv.Itoa(c.rolledBack), "skipped": strconv.Itoa(c.skipped),
		},
	}
	if c.done < c.total {
		e.Result, e.Reason = audit.ResultFailure, "some_not_updated"
	}
	if err := a.Audit.Record(wctx, e); err != nil {
		a.log().Error("a bulk image update could not be written to the audit log", "environment", req.EnvID, "error", err)
	}
}
