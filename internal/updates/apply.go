// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// Applying an image update (plan 12-15, IMG-06). An update is a job: the digest is read from the registry at the moment of
// the click and never taken from the request, the environment's operation lock is held from before the first engine write
// until the run has ended, and the run is on record before the engine is touched, so an update that was interrupted is found
// again. A plain workload whose update fails after the engine was written is put back by TierPlan itself (plan 12-22, IMG-07,
// D-12): the run then ends rolled_back, or rollback_failed when the restore failed too, and the digest that failed is remembered.

// JobKind is the Kind of the job of an image update in the jobs hub.
const JobKind = "image_update"

// The keys of the job's Meta. None holds a credential.
const (
	MetaWorkload = "workload"
	MetaKind     = "kind"
	MetaRunID    = "runId"
	MetaBy       = "startedBy"
	MetaFrom     = "fromDigest"
	MetaTo       = "toDigest"
	MetaBulk     = "bulk"
	// MetaEnvName is the environment's name when the update started, for the header's list of jobs.
	MetaEnvName = "envName"
)

// The items of an update's job, in order (UI-SPEC "Steps (plain service)"). The health item exists only when the image
// defines a health check.
const (
	ItemCheck  = "check"
	ItemPull   = "pull"
	ItemUpdate = "update"
	ItemWait   = "wait"
	ItemHealth = "health"
	// ItemRollback and ItemRollbackWait are added to the job when an update failed and TierPlan puts the previous version back
	// (plan 12-22): the write that does it and the wait for the previous version to run.
	ItemRollback     = "rollback"
	ItemRollbackWait = "rollback_wait"
)

// The items of a plain container's update, in order after check and pull (UI-SPEC "Steps (plain container)"): the replacement is
// created beside the old container, the names are swapped, the replacement is started, waited for, and the old container removed.
// The health item exists only when the replacement has a health check, and the last item is added once the replacement starts.
const (
	ItemCreate    = "create"
	ItemSwap      = "swap"
	ItemStart     = "start"
	ItemRemoveOld = "remove_old"
)

// The reasons an update ends failed, in TierPlan's words. They are stored in the run and in the audit entry.
const (
	ReasonPullFailed     = "pull_failed"
	ReasonDidntStart     = "didnt_start"
	ReasonTimeout        = "timeout"
	ReasonUnhealthy      = "unhealthy"
	ReasonServiceChanged = "service_changed"
	ReasonRejected       = "service_rejected"
	ReasonEngine         = "engine_error"
	ReasonCancelled      = "cancelled"
	ReasonInterrupted    = "interrupted"
	// The reasons of a container's update (plan 12-20). create_refused: the engine would not create the replacement, nothing else
	// was done. agent_host_policy: the node's agent refused it because it gives the container host access. host_read_only: the
	// host's owner made the node read-only. container_changed: the container was changed or removed by someone else.
	// restore_failed: the replacement failed and the old container could not be put back.
	ReasonCreateRefused    = "create_refused"
	ReasonAgentHostPolicy  = "agent_host_policy"
	ReasonHostReadOnly     = "host_read_only"
	ReasonContainerChanged = "container_changed"
	ReasonRestoreFailed    = "restore_failed"
)

// The codes of a NotUpdatableError.
const (
	NotUpdatableAgent      = "agent"
	NotUpdatableJobMode    = "job_mode"
	NotUpdatableManaged    = "managed_by_stack"
	NotUpdatablePinned     = "pinned_in_file"
	NotUpdatableNoSource   = "not_from_registry"
	NotUpdatableNotChecked = "not_checked"
	// NotUpdatableComposeUnmanaged: the container belongs to a Compose project TierPlan does not manage, so a recreate would leave
	// its config hash stale; it is adopted first (D-21). NotUpdatableTaskContainer: a task container is updated through its service.
	NotUpdatableComposeUnmanaged = "compose_unmanaged"
	NotUpdatableTaskContainer    = "task_container"
)

// digestGrammar is a digest as the image reference grammar writes one. Nothing else is ever put in a reference.
var digestGrammar = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// recheckTimeout bounds the registry check made at click time.
const recheckTimeout = 45 * time.Second

// persistTimeout bounds a write of the run's end or its audit entry.
const persistTimeout = 15 * time.Second

var (
	// ErrNothingToUpdate means the workload already runs the digest the registry has.
	ErrNothingToUpdate = errors.New("updates: the workload already runs the newest version")
	// ErrWorkloadGone means the environment has no such service.
	ErrWorkloadGone = errors.New("updates: no such workload")
	// ErrUnsupportedKind means the kind of workload cannot be updated by this applier.
	ErrUnsupportedKind = errors.New("updates: this kind of workload cannot be updated")
)

// NotUpdatableError means the workload is one this path does not update, with the reason as a code.
type NotUpdatableError struct{ Code string }

func (e *NotUpdatableError) Error() string {
	return "updates: the workload cannot be updated: " + e.Code
}

// MovedItem is a workload whose newest version is not the one the person confirmed.
type MovedItem struct {
	Service, FromDigest, ToDigest string
}

// MovedError means the registry's digest moved since the person confirmed: nothing was changed, and Items carry the
// refreshed digests to confirm again (D-05).
type MovedError struct{ Items []MovedItem }

func (e *MovedError) Error() string {
	return "updates: the newest version moved since it was confirmed"
}

// LockedError means the environment's operation lock is held, and by whom. Nothing was started.
type LockedError struct{ Holder stackops.Holder }

func (e *LockedError) Error() string {
	return "updates: the environment's operation lock is held by " + e.Holder.Kind
}

// CheckFailedError means the registry could not be asked at click time, so no digest was verified.
type CheckFailedError struct{ Reason Reason }

func (e *CheckFailedError) Error() string {
	return "updates: the registry could not be checked before the update: " + string(e.Reason)
}

// Request is one update asked for.
type Request struct {
	EnvID, EnvName string
	// Kind is "service" or "container" here; a stack is updated through its own revision (plan 12-18).
	Kind string
	Name string
	// Expect maps a service name to the digest the confirm showed. A digest here is only compared with the one the
	// registry gives now; it is never deployed.
	Expect map[string]string
	// Actor is who asked, and IP and UserAgent are only recorded in the audit log.
	Actor     audit.Actor
	IP        string
	UserAgent string
	// Owner is who the job belongs to, as the hub keeps it ("user:<id>").
	Owner string
	// BulkID ties the updates of one bulk run together; empty for a single update.
	BulkID string
}

// Applier updates workloads to the newer image their check found.
type Applier struct {
	Service    *Service
	Runs       *RunStore
	Locks      stackops.OperationLock
	Jobs       *jobs.Hub
	Aggregator *envs.Aggregator
	Resolver   *registry.Resolver
	Audit      *audit.Writer
	Clock      stackops.Clock
	// ConvergeTimeout is how long an update waits for a service to settle. Zero is stackops.DefaultConvergeTimeout.
	ConvergeTimeout time.Duration
	// ManagedStack says whether the environment manages the stack of that ID. Nil means every service that carries a
	// TierPlan stack label is read as managed, which is the safe answer.
	ManagedStack func(ctx context.Context, envID, stackID string) (bool, error)
	// Failures remembers the digest an update failed with, per workload, and forgets it after a good update (IMG-07, D-14). Nil
	// means nothing is remembered.
	Failures *FailureStore
	// SelfContainerID is the ID of the container TierPlan's server runs in. That container is never updated from here: stopping it
	// would end the update. Empty when TierPlan cannot tell.
	SelfContainerID string
	Log             *slog.Logger
}

func (a *Applier) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *Applier) clock() stackops.Clock {
	if a.Clock != nil {
		return a.Clock
	}
	return stackops.RealClock()
}

func (a *Applier) convergeTimeout() time.Duration {
	if a.ConvergeTimeout > 0 {
		return a.ConvergeTimeout
	}
	return stackops.DefaultConvergeTimeout
}

// target is one service update, judged and ready: the digests, the image the service is given and what it is called.
type target struct {
	// kind is "service" or "container". nodeID is, for a container, the node it runs on, as the engine routes name it.
	kind, nodeID    string
	envID, name, id string
	// image is what the service follows, as the audit log and the run name it (repository:tag).
	image string
	// ref is the reference the service is written with after the update: its own repository and tag, pinned to to.
	ref string
	// pullRef is that reference as the engine pulls it.
	pullRef  string
	from, to string
}

// Start updates one workload. It judges the request against the live state and the registry (refusing with a
// *NotUpdatableError, ErrNothingToUpdate, a *MovedError or a *LockedError, all before anything is written), takes the
// environment's operation lock, records the run, and starts the job that does the update. The job owns the lock and frees it
// before it reads as finished. It answers the job's ID. jobs.ErrTooMany is passed on when the owner has the most jobs running.
func (a *Applier) Start(ctx context.Context, req Request) (string, error) {
	if a.Service == nil || a.Runs == nil || a.Locks == nil || a.Jobs == nil || a.Aggregator == nil {
		return "", errors.New("updates: the applier is not wired")
	}
	var subject subject
	switch req.Kind {
	case "service":
		row, found, err := a.serviceRow(ctx, req.EnvID, req.Name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrWorkloadGone
		}
		if err := a.updatable(ctx, req.EnvID, row); err != nil {
			return "", err
		}
		subject = serviceSubject(row)
	case "container":
		w, found, err := a.containerWorkload(ctx, req.EnvID, req.Name)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrWorkloadGone
		}
		if err := a.containerUpdatable(ctx, req.EnvID, w); err != nil {
			return "", err
		}
		subject = containerSubject(w)
	default:
		return "", ErrUnsupportedKind
	}
	// A held lock is answered before the registry is asked: a second update of a workload that is being updated must
	// read as locked, and not as already up to date because the first has written its spec.
	if h, held := a.Locks.Held(req.EnvID); held {
		return "", &LockedError{Holder: h}
	}

	t, err := a.recheck(ctx, req, subject)
	if err != nil {
		return "", err
	}

	release, held, ok := a.Locks.TryAcquire(req.EnvID, stackops.Holder{
		Kind: stackops.HolderKindImageUpdate, Workload: t.name, By: req.Actor.Name, Started: a.clock().Now().UTC(),
	})
	if !ok {
		return "", &LockedError{Holder: held}
	}
	freeLock := release

	rid, err := uuid.NewV7()
	if err != nil {
		freeLock()
		return "", fmt.Errorf("new run id: %w", err)
	}
	run := Run{
		ID: rid.String(), EnvID: req.EnvID, BulkID: req.BulkID, Kind: t.kind, Workload: t.name, WorkloadID: t.id,
		Items: []RunItem{{Service: t.name, Image: t.image, FromDigest: t.from, ToDigest: t.to}},
		State: RunRunning, Actor: actorKey(req.Actor), StartedAt: a.clock().Now().UTC(),
	}
	meta := map[string]string{
		MetaWorkload: t.name, MetaKind: t.kind, MetaRunID: run.ID, MetaBy: req.Actor.Name, MetaFrom: t.from, MetaTo: t.to,
		MetaEnvName: req.EnvName,
	}
	if req.BulkID != "" {
		meta[MetaBulk] = req.BulkID
	}

	// The job needs the run to be on record first, and the run records the job: the job waits on ready until the row is
	// written, and does nothing at all if the row could not be.
	ready := make(chan struct{})
	job, err := a.Jobs.Start(jobs.Spec{
		Kind: JobKind, EnvID: req.EnvID, Owner: req.Owner, Meta: meta, Items: a.itemSpecs(t),
	}, func(jctx context.Context, r *jobs.Reporter) {
		select {
		case <-ready:
		case <-jctx.Done():
			return
		}
		a.execute(jctx, r, req, run, t, freeLock)
	})
	if err != nil {
		freeLock()
		return "", err
	}
	a.Locks.Attach(req.EnvID, job.ID)
	run.JobID = job.ID
	if err := a.Runs.Insert(context.WithoutCancel(ctx), run); err != nil {
		_ = a.Jobs.Cancel(job.ID)
		freeLock()
		return "", err
	}
	close(ready)
	return job.ID, nil
}

// actorKey is what the run keeps of who started it.
func actorKey(a audit.Actor) string {
	if a.ID != "" {
		return a.ID
	}
	return a.Name
}

// serviceRow reads the service of that name from the cluster reader.
func (a *Applier) serviceRow(ctx context.Context, envID, name string) (dockerx.ServiceRow, bool, error) {
	if a.Service.Clusters == nil {
		return dockerx.ServiceRow{}, false, errors.New("updates: the applier has no cluster reader")
	}
	st, err := a.Service.Clusters.State(ctx, envID)
	if err != nil {
		return dockerx.ServiceRow{}, false, err
	}
	name = strings.TrimPrefix(name, "/")
	for _, r := range st.Read.Services {
		if r.Name == name {
			return r, true, nil
		}
	}
	return dockerx.ServiceRow{}, false, nil
}

// updatable refuses the services this path does not update: TierPlan's own agent, a job, and a service of a stack TierPlan
// manages, which is updated through a revision of that stack. A service of a stack that was only discovered is a plain one.
func (a *Applier) updatable(ctx context.Context, envID string, row dockerx.ServiceRow) error {
	switch {
	case row.Agent:
		return &NotUpdatableError{Code: NotUpdatableAgent}
	case row.Mode == "replicated-job" || row.Mode == "global-job":
		return &NotUpdatableError{Code: NotUpdatableJobMode}
	}
	id, err := a.managedStackID(ctx, envID, row)
	if err != nil {
		return err
	}
	if id != "" {
		return &NotUpdatableError{Code: NotUpdatableManaged}
	}
	return nil
}

// recheck asks the registry for the digest of the service's tag now, bypassing every cache and the recent-check rule, saves
// the answer, and judges the service by it. It refuses what has nothing to update, what moved since the confirm, and
// what cannot be checked.
func (a *Applier) recheck(ctx context.Context, req Request, sub subject) (target, error) {
	sv, found, err := a.Service.Find(ctx, req.EnvID, sub.kind, sub.name)
	if err != nil {
		return target{}, err
	}
	if !found {
		return target{}, ErrWorkloadGone
	}
	w := sv.Workload
	switch {
	case w.PinnedInFile():
		return target{}, &NotUpdatableError{Code: NotUpdatablePinned}
	case !w.Checkable():
		return target{}, &NotUpdatableError{Code: NotUpdatableNoSource}
	case a.Service.Checker == nil:
		return target{}, errors.New("updates: the applier has no checker")
	}
	cctx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	if _, err := a.Service.Checker.CheckEnv(cctx, req.EnvID, []Workload{w}); err != nil {
		if ctx.Err() != nil {
			return target{}, ctx.Err()
		}
		return target{}, fmt.Errorf("check the image of %s: %w", w.Name, err)
	}
	sv, found, err = a.Service.Find(ctx, req.EnvID, sub.kind, sub.name)
	if err != nil {
		return target{}, err
	}
	if !found {
		return target{}, ErrWorkloadGone
	}
	v := sv.Verdict
	remote := v.RemoteDigest
	switch v.State {
	case StateCheckFailed:
		return target{}, &CheckFailedError{Reason: v.Reason}
	case StateNotChecked:
		return target{}, &NotUpdatableError{Code: NotUpdatableNotChecked}
	case StatePinned:
		return target{}, &NotUpdatableError{Code: NotUpdatablePinned}
	case StateNotFromRegistry:
		return target{}, &NotUpdatableError{Code: NotUpdatableNoSource}
	case StateUpToDate, StateCheckStale:
		return target{}, ErrNothingToUpdate
	}
	if remote == "" || remote == v.RunningDigest {
		return target{}, ErrNothingToUpdate
	}
	if want, ok := req.Expect[sub.name]; ok && want != remote {
		return target{}, &MovedError{Items: []MovedItem{{Service: sub.name, FromDigest: v.RunningDigest, ToDigest: remote}}}
	}
	if !digestGrammar.MatchString(remote) {
		return target{}, fmt.Errorf("the registry answered %s with something that is not a sha256 digest", w.Ref.Repo())
	}
	t := target{
		kind: sub.kind, nodeID: sub.nodeID, envID: req.EnvID, name: sub.name, id: sub.id, image: imageOf(sv.Workload), from: v.RunningDigest, to: remote,
	}
	specImage := sub.specImage
	if sub.kind == "container" {
		specImage = sv.Workload.Written
	}
	t.ref = pinnedRef(specImage, sv.Workload.Ref, remote)
	t.pullRef = t.ref
	if parsed, perr := registry.ParseRef(t.ref); perr == nil {
		t.pullRef = parsed.String()
	}
	return t, nil
}

// imageOf is the image a workload follows, as the run and the audit log name it.
func imageOf(w Workload) string {
	if !w.Checkable() {
		return ""
	}
	return w.Ref.Repo() + ":" + w.Ref.Tag
}

// pinnedRef writes the reference a service is given: the image it has now with its digest replaced, so what the person
// wrote is kept, when that image is the repository and tag that were checked; else the checked repository, tag and digest.
// The repository is always the service's own and the digest the one the server read, never one from the request.
func pinnedRef(specImage string, followed registry.Ref, to string) string {
	base, _, _ := strings.Cut(specImage, "@")
	if cur, err := registry.ParseRef(base); err == nil && cur.Repo() == followed.Repo() && cur.Tag == followed.Tag {
		return base + "@" + to
	}
	return followed.Repo() + ":" + followed.Tag + "@" + to
}

// itemSpecs are the steps of the job.
func (a *Applier) itemSpecs(t target) []jobs.ItemSpec {
	if t.kind == "container" {
		return []jobs.ItemSpec{
			{ID: ItemCheck, Label: "Check the new version", Kind: stackops.KindStep},
			{ID: ItemPull, Label: "Pull the image", Kind: stackops.KindStep},
			{ID: ItemCreate, Label: "Create the new container", Kind: stackops.KindStep},
			{ID: ItemSwap, Label: "Stop and rename the old container", Kind: stackops.KindStep},
			{ID: ItemStart, Label: "Start the new container", Kind: stackops.KindStep},
		}
	}
	specs := []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the new version", Kind: stackops.KindStep},
		{ID: ItemPull, Label: "Pull the image", Kind: stackops.KindStep},
		{ID: ItemUpdate, Label: "Update " + t.name, Kind: stackops.KindStep},
		{ID: ItemWait, Label: "Wait for the new tasks", Kind: stackops.KindStep},
	}
	return specs
}

// endFunc ends the run of an update. reason is what the run keeps. cause is why the update failed, which differs from reason only
// when the restore failed too (the run then keeps restore_failed); for a run that did not fail it is empty.
type endFunc func(st RunState, reason, cause string)

// execute is the job of an update: it does the work of the kind, ends the run, writes the audit entry and frees the lock
// before the job's last step ends, so whoever sees the job finished can start the next operation at once.
func (a *Applier) execute(ctx context.Context, r *jobs.Reporter, req Request, run Run, t target, release func()) {
	var once sync.Once
	end := func(st RunState, reason, cause string) {
		once.Do(func() {
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
			defer cancel()
			if _, err := a.Runs.Finish(wctx, run.ID, st, reason, 0, 0); err != nil {
				a.log().Error("an image update run could not be finished on record", "run", run.ID, "error", err)
			}
			a.rememberFailure(wctx, run, t, st, cause)
			a.auditEnd(wctx, req, t, st, reason, cause)
			release()
		})
	}
	// Whatever ends the function without ending the run (a panic included) ends it failed, and frees the lock.
	defer end(RunFailed, ReasonInterrupted, ReasonInterrupted)

	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	r.Output("Checked " + t.image + ": " + shortDigest(t.from) + " to " + shortDigest(t.to))
	r.Finish(ItemCheck, jobs.ItemDone, "", "", map[string]string{"from": t.from, "to": t.to, "image": t.image})

	if t.kind == "container" {
		a.updateContainer(ctx, r, run, t, end)
		return
	}
	a.updateService(ctx, r, t, end)
}

// auditEnd writes the audit entries of an update that ended. The detail keys are TierPlan's own words and digests, never
// a credential. A good update is one image.updated entry; one that failed is an image.update_failed entry with the reason, and,
// when TierPlan put the previous version back or tried to, an image.update_rolled_back entry whose result says whether it worked.
func (a *Applier) auditEnd(ctx context.Context, req Request, t target, st RunState, reason, cause string) {
	if a.Audit == nil {
		return
	}
	entry := func(action string, result audit.Result, why, detail string) audit.Entry {
		e := audit.Entry{
			Actor: req.Actor, TargetType: t.kind, TargetID: t.id, EnvID: req.EnvID, IP: req.IP, UserAgent: req.UserAgent,
			Action: action, Result: result, Reason: why,
			Details: map[string]string{
				"kind": t.kind, "workload": t.name, "image": t.image, "from_digest": t.from, "to_digest": t.to,
			},
		}
		if detail != "" {
			e.Details["reason"] = detail
		}
		if req.BulkID != "" {
			e.Details["bulk"] = req.BulkID
		}
		return e
	}
	var entries []audit.Entry
	switch st {
	case RunDone:
		entries = append(entries, entry(audit.ActionImageUpdated, audit.ResultSuccess, "", ""))
	case RunRolledBack:
		entries = append(entries,
			entry(audit.ActionImageUpdateFailed, audit.ResultFailure, cause, cause),
			entry(audit.ActionImageUpdateRolledBack, audit.ResultSuccess, "", cause))
	case RunRollbackFailed:
		entries = append(entries,
			entry(audit.ActionImageUpdateFailed, audit.ResultFailure, cause, cause),
			entry(audit.ActionImageUpdateRolledBack, audit.ResultFailure, reason, cause))
	default:
		entries = append(entries, entry(audit.ActionImageUpdateFailed, audit.ResultFailure, reason, reason))
	}
	for _, e := range entries {
		if err := a.Audit.Record(ctx, e); err != nil {
			a.log().Error("an image update could not be written to the audit log", "environment", req.EnvID, "workload", t.name, "action", e.Action, "error", err)
		}
	}
}

// shortDigest is the first 12 characters of a digest's hash, for a sentence.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
