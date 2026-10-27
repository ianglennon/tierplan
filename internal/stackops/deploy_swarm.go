// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
	"github.com/ianglennon/bayplan/internal/stacks/rebuild"
)

// The items of a swarm deploy job, in order.
const (
	ItemCheck    = "check"
	ItemLookup   = "lookup"
	ItemNetworks = "networks"
	ItemObjects  = "objects"
	ItemServices = "services"
	ItemCleanup  = "cleanup"
)

// DeployItems returns the items of a swarm deploy job in the order they run, for the caller that
// starts the job.
func DeployItems() []string {
	return []string{ItemCheck, ItemLookup, ItemNetworks, ItemObjects, ItemServices, ItemCleanup}
}

// The codes a swarm deploy ends a failed item with. Each has a sentence in codeMessages; the
// manager's own words are never put in a job.
const (
	CodeHostReadOnly       = "host_read_only"
	CodeAgentPolicyDenied  = "agent_policy_denied"
	CodeNoManagerConnected = "no_manager_connected"
	CodeServiceRejected    = "service_rejected"
	CodeServiceChanged     = "service_changed"
	CodeServiceNameTaken   = "service_name_taken"
	CodeNetworkConflict    = "network_conflict"
	CodeNetworkFailed      = "network_failed"
	CodeExternalMissing    = "external_missing"
	CodeExternalNotSwarm   = "external_not_swarm"
	CodeObjectInvalid      = "object_invalid"
	CodeObjectFailed       = "object_failed"
	CodeObjectNameTaken    = "object_name_taken"
	CodeStackInvalid       = "stack_invalid"
	CodeNameClash          = "stack_name_clash"
	CodeRegistryUnresolved = "registry_unresolved"
	CodeEngineError        = "engine_error"
	CodeCancelled          = "cancelled"
	CodeNotRun             = "not_run"
	CodeConvergeTimeout    = "converge_timeout"
	CodeTasksFailed        = "tasks_failed"
	CodeNetworkFull        = "network_full"
	CodeUpdateStopped      = "update_stopped"
	CodePolicyUnavailable  = "policy_unavailable"
)

var codeMessages = map[string]string{
	CodeHostReadOnly:        "This host is read-only, so Bayplan cannot change it.",
	CodeAgentPolicyDenied:   "The agent refused the request.",
	CodeNoManagerConnected:  "No manager of this swarm is connected to Bayplan.",
	CodeServiceRejected:     "The swarm rejected the service.",
	CodeServiceChanged:      "The service was changed by someone else while the deploy ran.",
	CodeServiceNameTaken:    "The swarm already has a service of that name.",
	CodeNetworkConflict:     "The swarm already has a network of that name that this stack does not own.",
	CodeNetworkFailed:       "The swarm could not create the network.",
	CodeExternalMissing:     "A network, secret or config the stack names as external does not exist.",
	CodeExternalNotSwarm:    "A network the stack names as external is not a swarm network.",
	CodeObjectInvalid:       "A secret or config of the stack has no usable content.",
	CodeObjectFailed:        "The swarm could not create a secret or config.",
	CodeObjectNameTaken:     "The swarm already has a secret or config of that name that this stack does not own.",
	CodeStackInvalid:        "The stack could not be converted for the swarm.",
	CodeNameClash:           "A stack of that name already exists on this environment that Bayplan does not manage.",
	CodeRegistryUnresolved:  "The registry credential for an image could not be read.",
	CodeImageUnresolved:     "An image could not be looked up to pin it to a digest.",
	CodeRegistryUnreachable: "A registry could not be reached to pin an image.",
	CodeEngineError:         "The engine could not complete the request.",
	CodeCancelled:           "The deploy was cancelled.",
	CodeNotRun:              "Not run because an earlier step failed.",
	CodeConvergeTimeout:     "The service didn't settle in time. Docker may still finish it.",
	CodeUpdateStopped:       "The swarm stopped the update of the service.",
	CodeTasksFailed:         "The service's tasks stopped and the swarm will not start them again.",
	CodeNetworkFull:         "A network of the service has no free addresses, so the swarm cannot start all of its tasks.",
	CodeNeedsAdmin:          "The stack uses host access that needs Admin.",
	CodePolicyUnavailable:   "The host-access policy could not be applied, so nothing was deployed.",
	CodePinsInvalid:         "The images in the request don't match what the deploy would use. Review the stack again.",
}

// CodeMessage is the sentence for a deploy code, empty for a code it does not know. A pull failure code
// (pull_denied, image_not_found, rate_limited ...) has the sentence of Phase 5's pulls (PullMessages).
func CodeMessage(code string) string {
	if m, ok := codeMessages[code]; ok {
		return m
	}
	return PullMessages[code]
}

// SwarmDeployInput is a stack to deploy to a swarm: the environment, the stack, the loaded compose
// project with the files and variables its secrets and configs are read from, and who asks.
type SwarmDeployInput struct {
	EnvID string
	Stack Stack
	// Project is the stack's compose file as stacks.Deployable (the check pipeline) produced it,
	// with env_file resolved and build stripped. The deploy converts it twice:
	// once for the networks and the list of services, and again for the services after the
	// secrets and configs exist, so that they name the objects it created.
	Project *composetypes.Project
	// Files and Vars are the stack's working copy: where a secret or config reads its content.
	Files stacks.FileSet
	Vars  map[string]string
	// Copy is the working copy the project was loaded from. The host-access policy is judged on it
	// again, for Caller, before the deploy writes anything.
	Copy   stacks.WorkingCopy
	Caller Caller
	// Masker hides the stack's secret values in every text a remote system sent that the deploy puts
	// in its job: an item's Detail and the output log. The zero Masker hides nothing.
	Masker stacks.Masker
	// Pins is where the pins of the stack's last good revision are read from and where this
	// deploy's are stored, on the revision RevisionID. Nil means neither: the images are still
	// pinned, and a service the engine lacks is looked up again.
	Pins       PinStore
	RevisionID string
	// Update, when not nil, is an update of the images (D-16): the services in it, by compose
	// name or swarm name, are looked up again even though their text did not change, and every
	// other service keeps the digest it runs.
	Update map[string]bool
	// Reviewed, when not nil, are the images a review chose, by swarm service name: the deploy uses
	// exactly these, after checking each against what it would decide itself (ReviewedPins), and
	// makes no registry lookup. Nil means the deploy looks the images up itself.
	Reviewed map[string]string
	// Rollback says the deploy restores a revision that ran: Reviewed are that revision's own pins, by
	// swarm service name, and are used as they are. They are not compared with what the swarm runs now
	// (the point is to move it back), and no registry is asked for anything (STK-07, T-07-93).
	Rollback bool
	// OnDone, when set, is called once with the result before the job's last item is finished,
	// so a job that reads as finished has already been recorded by the caller.
	OnDone func(DeployResult)
}

// DeployResult is what a deploy did, as far as it got. FailedService and Code are empty when it
// succeeded. A cancelled deploy has Code CodeCancelled.
type DeployResult struct {
	// Networks and Services count the networks and services created; Updated counts the
	// services that were updated in place.
	Networks, Services, Updated int
	// CreatedNetworks and CreatedObjects name what this deploy created: networks, and secrets and
	// configs by their hashed names. RemovedServices names the services the file no longer
	// declares that were removed, RemovedObjects the secrets and configs it replaced that were
	// removed, and LeftInPlace the replaced ones that could not be removed.
	CreatedNetworks, CreatedObjects, RemovedServices, RemovedObjects, LeftInPlace []string
	// Total is how many services the stack declares, Updated and Services how many of them the swarm
	// accepted before the deploy ended. A deploy that stopped reads "{Updated+Services} of {Total}".
	Total int
	// CleanedUp names the networks, secrets and configs this deploy created and removed again because
	// it stopped before any service used them. LeftInPlace, for a deploy that stopped, names those it
	// created that stay: a service uses them, or they could not be removed.
	CleanedUp []string
	// FailedService names the service, network, secret or config the failure is about.
	FailedService, Code string
	// Pins are the images the services were deployed with, by swarm service name, and Problems
	// what the lookup found: a blocking problem for each image it could not pin, a warning for
	// each local image.
	Pins     map[string]Pin
	Problems stacks.Problems
}

// PinStore keeps the digests a stack ran with. Repo is the one implementation.
type PinStore interface {
	// GoodPins is what the stack's newest good revision ran, by swarm service name.
	GoodPins(ctx context.Context, stackID string) (map[string]stacks.LivePin, error)
	// SetPins stores the pinned reference of each service on a revision.
	SetPins(ctx context.Context, snapshotID string, pins map[string]string) error
}

var _ PinStore = Repo{}

// OK reports whether the deploy ended without a failure.
func (r DeployResult) OK() bool { return r.Code == "" }

// DeploySwarm creates the stack's networks, secrets, configs and services on the swarm, once each
// and in order, reporting through r on the items of DeployItems. It reads what the stack already
// owns from a manager (one read), creates each missing network, creates the secrets and configs
// whose content is new, then creates or updates each service with the registry credential chosen
// for its image, and last removes the services the file no longer declares and the secrets and
// configs the deploy replaced. It stops at the first failure and ends the item that failed with a
// code; items after it end cancelled with CodeNotRun. Every write goes through
// Aggregator.WriteOnManager and is never retried (P2 D-42).
//
// An external network, secret or config must already exist and is never created, changed or
// removed. Nothing is removed before the last item, after every service was created or updated, so
// a deploy that stops earlier leaves the old services and objects where they are (D-07).
//
// A write that has begun is finished even if the job is cancelled: the cancel is looked for between
// writes, so a service is never left half written and no write is repeated. When the deploy stops,
// by a failure or a cancel, nothing is rolled back and nothing is retried; the networks, secrets and
// configs it created that no service uses are removed again (cleanupCreated).
//
// The last item is finished only after in.OnDone returns.
func (d Deps) DeploySwarm(ctx context.Context, in SwarmDeployInput, r *jobs.Reporter) (res DeployResult) {
	prog := newProgress(r, in.Masker)
	final := itemEnd{state: jobs.ItemDone}
	// detail is the engine's own words about the failure, set by the step that has them before it
	// fails; failWith puts them on the item that failed and in the output.
	var detail string
	defer func() {
		if in.OnDone != nil {
			in.OnDone(res)
		}
		r.Finish(ItemCleanup, final.state, final.code, final.message, final.result)
	}()
	// fail ends the item that failed with its code and the items after it as not run. The last
	// item is held in final, to be finished after OnDone.
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
		// The network, secret, config or service the failure is about ends with its code and the
		// engine's words; with none of those to name, the words go on the step. The lookup names a
		// service it could not pin, which no step of the swarm has yet.
		matched := item != ItemCheck && item != ItemLookup && prog.failed(item, name, state, code, message, detail)
		if !matched && detail != "" {
			prog.detail(item, detail)
			prog.line("Failed: " + detail)
		}
		// Whatever follows was not run: skipped after a failure, cancelled after a cancel.
		later := itemEnd{state: jobs.ItemSkipped, code: CodeNotRun, message: CodeMessage(CodeNotRun)}
		if cancelled {
			later.state = jobs.ItemCancelled
		}
		for _, id := range DeployItems() {
			var end itemEnd
			switch {
			case id == item:
				end = itemEnd{state: state, code: code, message: message}
			case order(id) > order(item):
				end = later
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
		// What this deploy created and no service uses goes again; nothing else is touched, and no
		// service is put back (D-14).
		d.cleanupCreated(ctx, in.EnvID, in.Stack, &res, prog)
		return res
	}
	fail := func(item, code, name string) DeployResult { return failWith(item, code, name, "") }
	// failErr fails an item whose cause is an error from the engine, whose words the item keeps.
	failErr := func(item, code, name string, err error) DeployResult {
		detail = engineWords(err)
		return fail(item, code, name)
	}
	// failStep ends the item that failed with the code and name a step reported.
	failStep := func(item string, err error) DeployResult {
		code, name := stepOf(ctx, err)
		return failErr(item, code, name, err)
	}

	// 1. Check: resolve the secrets and configs, plan the conversion and read what the stack
	// owns, once, from a manager.
	r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	objs, err := stacks.ResolveObjects(in.Project, in.Files, in.Vars)
	if err != nil {
		var oe *stacks.ObjectError
		name := ""
		if errors.As(err, &oe) {
			name = oe.Name
		}
		d.log().Warn("a stack's secret or config could not be resolved", "environment", in.EnvID, "stack", in.Stack.Name, "object", name, "error", err)
		return fail(ItemCheck, CodeObjectInvalid, name)
	}
	planned, err := convert.Convert(in.Project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: plannedRefs(in.Stack.Name, objs)})
	if err != nil {
		d.log().Warn("a stack could not be converted for a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", convertLogText(err, in.Copy))
		return fail(ItemCheck, CodeStackInvalid, "")
	}
	res.Total = len(planned.Services)
	// The host-access policy is judged again here, for the person who asked, on the values this deploy
	// will send and before it writes anything: the request was judged some time ago, and a grant can
	// be gone since. A stack that cannot be judged is not an allowed one (T-07-86).
	pol, perr := d.Policy(ctx, in.Caller, in.EnvID, in.Copy, planned)
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
	extSecrets, extConfigs := hasExternalObject(objs, stacks.KindSecret), hasExternalObject(objs, stacks.KindConfig)
	owned, err := d.readLive(ctx, in.EnvID, in.Stack.Name, liveWant{allNetworks: hasExternal(planned), allSecrets: extSecrets, allConfigs: extConfigs})
	if err != nil {
		d.log().Warn("could not read a stack's objects before a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		return failErr(ItemCheck, codeOf(ctx, err), "", err)
	}
	existingNets, allNets, existingSvcs, live := owned.Networks, owned.AllNetworks, owned.Services, owned.Objects
	if own := OwnershipOf(in.Stack); clash(own, existingNets, existingSvcs, live) {
		return fail(ItemCheck, CodeNameClash, "")
	}
	// The whole picture of what is about to happen goes into the job now: the networks and objects
	// that will be created and the services that will be deployed, all waiting, so the console
	// shows them before the first one starts.
	liveNets := existingNets
	if hasExternal(planned) {
		liveNets = allNets
	}
	for _, n := range planned.Networks {
		if _, found := findNetwork(liveNets, n.Name); !n.External && !found {
			prog.addNetwork(n.Name)
		}
	}
	for _, o := range objs {
		if o.External {
			continue
		}
		if full, _ := stacks.ObjectName(in.Stack.Name, o.Name, o.Content); !hasObject(live, o.Kind, full) {
			prog.addObject(o.Kind, o.Name, full)
		}
	}
	for _, sp := range planned.Services {
		prog.addService(sp)
	}
	r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	// 2. Look up images: decide which image each service is sent with. A service that runs the
	// text it is deployed with keeps the digest it runs and needs no registry; a new service or
	// a changed text is looked up through a manager and deployed as tag@sha256 (D-15).
	r.Update(ItemLookup, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	byName := make(map[string]dockerx.ServiceDetail, len(existingSvcs))
	for _, s := range existingSvcs {
		byName[s.Name] = s
	}
	// A service the swarm lost keeps what the stack's last good revision ran for the same text
	// (A7); a stack with no stored pins simply has none to fall back to.
	var stored map[string]stacks.LivePin
	if in.Pins != nil {
		var err error
		if stored, err = in.Pins.GoodPins(ctx, in.Stack.ID); err != nil {
			d.log().Warn("could not read the pins of a stack's last good revision", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		}
	}
	var pinned PinOutcome
	if in.Rollback {
		pins, err := RestoredPins(planned.Services, in.Reviewed)
		var invalid *PinsInvalidError
		if errors.As(err, &invalid) {
			return fail(ItemLookup, CodePinsInvalid, invalid.Service)
		}
		pinned = PinOutcome{Pins: pins}
	} else if in.Reviewed != nil {
		pins, err := d.ReviewedPins(ctx, in.EnvID, planned.Services, byName, stored, in.Update, in.Reviewed)
		var invalid *PinsInvalidError
		switch {
		case errors.As(err, &invalid):
			return fail(ItemLookup, CodePinsInvalid, invalid.Service)
		case err != nil:
			return fail(ItemLookup, codeOf(ctx, err), "")
		}
		pinned = PinOutcome{Pins: pins}
	} else {
		pinned = d.ResolvePins(ctx, in.EnvID, planned.Services, byName, stored, in.Update)
	}
	// A service keeps the platforms of the image it runs; they belong to the image, so a pin that names another one
	// borrows none (G-08-4, T-08-97).
	carryPlatforms(pinned.Pins, byName)
	res.Pins, res.Problems = pinned.Pins, placeProblems(in.Files, pinned.Problems)
	if pinned.Blocked() {
		msg := ""
		if isImageFailure(pinned.Code) {
			// The sentence names the registry and the service; it holds nothing the registry said.
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
	r.Finish(ItemLookup, jobs.ItemDone, "", "", map[string]string{"images": strconv.Itoa(len(pinned.Pins))})

	// 3. Networks: create each one the stack uses that does not exist yet, and check that the
	// external ones are there.
	r.Update(ItemNetworks, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	createdNets, err := d.ensureNetworks(ctx, in.EnvID, in.Stack, planned.Networks, liveNets, prog)
	res.Networks, res.CreatedNetworks = len(createdNets), createdNets
	if err != nil {
		return failStep(ItemNetworks, err)
	}
	r.Finish(ItemNetworks, jobs.ItemDone, "", "", nil)

	// 4. Objects: the secrets and configs, created under their hashed names or found.
	r.Update(ItemObjects, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	refs, created, err := d.ensureObjects(ctx, in.EnvID, in.Stack, objs, live, prog)
	res.CreatedObjects = created
	if err != nil {
		return failStep(ItemObjects, err)
	}
	r.Finish(ItemObjects, jobs.ItemDone, "", "", nil)

	// 5. Services: convert again with the objects that now exist, update the services that exist at
	// the version just read and create the rest.
	r.Update(ItemServices, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	plan, err := convert.Convert(in.Project, convert.Options{StackName: in.Stack.Name, StackID: in.Stack.ID, Objects: refs})
	if err != nil {
		d.log().Warn("a stack could not be converted for a deploy", "environment", in.EnvID, "stack", in.Stack.Name, "error", convertLogText(err, in.Copy))
		return fail(ItemServices, CodeStackInvalid, "")
	}
	r.Update(ItemServices, func(it *jobs.Item) { it.Unit, it.Total = "services", int64(len(plan.Services)) })
	var writes []serviceWrite
	for i, sp := range plan.Services {
		if ctx.Err() != nil {
			return fail(ItemServices, CodeCancelled, sp.Name)
		}
		existing, update := byName[sp.Name]
		prog.start(idService + sp.Name)
		// The image is the pinned one, never the text; an update keeps the counter that makes
		// the swarm replace tasks, so a redeploy that changed nothing replaces nothing.
		if p, ok := pinned.Pins[sp.Name]; ok {
			sp.Spec = withPin(sp.Spec, p)
		}
		if update {
			sp.Spec.TaskTemplate.ForceUpdate = liveForceUpdate(existing.Spec)
		}
		raw, err := json.Marshal(sp.Spec)
		if err != nil {
			return fail(ItemServices, CodeServiceRejected, sp.Name)
		}
		auth, err := d.authFor(ctx, in.EnvID, sp.ImageAsWritten)
		if err != nil {
			d.log().Error("could not resolve the registry credential of a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "error", err)
			return fail(ItemServices, CodeRegistryUnresolved, sp.Name)
		}
		serviceID := existing.ID
		err = d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error {
			if update {
				_, e := c.ServiceUpdate(wctx, existing.ID, existing.Version, raw, auth)
				return e
			}
			id, _, e := c.ServiceCreate(wctx, raw, auth)
			serviceID = id
			return e
		})
		if err != nil {
			d.log().Warn("could not write a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", sp.Name, "update", update, "error", err)
			return failErr(ItemServices, codeOf(ctx, err), sp.Name, err)
		}
		action := "created"
		var prior *dockerx.ServiceUpdateStatus
		if update {
			res.Updated++
			action = "updated"
			prior = existing.UpdateStatus
		} else {
			res.Services++
		}
		prog.line(strings.ToUpper(action[:1]) + action[1:] + " service " + sp.Name)
		// The swarm took the spec; the row goes on working until its tasks run.
		prog.accept(sp.Name, action)
		writes = append(writes, serviceWrite{name: sp.Name, id: serviceID, action: action, prior: prior, job: isJob(sp)})
		r.Update(ItemServices, func(it *jobs.Item) { it.Current = int64(i + 1) })
	}

	// A service is done when it converges, not when the swarm took it: wait for every service that
	// was written, a few at a time. The first that did not settle, in the file's order, is where the
	// deploy stopped; the others have their own answer on their rows.
	for _, v := range d.awaitServices(ctx, in.EnvID, writes, prog) {
		switch {
		case v.err == nil:
		case errors.Is(v.err, ErrConvergeTimeout):
			return fail(ItemServices, CodeConvergeTimeout, v.name)
		case errors.Is(v.err, ErrUpdateStopped):
			return fail(ItemServices, CodeUpdateStopped, v.name)
		case errors.Is(v.err, ErrTasksExhausted):
			return fail(ItemServices, CodeTasksFailed, v.name)
		case errors.Is(v.err, ErrNetworkFull):
			return fail(ItemServices, CodeNetworkFull, v.name)
		case ctx.Err() != nil:
			return fail(ItemServices, CodeCancelled, "")
		default:
			return fail(ItemServices, CodeEngineError, v.name)
		}
	}
	r.Finish(ItemServices, jobs.ItemDone, "", "", map[string]string{
		"services": strconv.Itoa(res.Services), "updated": strconv.Itoa(res.Updated),
	})

	// 6. Cleanup: every service now uses the new objects, so the ones they replaced can go.
	r.Update(ItemCleanup, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if ctx.Err() != nil {
		return fail(ItemCleanup, CodeCancelled, "")
	}
	declared := make(map[string]bool, len(plan.Services))
	for _, sp := range plan.Services {
		declared[sp.Name] = true
	}
	res.RemovedServices, err = d.pruneServices(ctx, in.EnvID, in.Stack, declared, existingSvcs, prog)
	if err != nil {
		return failStep(ItemCleanup, err)
	}
	removed, left, err := d.removeReplacedObjects(ctx, in.EnvID, in.Stack, refs, prog)
	res.RemovedObjects, res.LeftInPlace = removed, left
	if err != nil {
		return failErr(ItemCleanup, codeOf(ctx, err), "", err)
	}
	final = itemEnd{state: jobs.ItemDone, result: map[string]string{
		"networks": strconv.Itoa(res.Networks), "objects": strconv.Itoa(len(res.CreatedObjects)),
		"removedServices": strconv.Itoa(len(res.RemovedServices)),
		"removedObjects":  strconv.Itoa(len(res.RemovedObjects)), "leftInPlace": strconv.Itoa(len(res.LeftInPlace)),
	}}
	return res
}

// policyWords says what the policy refused, for the check step that failed: each setting and where it
// is, as the refusal of the request words it.
func policyWords(pol PolicyResult) string {
	if len(pol.Entries) == 0 {
		return "The compose file can't be checked for host access."
	}
	parts := make([]string, 0, len(pol.Entries))
	for _, e := range pol.Entries {
		parts = append(parts, e.Text+" at "+e.File+":"+strconv.Itoa(e.Line))
	}
	return strings.Join(parts, "; ")
}

// engineWords are the words of an error from the engine, for the item that failed: what the manager
// said when it refused a spec, else the error's own text. They are masked and cleaned by the job before
// anyone sees them, never put in a log line of the server's and never in an item's Message.
func engineWords(err error) string {
	if err == nil {
		return ""
	}
	var se *stepError
	if errors.As(err, &se) {
		err = se.err
	}
	var rejected *dockerx.SpecRejectedError
	if errors.As(err, &rejected) {
		return trimDaemon(rejected.Message)
	}
	// The wrapping says which host the call went to; the browser is told what the engine said.
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return trimDaemon(err.Error())
		}
		err = inner
	}
}

// trimDaemon drops the prefix the engine's client puts before a daemon's answer: the console says
// Docker said it already.
func trimDaemon(s string) string { return strings.TrimPrefix(s, "Error response from daemon: ") }

// firstError is the first blocking problem of a list.
func firstError(ps stacks.Problems) (stacks.Problem, bool) {
	for _, p := range ps {
		if p.Severity == stacks.SeverityError {
			return p, true
		}
	}
	return stacks.Problem{}, false
}

// isJob reports whether a service runs to its end (a replicated or global job) rather than staying
// up.
func isJob(sp convert.ServicePlan) bool {
	return sp.Spec.Mode.ReplicatedJob != nil || sp.Spec.Mode.GlobalJob != nil
}

// liveForceUpdate is the force-update counter of a service's spec as the swarm holds it, zero
// when the spec does not say.
func liveForceUpdate(spec json.RawMessage) uint64 {
	var s struct {
		TaskTemplate struct{ ForceUpdate uint64 }
	}
	if json.Unmarshal(spec, &s) != nil {
		return 0
	}
	return s.TaskTemplate.ForceUpdate
}

// itemEnd is how an item will be finished.
type itemEnd struct {
	state, code, message string
	result               map[string]string
}

func order(item string) int {
	for i, id := range DeployItems() {
		if id == item {
			return i
		}
	}
	return len(DeployItems())
}

// authFor chooses the registry credential an image is sent with, "" for none or no resolver.
func (d Deps) authFor(ctx context.Context, envID, image string) (string, error) {
	if d.Resolver == nil {
		return "", nil
	}
	ref, err := registry.ParseRef(image)
	if err != nil {
		// The engine will reject an unparsable reference with its own words; there is no
		// credential to choose for it.
		return "", nil //nolint:nilerr // see above
	}
	header, _, err := d.Resolver.AuthFor(ctx, envID, ref)
	return header, err
}

// clash reports whether the swarm holds an object under this stack's namespace that another
// stack, or none of Bayplan's, owns: a stack of that name that Bayplan does not manage (D-12).
// objects are the secrets and configs the deploy read; only those under the namespace count.
//
// What is foreign is decided by the Ownership predicate (plan 08-16, D-16): a network or service the stack owns is
// not, and neither is a network, secret or config an adopted stack recorded when it was adopted, whatever
// bayplan.stack.id it carries. A managed stack is judged by its own ID alone, as before.
func clash(own Ownership, nets []dockerx.NetworkRow, svcs []dockerx.ServiceDetail, objects []objectRow) bool {
	for _, n := range nets {
		if !own.OwnsNetwork(n.Name, n.Labels) && !own.Knows(rebuild.KindNetwork, n.Name) {
			return true
		}
	}
	for _, s := range svcs {
		if !own.OwnsWorkload(s.Labels) {
			return true
		}
	}
	for _, o := range objects {
		if o.Labels[stacks.LabelSwarmNamespace] == own.Name && !own.OwnsObject(o.Labels) && !own.Knows(string(o.Kind), o.Name) {
			return true
		}
	}
	return false
}

// hasObject reports whether the swarm already holds a secret or config of the kind under that name.
func hasObject(rows []objectRow, kind stacks.ObjectKind, name string) bool {
	_, ok := findObject(rows, kind, name)
	return ok
}

func hasExternal(p convert.Plan) bool {
	for _, n := range p.Networks {
		if n.External {
			return true
		}
	}
	return false
}

// codeOf classifies the error of one engine call into a deploy code.
func codeOf(ctx context.Context, err error) string {
	err = envs.ClassifyDockerError(err)
	var denied *envs.AgentPolicyDeniedError
	var rejected *dockerx.SpecRejectedError
	switch {
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return CodeCancelled
	case errors.Is(err, envs.ErrHostReadOnly):
		return CodeHostReadOnly
	case errors.Is(err, envs.ErrNodeUnavailable):
		return CodeNoManagerConnected
	case errors.As(err, &denied):
		return CodeAgentPolicyDenied
	case errors.Is(err, dockerx.ErrServiceChanged):
		return CodeServiceChanged
	case errors.Is(err, dockerx.ErrServiceNameTaken):
		return CodeServiceNameTaken
	case errors.As(err, &rejected), errors.Is(err, dockerx.ErrServiceSpecInvalid):
		return CodeServiceRejected
	}
	return CodeEngineError
}

// isConflict reports whether a network create was refused because the name exists.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "already exists") || strings.Contains(text, "conflict")
}
