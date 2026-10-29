// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
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

	idVolume          = "volume:"
	idContainer       = "container:"
	idRemoveContainer = "remove-container:"
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

// standaloneOrder is where a step is in the run: ItemPull sits between the lookup and the networks.
func standaloneOrder(item string) int {
	order := []string{ItemCheck, ItemLookup, ItemPull, ItemNetworks, ItemVolumes, ItemServices, ItemCleanup}
	for i, id := range order {
		if id == item {
			return i
		}
	}
	return len(order)
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
	// Rollback says the deploy restores a revision that ran: Reviewed are that revision's own pins, by service
	// name in the compose file, and are used as they are (RestoredStandalonePins). They are not compared with
	// what the host runs now, which a reviewed deploy does and which would refuse an older digest (the point is
	// to move the host back), and no registry is asked for anything (STK-08, T-08-18).
	Rollback bool
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
	var events *composeEvents
	// cleanup, once the SDK's up has begun, removes what it left behind when the deploy stops.
	var cleanup func()
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
		if events != nil && !cancelled {
			events.endFailedRows()
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
		// What this deploy created and never started, and the networks nothing uses, go again; nothing else
		// is touched, no volume is removed and nothing is put back (D-14, D-20).
		if cleanup != nil {
			cleanup()
		}
		return res
	}
	fail := func(item, code, name string) DeployResult { return failWith(item, code, name, "") }
	failErr := func(item, code, name string, err error) DeployResult {
		detail = engineWords(err)
		return fail(item, code, name)
	}

	// 1. Check: the stack again, the host-access policy for the caller, the objects, and what the host holds.
	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	// A host its owner made read-only is the first thing to learn: nothing is asked of it or of a registry.
	if err := d.writable(ctx, in.EnvID, in.NodeID); err != nil {
		if errors.Is(err, envs.ErrHostReadOnly) {
			return fail(ItemCheck, CodeHostReadOnly, "")
		}
		d.log().Warn("a standalone host could not be written to before a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return failErr(ItemCheck, codeOf(ctx, err), "", err)
	}
	checked := stacks.Check(ctx, stacks.CheckInput{Name: in.Stack.Name, Target: stacks.TargetStandalone, Copy: in.Copy})
	if checked.Loaded == nil || checked.Problems.Errors() > 0 {
		d.log().Warn("a standalone stack no longer passes its check at deploy time", "environment", in.EnvID, "stack", in.Stack.Name)
		return fail(ItemCheck, CodeStackInvalid, "")
	}
	project := checked.Loaded.Project
	files, vars := stacks.NewFileSet(in.Copy.Files), stacks.ObjectVars(in.Copy)
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
	if foreignLive(live, OwnershipOf(in.Stack)) {
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
	if in.Rollback {
		pins, err := RestoredStandalonePins(project, in.Reviewed)
		var invalid *PinsInvalidError
		if errors.As(err, &invalid) {
			return fail(ItemLookup, CodePinsInvalid, invalid.Service)
		}
		pinned = PinOutcome{Pins: pins}
	} else if in.Reviewed != nil {
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
	// The SDK compares labels, and a container changed by hand keeps them: the services the review lists as
	// changed with a hash that matches are the ones the up is told to recreate, and the services a recreate
	// carries with it are named from the same comparison.
	if len(live.Containers) > 0 {
		err = d.Aggregator.OnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
			cmp, e := compareStandalone(ctx, c, project, live.Containers, nil, stacks.NewMasker(nil, nil))
			diff.Forced = cmp.forced()
			for _, row := range cmp.Carried {
				if row.Change == stacks.ChangeRecreated {
					diff.Carried = append(diff.Carried, row.Name)
				}
			}
			return e
		})
		if err != nil {
			d.log().Warn("could not compare a standalone stack with its host before a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
			return failErr(ItemLookup, codeOf(ctx, err), "", err)
		}
	}
	netNames, volNames := missingNetworks(project, live.Networks), missingVolumes(project, live.Volumes)
	for _, n := range netNames {
		prog.addNetwork(n)
	}
	for _, v := range volNames {
		prog.addVolume(v)
	}
	expected := map[string]int{}
	images := map[string][]string{}
	for _, name := range project.ServiceNames() {
		svc := project.Services[name]
		expected[name] = max(1, svc.GetScale())
		images[svc.Image] = append(images[svc.Image], name)
		prog.addStandaloneService(name)
	}
	// The containers of services the file no longer declares are removed by the up: each is a step under
	// the cleanup step, waiting until the SDK says it is removing it.
	orphans := map[string]bool{}
	for _, c := range live.Containers {
		if slices.Contains(diff.Removed, c.Service) {
			orphans[c.Name] = true
		}
	}
	for _, name := range sortedKeys(orphans) {
		prog.addRemoveContainer(name)
	}
	r.Finish(ItemLookup, jobs.ItemDone, "", "", map[string]string{"images": strconv.Itoa(len(pinned.Pins))})

	// 2b. Pull the images the services that will be created or recreated need and the host lacks, with the
	// stored credential. A service whose container stays is not pulled for: its image text did not change,
	// and a deploy never moves it (STK-09). The SDK finds the images on the host and needs no credential.
	if err := d.prePull(ctx, in.EnvID, in.NodeID, pullPins(project, diff, standalonePins(items, pinned)), r); err != nil {
		var pe *prePullError
		if !errors.As(err, &pe) {
			return failErr(ItemPull, codeOf(ctx, err), "", err)
		}
		return fail(ItemPull, pe.Code, pe.Service)
	}

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
	events = newComposeEvents(prog, expected, images, orphans)
	for _, name := range project.ServiceNames() {
		events.startService(name)
	}
	timeout := d.ConvergeTimeout
	if timeout <= 0 {
		timeout = DefaultConvergeTimeout
	}
	cleanup = func() {
		d.cleanupStandalone(ctx, in.EnvID, in.NodeID, in.Stack, live.Containers, netNames, &res, prog)
	}
	err = d.Aggregator.WriteOnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
		return d.up(ctx, c, project, dockerx.ComposeUpOptions{WaitTimeout: timeout, ForceRecreate: diff.Forced}, events.on)
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

	cleanup = nil

	// 4. Done: the networks and volumes exist, every service was brought to what the file says. What the
	// SDK changed is known from the hashes read before it ran.
	res.Networks, res.CreatedNetworks = len(netNames), netNames
	res.Services, res.Updated = len(diff.Added), len(diff.recreated())
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
		events.finishService(name, standaloneAction(diff, name), after.Containers)
	}
	for _, name := range sortedKeys(orphans) {
		prog.done(idRemoveContainer+name, nil)
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

// pullPins are the references of the services whose containers the up creates or recreates: the added and
// the changed ones, by service name.
func pullPins(p *types.Project, diff standaloneDiff, pins map[string]string) map[string]string {
	out := map[string]string{}
	for _, n := range slices.Concat(diff.Added, diff.Changed) {
		if ref, ok := pins[n]; ok && p.Services[n].Image != "" {
			out[n] = ref
		}
	}
	return out
}

// standaloneAction is what a deploy did to a service: created when it had no container, updated when its
// containers were recreated (its hash differed, it was forced, or a service it shares a namespace or volumes
// with was recreated), unchanged when the SDK left them alone.
func standaloneAction(diff standaloneDiff, name string) string {
	if slices.Contains(diff.Added, name) {
		return "created"
	}
	if slices.Contains(diff.Changed, name) || slices.Contains(diff.Forced, name) || slices.Contains(diff.Carried, name) {
		return "updated"
	}
	return "unchanged"
}

// foreignLive reports whether the host holds a container, network or volume under the stack's Compose
// project that the stack does not own: a Compose project of that name that Bayplan does not manage (D-12). A
// container is the stack's own, one the host CLI added to its project, or foreign (SplitProject): the CLI's has no
// bayplan.stack.id and counts only beside a container the stack owns of the same service, so a stack scaled with
// `docker compose up --scale` is no name clash, while another stack's container, a same-named project the stack holds
// nothing of, or a service the stack runs nothing of, still is (G-08-6, WR-03, P7 D-12). An adopted stack owns the containers of its project that carry no ID of its own until
// its first deploy, and the networks and volumes recorded when it was adopted for good (D-11, D-18), so a project it
// took over is no clash.
func foreignLive(live standaloneLive, own Ownership) bool {
	if _, _, foreign := SplitProject(own, live.Containers, func(c dockerx.ComposeContainer) map[string]string { return c.Labels }); len(foreign) > 0 {
		return true
	}
	for _, n := range live.Networks {
		if own.foreignNetwork(n.Name, n.Labels) {
			return true
		}
	}
	for _, v := range live.Volumes {
		if own.foreignVolume(v.Name, v.Labels) {
			return true
		}
	}
	return false
}

// composeEvents receives the Compose SDK's progress events of one deploy, one at a time, and turns them into
// the rows of the job: the network and volume steps, the rows of the containers under their service, the
// service row that summarises its containers, and the steps that remove a container the file no longer
// declares. It remembers the first event that failed, which names the service, network or volume a failed up
// was about (the SDK's error does not). The events are for display: nothing here decides whether the deploy
// worked.
type composeEvents struct {
	prog *progress
	// expected is how many containers each service must have, images the services that use each image
	// reference (to show a pull), orphans the containers the up removes.
	expected map[string]int
	images   map[string][]string
	orphans  map[string]bool

	mu         sync.Mutex
	failedKind string
	failedName string
	// states is the last state word of each container of each service, by service then container name.
	states map[string]map[string]string
	// pulling are the services an image they use is being pulled for.
	pulling map[string]bool
}

func newComposeEvents(prog *progress, expected map[string]int, images map[string][]string, orphans map[string]bool) *composeEvents {
	return &composeEvents{
		prog: prog, expected: expected, images: images, orphans: orphans,
		states: map[string]map[string]string{}, pulling: map[string]bool{},
	}
}

// startService marks a service row as working, counting its containers.
func (e *composeEvents) startService(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := int64(max(1, e.expected[name]))
	e.prog.r.Update(idService+name, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "containers", total
	})
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
	switch ev.Kind {
	case dockerx.ComposeKindNetwork:
		e.simple(idNetwork+ev.Name, ev)
	case dockerx.ComposeKindVolume:
		e.simple(idVolume+ev.Name, ev)
	case dockerx.ComposeKindImage:
		for _, svc := range e.images[ev.Name] {
			e.pulling[svc] = !ev.Done && !ev.Failed
			e.publish(svc)
		}
	case dockerx.ComposeKindContainer:
		if e.orphans[ev.Name] {
			e.simple(idRemoveContainer+ev.Name, ev)
		} else if ev.Service != "" && e.expected[ev.Service] > 0 {
			e.container(ev)
		}
	}
	e.prog.line(composeLine(ev))
}

func (e *composeEvents) firstFailure() (kind, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failedKind, e.failedName
}

// simple moves a network, volume or removal step with an event: working while the SDK is at it, done when it
// says the step is done (a removal is done only when it says Removed, since it stops the container first).
// A step the event is not for (a network that already exists has none) is left alone, as the reporter does
// for an item it does not have. Its failure is put on the step by the deploy that stops.
func (e *composeEvents) simple(id string, ev dockerx.ComposeEvent) {
	switch {
	case ev.Failed:
	case ev.Done && (!strings.HasPrefix(id, idRemoveContainer) || ev.Status == "Removed"):
		e.prog.done(id, nil)
	default:
		e.prog.start(id)
	}
}

// container moves the row of a container with an event and then publishes its service.
func (e *composeEvents) container(ev dockerx.ComposeEvent) {
	id, word := idContainer+ev.Name, stateWord(ev.Status)
	e.prog.addContainer(id, ev.Name, ev.Service)
	row := e.states[ev.Service]
	if row == nil {
		row = map[string]string{}
		e.states[ev.Service] = row
	}
	if ev.Failed {
		row[ev.Name] = "failed"
		detail := jobs.Clean(ev.Text, e.prog.mask)
		// The row keeps working until the deploy ends: an event is not the answer, and a deploy the SDK
		// reports as done is not failed because one of its events was worded as an error.
		e.prog.r.Update(id, func(it *jobs.Item) {
			it.State, it.Result, it.Detail = jobs.ItemRunning, map[string]string{"state": "failed"}, detail
		})
	} else {
		row[ev.Name] = word
		e.prog.r.Update(id, func(it *jobs.Item) { it.State, it.Result = jobs.ItemRunning, map[string]string{"state": word} })
	}
	e.publish(ev.Service)
}

// endFailedRows ends the rows of the containers an event reported as failed, failed, for a deploy that the
// SDK's answer says failed.
func (e *composeEvents) endFailedRows() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, row := range e.states {
		for name, word := range row {
			if word == "failed" {
				id := idContainer + name
				e.prog.r.Finish(id, jobs.ItemFailed, CodeEngineError, CodeMessage(CodeEngineError), map[string]string{"state": "failed"})
				e.prog.setEnded(id, jobs.ItemFailed)
			}
		}
	}
}

// stateWord is the word a container row shows for the SDK's status: the status in lower case, with the two
// group events of a recreate worded as an action.
func stateWord(status string) string {
	w := strings.ToLower(strings.TrimSpace(status))
	if w == "recreate" {
		return "recreating"
	}
	return w
}

// stage ranks a state word by how far a container has come: 0 for a word that is not part of coming up (a
// container removed or stopped on the way), 1 creating, 2 starting, 3 running, 4 healthy. A failed container
// is -1.
func stage(word string) int {
	switch word {
	case "creating", "created", "recreating", "waiting":
		return 1
	case "starting":
		return 2
	case "started", "running", "recreated", "restarted":
		return 3
	case "healthy":
		return 4
	case "failed":
		return -1
	}
	return 0
}

// summary is the phase of a service, how many of its containers run and how many it has or must have, from
// the last words of its containers.
func (e *composeEvents) summary(svc string) (phase string, running, total int) {
	lowest := 99
	for _, word := range e.states[svc] {
		st := stage(word)
		switch st {
		case -1:
			phase = "failed"
			total++
		case 0:
		default:
			total++
			lowest = min(lowest, st)
			if st >= 3 {
				running++
			}
		}
	}
	total = max(total, e.expected[svc])
	if phase == "" {
		phase = map[int]string{1: "creating", 2: "starting", 3: "running", 4: "healthy"}[lowest]
	}
	if phase == "" {
		phase = "creating"
	}
	return phase, running, total
}

// publish puts the summary of a service on its row.
func (e *composeEvents) publish(svc string) {
	phase, running, total := e.summary(svc)
	if e.pulling[svc] && phase == "creating" {
		phase = "pulling"
	}
	e.prog.r.Update(idService+svc, func(it *jobs.Item) {
		it.Current, it.Total, it.Unit = int64(running), int64(total), "containers"
		it.Result = map[string]string{"phase": phase}
	})
}

// finishService ends the row of a service the SDK brought up, and the rows of its containers. What the host
// holds now (final) is the truth about each container it lists; a container the host does not list keeps the
// last word the SDK gave it. A service the SDK left alone has no events, so its rows come from the host.
func (e *composeEvents) finishService(name, action string, final []dockerx.ComposeContainer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	row := e.states[name]
	if row == nil {
		row = map[string]string{}
		e.states[name] = row
	}
	for _, c := range final {
		if c.Service != name {
			continue
		}
		word := c.State
		if c.Health == "healthy" {
			word = "healthy"
		}
		row[c.Name] = word
	}
	names := make([]string, 0, len(row))
	for n, word := range row {
		// A container whose last word is not part of coming up is not shown, except one an event
		// reported as failed that the host does not hold: the deploy worked, and the row says what the
		// SDK said.
		if stage(word) != 0 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		e.prog.addContainer(idContainer+n, n, name)
		e.prog.r.Finish(idContainer+n, jobs.ItemDone, "", "", map[string]string{"state": row[n]})
		e.prog.setEnded(idContainer+n, jobs.ItemDone)
	}
	// A container the SDK removed on the way (the surplus one of a scale-down) was given its row by its first
	// event, so the row is ended like the others, reading the word the SDK last gave it. Left running it would
	// be reported as interrupted when the job ends, and a deploy that worked would read partial.
	for n, word := range row {
		id := idContainer + n
		if stage(word) == 0 && e.prog.has(id) {
			e.prog.r.Finish(id, jobs.ItemDone, "", "", map[string]string{"state": word})
			e.prog.setEnded(id, jobs.ItemDone)
		}
	}
	phase, running, total := e.summary(name)
	e.prog.serviceDone(name, action, phase, running, total)
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

// addRemoveContainer adds the step that removes a container of a service the file no longer declares.
func (p *progress) addRemoveContainer(name string) {
	p.add(jobs.ItemSpec{ID: idRemoveContainer + name, Label: "Remove container " + name, Parent: ItemCleanup, Kind: KindStep}, name, "container")
}

// addContainer adds the row of a container under its service, once. A container the SDK names that the job
// has no row for yet gets one when the first event about it arrives.
func (p *progress) addContainer(id, name, service string) {
	if p == nil {
		return
	}
	if p.has(id) {
		return
	}
	p.add(jobs.ItemSpec{ID: id, Label: name, Parent: idService + service, Kind: KindContainer}, name, "container")
}

// has says whether the job has a row with this ID.
func (p *progress) has(id string) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.ContainsFunc(p.added, func(a addedItem) bool { return a.id == id })
}

// serviceDone ends the row of a service that the SDK brought up: done, with what was done to it, the phase
// its containers ended in and how many of them run.
func (p *progress) serviceDone(name, action, phase string, running, total int) {
	if p == nil {
		return
	}
	id := idService + name
	p.r.Update(id, func(it *jobs.Item) {
		it.Current, it.Total, it.Unit = int64(running), int64(total), "containers"
	})
	p.r.Finish(id, jobs.ItemDone, "", "", map[string]string{
		"action": action, "settled": "true", "phase": phase,
		"running": strconv.Itoa(running), "desired": strconv.Itoa(total),
	})
	p.setEnded(id, jobs.ItemDone)
	p.line("Service " + name + " is running")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
