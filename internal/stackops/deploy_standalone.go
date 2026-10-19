// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The items of a standalone deploy job, in order. The check, lookup, services and cleanup items are the
// swarm deploy's (ItemCheck and the others); the networks and volumes the Compose SDK creates are added to
// their groups once the deploy knows which are missing.
const (
	// ItemVolumes is the group of the volumes a standalone deploy creates.
	ItemVolumes = "volumes"

	idVolume = "volume:"
)

// StandaloneDeployItems are the items of a standalone deploy job in the order they run.
func StandaloneDeployItems() []string {
	return []string{ItemCheck, ItemLookup, ItemNetworks, ItemVolumes, ItemServices, ItemCleanup}
}

// StandaloneItemSpecs are the items a standalone deploy job starts with, in run order, with the words the
// console shows (UI-SPEC, Deploy console, "Steps (standalone)"). The networks, volumes, services and
// containers are added while the job runs, once they are known.
func StandaloneItemSpecs() []jobs.ItemSpec {
	return []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the stack", Kind: KindStep},
		{ID: ItemLookup, Label: "Look up images", Kind: KindStep},
		{ID: ItemNetworks, Label: "Networks", Kind: KindGroup},
		{ID: ItemVolumes, Label: "Volumes", Kind: KindGroup},
		{ID: ItemServices, Label: "Deploy services", Kind: KindStep},
		{ID: ItemCleanup, Label: "Remove containers no longer in the stack", Kind: KindStep},
	}
}

func standaloneOrder(item string) int {
	for i, id := range StandaloneDeployItems() {
		if id == item {
			return i
		}
	}
	return len(StandaloneDeployItems())
}

// StandaloneDeployInput is a stack to deploy to a standalone host: the environment and the node of its one
// engine, the stack, the working copy the check and the deploy read, and who asks.
type StandaloneDeployInput struct {
	EnvID string
	// NodeID is the node the stack deploys to: the environment's one node, by the ID the write is routed
	// by (the environment's own ID for the server's local socket, the agent's otherwise).
	NodeID string
	Stack  Stack
	Copy   stacks.WorkingCopy
	// Reviewed, when not nil, are the images a review chose, by service name: the deploy uses exactly
	// these after checking each against what it would decide itself, and makes no registry lookup.
	Reviewed map[string]string
	// Update, when not nil, is an update of the images (D-16): the services in it are looked up again
	// even though their text did not change.
	Update map[string]bool
	// Pins is where the pins of the stack's last good revision are read from and where this deploy's are
	// stored, on the revision RevisionID. Nil means neither.
	Pins       PinStore
	RevisionID string
	Caller     Caller
	// Masker hides the stack's secret values in every text a remote system sent that the deploy puts in
	// its job.
	Masker stacks.Masker
	// OnDone, when set, is called once with the result before the job's last item is finished.
	OnDone func(DeployResult)
}

// DeployStandalone brings the stack up on a standalone host with the Compose SDK, reporting through r on the
// items of StandaloneDeployItems (STK-02, D-13, D-17). It checks the stack again, for the person who asks and
// on the values it will send (the request was judged some time ago and the operation lock is held now), reads
// what the host holds under the stack's Compose project, pins every image the way a swarm deploy does (a
// service that runs the text it is deployed with keeps its digest, a new or changed text is looked up on the
// host with the stored credential), prepares the project (stacks.PrepareStandalone) and runs the SDK's up:
// containers created, those whose configuration changed recreated, the others left alone, those of services
// the file no longer declares removed, and a wait until they run. The SDK's progress events are put in the job
// for display and decide nothing: the result is the error the SDK returned. It stops at the first failure,
// rolls nothing back and retries nothing.
//
// The write goes through Aggregator.WriteOnNode, so a read-only host is refused before the SDK is built, and
// it runs on the job's context: a cancel stops the SDK where it is.
//
// The last item is finished only after in.OnDone returns.
func (d Deps) DeployStandalone(ctx context.Context, in StandaloneDeployInput, r *jobs.Reporter) (res DeployResult) {
	prog := newProgress(r, in.Masker)
	final := itemEnd{state: jobs.ItemDone}
	var detail string
	defer func() {
		if in.OnDone != nil {
			in.OnDone(res)
		}
		r.Finish(ItemCleanup, final.state, final.code, final.message, final.result)
	}()

	// failWith ends the item that failed with its code and the items after it as not run. The row of the
	// network, volume or service the failure is about ends with the engine's words; with none to name, the
	// step does. The last item is held in final, to be finished after OnDone.
	failWith := func(item, code, name, message string) DeployResult {
		if message == "" {
			message = CodeMessage(code)
		}
		res.Code, res.FailedService = code, name
		state := jobs.ItemFailed
		cancelled := code == CodeCancelled
		if cancelled {
			state = jobs.ItemCancelled
			detail = ""
		}
		matched := item != ItemCheck && item != ItemLookup && prog.failed(item, name, state, code, message, detail)
		if !matched && detail != "" {
			prog.detail(item, detail)
			prog.line("Failed: " + detail)
		}
		later := itemEnd{state: jobs.ItemSkipped, code: CodeNotRun, message: CodeMessage(CodeNotRun)}
		if cancelled {
			later.state = jobs.ItemCancelled
		}
		for _, id := range StandaloneDeployItems() {
			var end itemEnd
			switch {
			case id == item:
				end = itemEnd{state: state, code: code, message: message}
			case standaloneOrder(id) > standaloneOrder(item):
				end = later
			case id == ItemNetworks || id == ItemVolumes:
				// A group runs through the SDK's up, which failed on something else: it ends as its
				// children did (r.Finish changes nothing for a group that has already ended).
				if gs := prog.groupState(id); gs == jobs.ItemDone {
					end = itemEnd{state: gs}
				} else {
					end = later
				}
			default:
				continue // finished before the failure
			}
			if id == ItemCleanup {
				final = end
			} else {
				r.Finish(id, end.state, end.code, end.message, nil)
			}
		}
		prog.rest(later.state, later.code, later.message)
		return res
	}
	fail := func(item, code, name string) DeployResult { return failWith(item, code, name, "") }
	failErr := func(item, code, name string, err error) DeployResult {
		detail = engineWords(err)
		return fail(item, code, name)
	}

	// 1. Check: the stack again, the host-access policy for the caller, the objects, and what the host holds.
	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	checked := stacks.Check(ctx, stacks.CheckInput{Name: in.Stack.Name, Target: stacks.TargetStandalone, Copy: in.Copy})
	if checked.Loaded == nil || checked.Problems.Errors() > 0 {
		d.log().Warn("a standalone stack no longer passes its check at deploy time", "environment", in.EnvID, "stack", in.Stack.Name)
		return fail(ItemCheck, CodeStackInvalid, "")
	}
	project := checked.Loaded.Project
	files, vars := stacks.NewFileSet(in.Copy.Files), varTable(in.Copy.Vars)
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, files, vars)
	if err != nil {
		var oe *stacks.ObjectError
		name := ""
		if errors.As(err, &oe) {
			name = oe.Name
		}
		d.log().Warn("a standalone stack's secret or config could not be resolved", "environment", in.EnvID, "stack", in.Stack.Name, "object", name, "error", err)
		return fail(ItemCheck, CodeObjectInvalid, name)
	}
	res.Total = len(project.Services)
	// The host-access policy is judged again, for the person who asked, before anything is written: a grant
	// can be gone since the request was made. A stack that cannot be judged is not an allowed one (T-07-86).
	pol, perr := d.Policy(ctx, in.Caller, in.EnvID, in.Copy, convert.Plan{})
	switch {
	case perr != nil:
		if !errors.Is(perr, ErrNoPolicy) {
			d.log().Error("the host-access policy could not be applied to a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", perr)
		}
		return fail(ItemCheck, CodePolicyUnavailable, "")
	case pol.Refused:
		detail = policyWords(pol)
		return fail(ItemCheck, CodeNeedsAdmin, "")
	}
	live, err := d.readStandalone(ctx, in.EnvID, in.NodeID, in.Stack.Name)
	if err != nil {
		d.log().Warn("could not read a standalone host before a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return failErr(ItemCheck, codeOf(ctx, err), "", err)
	}
	if foreignLive(live, in.Stack.ID) {
		return fail(ItemCheck, CodeNameClash, "")
	}
	r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	// 2. Look up images: decide which image each service is sent with (D-15, D-17). A service that runs the
	// text it is deployed with keeps the digest it runs and needs no registry.
	r.Update(ItemLookup, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	items := standaloneItems(project)
	var stored map[string]stacks.LivePin
	if in.Pins != nil {
		var perr error
		if stored, perr = in.Pins.GoodPins(ctx, in.Stack.ID); perr != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", in.EnvID, "stack", in.Stack.Name, "error", perr)
		}
	}
	read := d.nodeReader(in.EnvID, in.NodeID)
	var pinned PinOutcome
	if in.Reviewed != nil {
		pins, err := d.reviewedPins(ctx, in.EnvID, items, standaloneLivePins(live.Containers), stored, in.Update, in.Reviewed, read)
		var invalid *PinsInvalidError
		switch {
		case errors.As(err, &invalid):
			return fail(ItemLookup, CodePinsInvalid, invalid.Service)
		case err != nil:
			return fail(ItemLookup, codeOf(ctx, err), "")
		}
		pinned = PinOutcome{Pins: pins}
	} else {
		pinned = d.resolvePins(ctx, in.EnvID, items, standaloneLivePins(live.Containers), stored, in.Update, read)
	}
	res.Pins, res.Problems = pinned.Pins, placeProblems(files, pinned.Problems)
	if pinned.Blocked() {
		msg := ""
		if isImageFailure(pinned.Code) {
			if first, ok := firstError(pinned.Problems); ok {
				msg = first.Message
			}
		}
		return failWith(ItemLookup, pinned.Code, pinned.FailedService, msg)
	}
	if in.Pins != nil && in.RevisionID != "" {
		if err := in.Pins.SetPins(ctx, in.RevisionID, pinned.Refs()); err != nil {
			d.log().Error("could not store the pins of a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		}
	}
	if err := stacks.PrepareStandalone(project, in.Stack.ID, standalonePins(items, pinned), objs); err != nil {
		d.log().Warn("a standalone stack could not be prepared for a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return fail(ItemLookup, CodeStackInvalid, "")
	}
	diff, err := diffStandalone(project, live.Containers)
	if err != nil {
		d.log().Warn("could not hash a standalone stack's services", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return fail(ItemLookup, CodeStackInvalid, "")
	}
	netNames, volNames := missingNetworks(project, live.Networks), missingVolumes(project, live.Volumes)
	for _, n := range netNames {
		prog.addNetwork(n)
	}
	for _, v := range volNames {
		prog.addVolume(v)
	}
	for _, name := range project.ServiceNames() {
		prog.addStandaloneService(name)
	}
	r.Finish(ItemLookup, jobs.ItemDone, "", "", map[string]string{"images": strconv.Itoa(len(pinned.Pins))})

	// 3. The SDK's up creates the networks and volumes, then the services, in one call. The networks and
	// volumes items end with it, the services as the events and the containers left say.
	if ctx.Err() != nil {
		return fail(ItemNetworks, CodeCancelled, "")
	}
	r.Update(ItemNetworks, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	r.Update(ItemVolumes, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	r.Update(ItemServices, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "services", int64(len(project.Services))
	})
	for _, name := range project.ServiceNames() {
		prog.start(idService + name)
	}
	events := &composeEvents{prog: prog}
	timeout := d.ConvergeTimeout
	if timeout <= 0 {
		timeout = DefaultConvergeTimeout
	}
	err = d.Aggregator.WriteOnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
		return d.up(ctx, c, project, dockerx.ComposeUpOptions{WaitTimeout: timeout}, events.on)
	})
	if err != nil {
		d.log().Warn("the Compose SDK's up failed", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		kind, name := events.firstFailure()
		item := ItemServices
		switch kind {
		case dockerx.ComposeKindNetwork:
			item = ItemNetworks
		case dockerx.ComposeKindVolume:
			item = ItemVolumes
		}
		if name == "" || kind == dockerx.ComposeKindImage {
			name = ""
		}
		return failErr(item, codeOf(ctx, err), name, err)
	}

	// 4. Done: the networks and volumes exist, every service was brought to what the file says. What the
	// SDK changed is known from the hashes read before it ran.
	res.Networks, res.CreatedNetworks = len(netNames), netNames
	res.Services, res.Updated = len(diff.Added), len(diff.Changed)
	res.RemovedServices = diff.Removed
	after, aerr := d.readStandalone(ctx, in.EnvID, in.NodeID, in.Stack.Name)
	if aerr != nil {
		d.log().Warn("could not read a standalone host after a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", aerr)
	}
	for _, n := range netNames {
		prog.done(idNetwork+n, nil)
	}
	for _, v := range volNames {
		prog.done(idVolume+v, nil)
	}
	r.Finish(ItemNetworks, jobs.ItemDone, "", "", nil)
	r.Finish(ItemVolumes, jobs.ItemDone, "", "", nil)
	for _, name := range project.ServiceNames() {
		prog.serviceDone(name, standaloneAction(diff, name), after.Containers)
	}
	r.Finish(ItemServices, jobs.ItemDone, "", "", map[string]string{
		"services": strconv.Itoa(res.Services), "updated": strconv.Itoa(res.Updated),
	})
	final = itemEnd{state: jobs.ItemDone, result: map[string]string{
		"networks": strconv.Itoa(res.Networks), "volumes": strconv.Itoa(len(volNames)),
		"removedServices": strconv.Itoa(len(diff.Removed)),
	}}
	return res
}

// standaloneAction is what a deploy did to a service: created when it had no container, updated when its
// containers were recreated, unchanged when the SDK left them alone.
func standaloneAction(diff standaloneDiff, name string) string {
	for _, n := range diff.Added {
		if n == name {
			return "created"
		}
	}
	for _, n := range diff.Changed {
		if n == name {
			return "updated"
		}
	}
	return "unchanged"
}

// foreignLive reports whether the host holds a container, network or volume under the stack's Compose
// project that does not carry the stack's ID: a Compose project of that name that Bayplan does not manage
// (D-12).
func foreignLive(live standaloneLive, stackID string) bool {
	for _, c := range live.Containers {
		if c.Labels[stacks.LabelStackID] != stackID {
			return true
		}
	}
	for _, n := range live.Networks {
		if n.Labels[stacks.LabelStackID] != stackID {
			return true
		}
	}
	for _, v := range live.Volumes {
		if v.Labels[stacks.LabelStackID] != stackID {
			return true
		}
	}
	return false
}

// composeEvents receives the Compose SDK's progress events of one deploy, one at a time, and puts them in the
// job's output. It remembers the first event that failed, which names the service, network or volume a failed
// up was about (the SDK's error does not).
type composeEvents struct {
	prog *progress

	mu         sync.Mutex
	failedKind string
	failedName string
}

func (e *composeEvents) on(ev dockerx.ComposeEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.Failed && e.failedKind == "" {
		e.failedKind, e.failedName = ev.Kind, ev.Name
		if ev.Service != "" {
			e.failedName = ev.Service
		}
	}
	e.prog.line(composeLine(ev))
}

func (e *composeEvents) firstFailure() (kind, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failedKind, e.failedName
}

// composeLine is one line of the output for an SDK event: its status and what it is about, and the SDK's
// words when it has more to say.
func composeLine(ev dockerx.ComposeEvent) string {
	var b strings.Builder
	if ev.Status != "" {
		b.WriteString(ev.Status)
	}
	if ev.Kind != "" {
		b.WriteString(" " + ev.Kind + " " + ev.Name)
	}
	if ev.Text != "" && ev.Text != ev.Status {
		b.WriteString(": " + ev.Text)
	}
	return strings.TrimSpace(b.String())
}

// addVolume adds the step that creates a volume.
func (p *progress) addVolume(name string) {
	p.add(jobs.ItemSpec{ID: idVolume + name, Label: "Create volume " + name, Parent: ItemVolumes, Kind: KindStep}, name, "volume")
}

// addStandaloneService adds the row of a service of a standalone stack.
func (p *progress) addStandaloneService(name string) {
	p.add(jobs.ItemSpec{ID: idService + name, Label: name, Parent: ItemServices, Kind: KindService}, name, "service")
}

// serviceDone ends the row of a service that the SDK brought up: done, with what was done to it and how many
// of its containers run.
func (p *progress) serviceDone(name, action string, containers []dockerx.ComposeContainer) {
	if p == nil {
		return
	}
	running, total := 0, 0
	for _, c := range containers {
		if c.Service != name {
			continue
		}
		total++
		if c.State == "running" {
			running++
		}
	}
	p.r.Finish(idService+name, jobs.ItemDone, "", "", map[string]string{
		"action": action, "settled": "true", "running": strconv.Itoa(running), "desired": strconv.Itoa(total),
	})
	p.line("Service " + name + " is running")
}
