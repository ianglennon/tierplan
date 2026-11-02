// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

// The modes of a stop and a start: the Mode of the job and the kind of the row in stack_deploys.
const (
	LifecycleStop  = DeployKindStop
	LifecycleStart = DeployKindStart
)

// CodeNoRevision ends the check of a start for a stack that has never been deployed: there is no last
// version to bring back.
const CodeNoRevision = "no_revision"

func init() {
	codeMessages[CodeNoRevision] = "There's no deployed version to bring back."
}

// LifecycleStore is the stored side of a stop and a start: the stack's own record that it is stopped, and the
// last version that ran well, whose project a start brings back. Repo is the one implementation.
type LifecycleStore interface {
	// SetStopped records that the stack is stopped (true) or running again (false).
	SetStopped(ctx context.Context, stackID string, stopped bool) error
	// LatestGoodRevision is the newest revision whose deploy succeeded, with its content opened.
	LatestGoodRevision(ctx context.Context, stackID string) (Snapshot, bool, error)
	// RevisionPins are the images a revision ran, by service name.
	RevisionPins(ctx context.Context, snapshotID string) (map[string]string, error)
}

var _ LifecycleStore = Repo{}

// LifecycleInput is a stack to stop or to start: the environment, the node of a standalone host, the stack,
// who asks and the store the last good revision is read from.
type LifecycleInput struct {
	EnvID string
	// NodeID is the node of a standalone host the stack runs on (the environment's one node). A swarm stack
	// leaves it empty: its writes go through a manager.
	NodeID string
	Stack  Stack
	Caller Caller
	Store  LifecycleStore
	// Masker hides the stack's secret values in every text a remote system sent that the job keeps.
	Masker stacks.Masker
	// OnDone, when set, is called once with the result before the job's last item is finished, so a job that
	// reads as finished has already been recorded by the caller.
	OnDone func(LifecycleResult)
}

// LifecycleResult is what a stop or a start did, as far as it got.
type LifecycleResult struct {
	// Scaled names the replicated services a stop scaled to 0 and a start scaled back, Removed the global and
	// job services a stop removed, Recreated the services a start made again from the last good revision.
	Scaled, Removed, Recreated []string
	// Containers counts the services of a standalone stack the Compose SDK stopped or started.
	Containers int
	// FailedService names the service the failure is about; Code is empty when the operation worked and
	// CodeCancelled when it was cancelled.
	FailedService, Code string
}

// OK reports whether the operation ended without a failure.
func (r LifecycleResult) OK() bool { return r.Code == "" }

// Services is how many services the operation touched.
func (r LifecycleResult) Services() int {
	return len(r.Scaled) + len(r.Removed) + len(r.Recreated) + r.Containers
}

// LifecycleItemSpecs are the items a stop or a start job starts with, in run order. The service rows are added
// while the job runs, once the stack's services are known.
func LifecycleItemSpecs(mode string, target stacks.Target) []jobs.ItemSpec {
	noun, verb := "services", "Stop"
	if target == stacks.TargetStandalone {
		noun = "containers"
	}
	if mode == LifecycleStart {
		verb = "Start"
	}
	return []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the stack", Kind: KindStep},
		{ID: ItemServices, Label: verb + " " + noun, Kind: KindStep},
	}
}

// lifecycle is one stop or start in progress: it reports on the job and remembers how the last item will end,
// which is only after the caller has recorded the result.
type lifecycle struct {
	d      Deps
	in     LifecycleInput
	r      *jobs.Reporter
	prog   *progress
	final  itemEnd
	detail string
	res    LifecycleResult
}

func (d Deps) newLifecycle(in LifecycleInput, r *jobs.Reporter) *lifecycle {
	return &lifecycle{d: d, in: in, r: r, prog: newProgress(r, in.Masker), final: itemEnd{state: jobs.ItemDone}}
}

// end tells the caller the result and finishes the last item.
func (l *lifecycle) end() {
	if l.in.OnDone != nil {
		l.in.OnDone(l.res)
	}
	l.r.Finish(ItemServices, l.final.state, l.final.code, l.final.message, l.final.result)
}

// failWith ends the item that failed with its code and the one after it as not run. The row of the service the
// failure is about ends with the engine's words; with none to name, the step does.
func (l *lifecycle) failWith(item, code, name, message string) {
	if message == "" {
		message = CodeMessage(code)
	}
	l.res.Code, l.res.FailedService = code, name
	state := jobs.ItemFailed
	cancelled := code == CodeCancelled
	if cancelled {
		state = jobs.ItemCancelled
		l.detail = ""
	}
	matched := item == ItemServices && l.prog.failed(item, name, state, code, message, l.detail)
	if !matched && l.detail != "" {
		l.prog.detail(item, l.detail)
		l.prog.line("Failed: " + l.detail)
	}
	later := itemEnd{state: jobs.ItemSkipped, code: CodeNotRun, message: CodeMessage(CodeNotRun)}
	if cancelled {
		later.state = jobs.ItemCancelled
	}
	if item == ItemCheck {
		l.r.Finish(ItemCheck, state, code, message, nil)
		l.final = later
	} else {
		l.final = itemEnd{state: state, code: code, message: message}
	}
	l.prog.rest(later.state, later.code, later.message)
}

func (l *lifecycle) fail(item, code, name string) { l.failWith(item, code, name, "") }

// failErr fails an item whose cause is an error from the engine, whose words the item keeps.
func (l *lifecycle) failErr(item, code, name string, err error) {
	l.detail = engineWords(err)
	l.fail(item, code, name)
}

// failStep ends the item that failed with the code and name a step reported.
func (l *lifecycle) failStep(ctx context.Context, item string, err error) {
	code, name := stepOf(ctx, err)
	l.failErr(item, code, name, err)
}

// Stop parks a stack without losing anything it holds (STK-11, D-19): on a swarm every replicated service is
// scaled to 0 with its count remembered on the service itself, in the label tierplan.stack.stopped-replicas, and
// the global and job services are removed (Start makes them again from the last good revision); on a standalone
// host the Compose SDK stops the containers. Networks, volumes, secrets and configs are never touched. The stack
// is marked stopped once everything has stopped. A stack whose services were stopped before, or only in part, is
// stopped from there: a service already stopped keeps the count it remembered.
//
// Every write goes through the aggregator once and is never retried (P2 D-42); a failure ends the operation at
// the service it was about and what already stopped stays stopped. The last item is finished only after
// in.OnDone returns.
func (d Deps) Stop(ctx context.Context, in LifecycleInput, r *jobs.Reporter) LifecycleResult {
	l := d.newLifecycle(in, r)
	defer l.end()
	if in.Stack.Target == stacks.TargetStandalone {
		l.stopStandalone(ctx)
	} else {
		l.stopSwarm(ctx)
	}
	return l.res
}

// Start brings a stopped stack back as it was (STK-11, D-19): on a swarm every service that remembers a count is
// scaled to it and the label removed, and a service of the last good revision that has none (a global or job
// service Stop removed) is created again from that revision with the images it ran, never the draft and never
// a newer digest; on a standalone host the Compose SDK starts the stopped containers and puts a stack file's
// secrets and configs in them again. A service that has to be created is judged by the host-access policy for
// the caller again, just as a deploy is. The stack is marked running once everything is.
func (d Deps) Start(ctx context.Context, in LifecycleInput, r *jobs.Reporter) LifecycleResult {
	l := d.newLifecycle(in, r)
	defer l.end()
	if in.Stack.Target == stacks.TargetStandalone {
		l.startStandalone(ctx)
	} else {
		l.startSwarm(ctx)
	}
	return l.res
}

// Workload counts what a stack holds on its target that a stop would act on: its services on a swarm, its
// containers (stopped ones too) on a standalone host. Zero is a stack with nothing to stop.
func (d Deps) Workload(ctx context.Context, envID, nodeID string, st Stack) (int, error) {
	if st.Target == stacks.TargetStandalone {
		live, err := d.readStandalone(ctx, envID, nodeID, st.Name)
		return len(live.Containers), err
	}
	svcs, err := d.ownedServices(ctx, envID, st)
	return len(svcs), err
}

// StartCheck is what a request for a start finds out before it takes the lock: whether the start has to create
// services again, and whether the caller may have them created.
type StartCheck struct {
	// Recreates is true when the last good revision declares a service the swarm no longer has, which a start
	// of a swarm stack creates again from that revision.
	Recreates bool
	// Policy is the host-access judgement of that revision for the caller, made only when services are
	// created: the job judges it again under the lock.
	Policy PolicyResult
}

// CheckStart reads what a start of the stack would have to do. A standalone stack starts its stopped
// containers and creates nothing. A revision that cannot be redeployed as it stands is a *PrepareError.
func (d Deps) CheckStart(ctx context.Context, envID string, st Stack, store LifecycleStore, caller Caller) (StartCheck, error) {
	if st.Target == stacks.TargetStandalone {
		return StartCheck{}, nil
	}
	live, err := d.ownedServices(ctx, envID, st)
	if err != nil {
		return StartCheck{}, err
	}
	plan, err := d.planStart(ctx, st, store, live)
	if err != nil {
		return StartCheck{}, err
	}
	if len(plan.missing) == 0 {
		return StartCheck{}, nil
	}
	pol, err := d.Policy(ctx, caller, envID, plan.rev.Copy, plan.prep.Plan)
	return StartCheck{Recreates: true, Policy: pol}, err
}

// ownedServices are the services of the swarm that the stack owns (OwnershipOf), by name: those that carry its
// namespace and its ID, and, until an adopted stack's first deploy, those that carry no ID or the one recorded at
// adoption. A service of the namespace that another owner made is not the stack's and is never touched.
func (d Deps) ownedServices(ctx context.Context, envID string, st Stack) ([]dockerx.ServiceDetail, error) {
	var all []dockerx.ServiceDetail
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		all, e = c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, st.Name)
		return e
	})
	if err != nil {
		return nil, err
	}
	own := OwnershipOf(st)
	out := make([]dockerx.ServiceDetail, 0, len(all))
	for _, s := range all {
		if own.OwnsWorkload(s.Labels) {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b dockerx.ServiceDetail) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// shortName is a service's name without the stack's prefix, as the console shows it.
func shortName(stack, name string) string { return strings.TrimPrefix(name, stack+"_") }

// addRow puts the row of a service under the services step.
func (l *lifecycle) addRow(name string) {
	l.prog.add(jobs.ItemSpec{ID: idService + name, Label: shortName(l.in.Stack.Name, name), Parent: ItemServices, Kind: KindService}, name, "service")
}

// replicasSpec is the service spec with its replica count set to replicas and the stopped-replicas label set to
// label, or removed when label is empty. Nothing else changes, so the swarm replaces no task: a label of the
// service itself is not part of the task template.
func replicasSpec(spec json.RawMessage, replicas uint64, label string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(spec, &top); err != nil {
		return nil, err
	}
	labels := map[string]string{}
	if raw, ok := top["Labels"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &labels); err != nil {
			return nil, err
		}
	}
	if label == "" {
		delete(labels, stacks.LabelStoppedReplicas)
	} else {
		labels[stacks.LabelStoppedReplicas] = label
	}
	if len(labels) == 0 {
		delete(top, "Labels")
	} else {
		raw, err := json.Marshal(labels)
		if err != nil {
			return nil, err
		}
		top["Labels"] = raw
	}
	mode := map[string]json.RawMessage{}
	if raw, ok := top["Mode"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &mode); err != nil {
			return nil, err
		}
	}
	rep, err := json.Marshal(map[string]uint64{"Replicas": replicas})
	if err != nil {
		return nil, err
	}
	mode["Replicated"] = rep
	rawMode, err := json.Marshal(mode)
	if err != nil {
		return nil, err
	}
	top["Mode"] = rawMode
	return json.Marshal(top)
}

// imageAsWritten is the image of a service as the stack's file wrote it, which is what its registry credential
// is chosen by.
func imageAsWritten(s dockerx.ServiceDetail) string {
	if v := s.Labels[stacks.LabelStackImage]; v != "" {
		return v
	}
	if v := s.Labels[stacks.LabelSwarmImage]; v != "" {
		return v
	}
	return s.Image
}

// stopSwarm scales the replicated services of a swarm stack to 0 and removes its global and job services.
func (l *lifecycle) stopSwarm(ctx context.Context) {
	d, in := l.d, l.in
	l.r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	live, err := d.ownedServices(ctx, in.EnvID, in.Stack)
	if err != nil {
		d.log().Warn("could not read a stack's services before a stop", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemCheck, codeOf(ctx, err), "", err)
		return
	}
	for _, s := range live {
		l.addRow(s.Name)
	}
	l.r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	l.r.Update(ItemServices, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "services", int64(len(live))
	})
	for i, s := range live {
		if ctx.Err() != nil {
			l.fail(ItemServices, CodeCancelled, s.Name)
			return
		}
		l.prog.start(idService + s.Name)
		switch {
		case s.Mode != modeReplicated:
			// A global service has a task on every node and a job runs to its end: neither has a count to
			// remember, so Stop removes it and Start creates it again from the last good revision.
			err = d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error {
				return c.ServiceRemove(wctx, s.ID)
			})
			if err != nil && !errors.Is(err, dockerx.ErrServiceNotFound) {
				d.log().Warn("could not remove a stack service to stop it", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", err)
				l.failErr(ItemServices, codeOf(ctx, err), s.Name, err)
				return
			}
			l.res.Removed = append(l.res.Removed, s.Name)
			l.prog.line("Removed service " + s.Name)
			l.prog.done(idService+s.Name, map[string]string{"action": "removed"})
		case s.Labels[stacks.LabelStoppedReplicas] != "" || s.Replicas == 0:
			// Already stopped, by an earlier stop that did not finish or by a person: the count it
			// remembers is the one Start restores, so it is not written over with 0.
			l.prog.done(idService+s.Name, map[string]string{"action": "unchanged"})
		default:
			raw, rerr := replicasSpec(s.Spec, 0, strconv.FormatUint(s.Replicas, 10))
			if rerr != nil {
				d.log().Warn("could not build the stopped spec of a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", rerr)
				l.fail(ItemServices, CodeServiceRejected, s.Name)
				return
			}
			err = d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error {
				_, e := c.ServiceUpdate(wctx, s.ID, s.Version, raw, "")
				return e
			})
			if err != nil {
				d.log().Warn("could not scale a stack service to stop it", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", err)
				l.failErr(ItemServices, codeOf(ctx, err), s.Name, err)
				return
			}
			l.res.Scaled = append(l.res.Scaled, s.Name)
			l.prog.line("Stopped service " + s.Name + ", remembering " + strconv.FormatUint(s.Replicas, 10) + " replicas")
			l.prog.done(idService+s.Name, map[string]string{"action": "stopped", "replicas": strconv.FormatUint(s.Replicas, 10)})
		}
		l.r.Update(ItemServices, func(it *jobs.Item) { it.Current = int64(i + 1) })
	}
	l.markStopped(ctx, true)
}

// markStopped records the stack as stopped or running again, once everything the operation had to do is done.
func (l *lifecycle) markStopped(ctx context.Context, stopped bool) {
	if l.in.Store != nil {
		if err := l.in.Store.SetStopped(context.WithoutCancel(ctx), l.in.Stack.ID, stopped); err != nil {
			l.d.log().Error("could not record whether a stack is stopped", "environment", l.in.EnvID, "stack", l.in.Stack.Name, "error", err)
			l.fail(ItemServices, CodeEngineError, "")
			return
		}
	}
	l.final = itemEnd{state: jobs.ItemDone, result: map[string]string{
		"scaled": strconv.Itoa(len(l.res.Scaled)), "removed": strconv.Itoa(len(l.res.Removed)),
		"recreated": strconv.Itoa(len(l.res.Recreated)), "services": strconv.Itoa(l.res.Containers),
	}}
}

// startPlan is what a start of a swarm stack has to do: the services that remember a count, and the services
// of the last good revision the swarm no longer has.
type startPlan struct {
	live  []dockerx.ServiceDetail
	scale []dockerx.ServiceDetail
	// missing are the swarm names of the services to create again, plans their converted plans in the file's
	// order. prep and rev are the revision they come from, pins the images it ran by swarm name.
	missing map[string]bool
	plans   []convert.ServicePlan
	prep    RollbackPlan
	rev     Snapshot
	pins    map[string]string
}

// planStart reads what a start has to do from the services the stack has and its last good revision. A stack
// that was never deployed has no revision and nothing to create. A revision that cannot be redeployed as it
// stands is a *PrepareError.
func (d Deps) planStart(ctx context.Context, st Stack, store LifecycleStore, live []dockerx.ServiceDetail) (startPlan, error) {
	p := startPlan{live: live, missing: map[string]bool{}}
	for _, s := range live {
		if s.Labels[stacks.LabelStoppedReplicas] != "" {
			p.scale = append(p.scale, s)
		}
	}
	if store == nil {
		return p, errors.New("stackops: no store to read the last deployed version from")
	}
	rev, ok, err := store.LatestGoodRevision(ctx, st.ID)
	if err != nil || !ok {
		return p, err
	}
	pins, err := store.RevisionPins(ctx, rev.ID)
	if err != nil {
		return p, err
	}
	prep, err := d.PrepareRollback(ctx, st, rev, pins)
	if err != nil {
		return p, err
	}
	p.rev, p.pins, p.prep = rev, pins, prep
	have := make(map[string]bool, len(live))
	for _, s := range live {
		have[s.Name] = true
	}
	for _, sp := range prep.Plan.Services {
		if !have[sp.Name] {
			p.missing[sp.Name] = true
			p.plans = append(p.plans, sp)
		}
	}
	return p, nil
}

// startSwarm scales the services that remember a count back to it and creates again the ones Stop removed.
func (l *lifecycle) startSwarm(ctx context.Context) {
	d, in := l.d, l.in
	l.r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	live, err := d.ownedServices(ctx, in.EnvID, in.Stack)
	if err != nil {
		d.log().Warn("could not read a stack's services before a start", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemCheck, codeOf(ctx, err), "", err)
		return
	}
	plan, err := d.planStart(ctx, in.Stack, in.Store, live)
	if err != nil {
		code := CodeEngineError
		var pe *PrepareError
		var invalid *PinsInvalidError
		switch {
		case errors.As(err, &pe) && errors.As(pe.Err, &invalid):
			code = CodePinsInvalid
		case errors.As(err, &pe):
			code = CodeStackInvalid
		}
		d.log().Warn("a stopped stack could not be read for a start", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.fail(ItemCheck, code, "")
		return
	}
	// What a remote system says about a service made again can echo a value of the revision's: hide them all.
	l.prog.mask = stacks.NewMasker(plan.rev.Copy.Vars, plan.rev.Copy.Files).Mask
	if len(plan.scale) == 0 && len(plan.missing) == 0 && plan.rev.ID == "" {
		l.fail(ItemCheck, CodeNoRevision, "")
		return
	}
	if len(plan.missing) > 0 {
		// A service made again is a service created: the host-access policy is judged again, for the person
		// who asked, before anything is written (T-07-126).
		pol, perr := d.Policy(ctx, in.Caller, in.EnvID, plan.rev.Copy, plan.prep.Plan)
		switch {
		case perr != nil:
			if !errors.Is(perr, ErrNoPolicy) {
				d.log().Error("the host-access policy could not be applied to a start", "environment", in.EnvID, "stack", in.Stack.Name, "error", perr)
			}
			l.fail(ItemCheck, CodePolicyUnavailable, "")
			return
		case pol.Refused:
			l.detail = policyWords(pol)
			l.fail(ItemCheck, CodeNeedsAdmin, "")
			return
		}
	}
	for _, s := range plan.scale {
		l.addRow(s.Name)
	}
	for _, sp := range plan.plans {
		l.prog.addService(sp)
	}
	l.r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	l.r.Update(ItemServices, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "services", int64(len(plan.scale)+len(plan.plans))
	})
	var writes []serviceWrite
	for i, s := range plan.scale {
		if ctx.Err() != nil {
			l.fail(ItemServices, CodeCancelled, s.Name)
			return
		}
		l.prog.start(idService + s.Name)
		n, perr := strconv.ParseUint(s.Labels[stacks.LabelStoppedReplicas], 10, 64)
		if perr != nil {
			d.log().Warn("a stopped service remembers a count that is not a number", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name)
			l.fail(ItemServices, CodeServiceRejected, s.Name)
			return
		}
		raw, rerr := replicasSpec(s.Spec, n, "")
		if rerr != nil {
			l.fail(ItemServices, CodeServiceRejected, s.Name)
			return
		}
		// The tasks come back on nodes that may have to pull the image again, so the update carries the
		// registry credential of the image as the file wrote it.
		auth, aerr := d.authFor(ctx, in.EnvID, imageAsWritten(s))
		if aerr != nil {
			d.log().Error("could not resolve the registry credential of a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", aerr)
			l.fail(ItemServices, CodeRegistryUnresolved, s.Name)
			return
		}
		err = d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error {
			_, e := c.ServiceUpdate(wctx, s.ID, s.Version, raw, auth)
			return e
		})
		if err != nil {
			d.log().Warn("could not scale a stack service back up", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", err)
			l.failErr(ItemServices, codeOf(ctx, err), s.Name, err)
			return
		}
		l.res.Scaled = append(l.res.Scaled, s.Name)
		l.prog.line("Started service " + s.Name + " with " + strconv.FormatUint(n, 10) + " replicas")
		l.prog.accept(s.Name, "started")
		writes = append(writes, serviceWrite{name: s.Name, id: s.ID, action: "started", prior: s.UpdateStatus})
		l.r.Update(ItemServices, func(it *jobs.Item) { it.Current = int64(i + 1) })
	}
	if len(plan.plans) > 0 && !l.recreate(ctx, plan, &writes) {
		return
	}

	// A service is started when its tasks run, not when the swarm took the write.
	for _, v := range d.awaitServices(ctx, in.EnvID, writes, l.prog) {
		switch {
		case v.err == nil:
		case errors.Is(v.err, ErrConvergeTimeout):
			l.fail(ItemServices, CodeConvergeTimeout, v.name)
			return
		case errors.Is(v.err, ErrUpdateStopped):
			l.fail(ItemServices, CodeUpdateStopped, v.name)
			return
		case errors.Is(v.err, ErrTasksExhausted):
			l.fail(ItemServices, CodeTasksFailed, v.name)
			return
		case errors.Is(v.err, ErrNetworkFull):
			l.fail(ItemServices, CodeNetworkFull, v.name)
			return
		case ctx.Err() != nil:
			l.fail(ItemServices, CodeCancelled, "")
			return
		default:
			l.fail(ItemServices, CodeEngineError, v.name)
			return
		}
	}
	l.markStopped(ctx, false)
}

// recreate makes the services Stop removed again from the last good revision: the networks and the secrets and
// configs it uses are found (or made, if somebody removed them), and each service is created with the image
// the revision ran. It reports false when the operation failed.
func (l *lifecycle) recreate(ctx context.Context, plan startPlan, writes *[]serviceWrite) bool {
	d, in := l.d, l.in
	project := plan.prep.Loaded.Project
	objs, err := stacks.ResolveObjects(project, plan.prep.Files, plan.prep.Vars)
	if err != nil {
		var oe *stacks.ObjectError
		name := ""
		if errors.As(err, &oe) {
			name = oe.Name
		}
		l.fail(ItemServices, CodeObjectInvalid, name)
		return false
	}
	owned, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{
		allNetworks: hasExternal(plan.prep.Plan),
		allSecrets:  hasExternalObject(objs, stacks.KindSecret), allConfigs: hasExternalObject(objs, stacks.KindConfig),
	})
	if err != nil {
		d.log().Warn("could not read a stack's objects before a start", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemServices, codeOf(ctx, err), "", err)
		return false
	}
	liveNets := owned.Networks
	if hasExternal(plan.prep.Plan) {
		liveNets = owned.AllNetworks
	}
	if _, err := d.ensureNetworks(ctx, in.EnvID, in.Stack, plan.prep.Plan.Networks, liveNets, l.prog, nil); err != nil {
		l.failStep(ctx, ItemServices, err)
		return false
	}
	refs, _, err := d.ensureObjects(ctx, in.EnvID, in.Stack, objs, owned.Objects, l.prog)
	if err != nil {
		l.failStep(ctx, ItemServices, err)
		return false
	}
	converted, err := convert.Convert(project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: refs})
	if err != nil {
		d.log().Warn("a stack could not be converted for a start", "environment", in.EnvID, "stack", in.Stack.Name, "error", convertLogText(err, plan.rev.Copy))
		l.fail(ItemServices, CodeStackInvalid, "")
		return false
	}
	pins, err := RestoredPins(converted.Services, plan.pins)
	if err != nil {
		var invalid *PinsInvalidError
		name := ""
		if errors.As(err, &invalid) {
			name = invalid.Service
		}
		l.fail(ItemServices, CodePinsInvalid, name)
		return false
	}
	// The platforms the revision recorded go back with its images. A service made again is not running, so no running
	// service has platforms to lend, and nothing is asked of a registry (T-07-93).
	withStoredPlatforms(pins, plan.rev.Platforms)
	for _, sp := range converted.Services {
		if !plan.missing[sp.Name] {
			continue
		}
		if ctx.Err() != nil {
			l.fail(ItemServices, CodeCancelled, sp.Name)
			return false
		}
		l.prog.start(idService + sp.Name)
		// The image is the pinned one the revision ran, never the text and never a newer digest.
		if p, ok := pins[sp.Name]; ok && sp.Spec.TaskTemplate.ContainerSpec != nil {
			sp.Spec = withPin(sp.Spec, p)
		}
		raw, merr := json.Marshal(sp.Spec)
		if merr != nil {
			l.fail(ItemServices, CodeServiceRejected, sp.Name)
			return false
		}
		auth, aerr := d.authFor(ctx, in.EnvID, sp.ImageAsWritten)
		if aerr != nil {
			d.log().Error("could not resolve the registry credential of a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "error", aerr)
			l.fail(ItemServices, CodeRegistryUnresolved, sp.Name)
			return false
		}
		serviceID := ""
		err = d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error {
			id, _, e := c.ServiceCreate(wctx, raw, auth)
			serviceID = id
			return e
		})
		if err != nil {
			d.log().Warn("could not create a stack service again", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "error", err)
			l.failErr(ItemServices, codeOf(ctx, err), sp.Name, err)
			return false
		}
		l.res.Recreated = append(l.res.Recreated, sp.Name)
		l.prog.line("Created service " + sp.Name)
		l.prog.accept(sp.Name, "created")
		*writes = append(*writes, serviceWrite{name: sp.Name, id: serviceID, action: "created", job: isJob(sp)})
	}
	return true
}
