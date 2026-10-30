// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/store/sqlc"
)

// ModeRemove is the Mode of a removal job and the kind of its row in stack_deploys.
const ModeRemove = DeployKindRemove

// The items of a removal job, in run order. The volume items are added by RemoveItemSpecs, one for each volume
// the person ticked.
const (
	ItemWait           = "wait"
	ItemRemoveNetworks = "remove-networks"
	ItemRemoveObjects  = "remove-objects"
	ItemDefinition     = "definition"

	idRemoveVolume = "remove-volume:"
)

// The codes a removal ends a failed item with, beside the ones a deploy uses.
const (
	// CodeTasksRemain ends the wait for the stack's tasks or containers to go: they were still there when the wait
	// ran out, so no network was removed under them (Pitfall 19).
	CodeTasksRemain = "tasks_remain"
	// CodeInUse is an object that something outside the stack still uses. It is left in place.
	CodeInUse = "in_use"
	// CodeVolumeNotOwned is a volume the person ticked that does not carry this stack's labels. It is never removed.
	CodeVolumeNotOwned = "volume_not_owned"
	// CodeDefinitionFailed ends the last step of a removal whose definition could not be deleted.
	CodeDefinitionFailed = "definition_failed"
)

func init() {
	codeMessages[CodeTasksRemain] = "Tasks were still running when Coaming stopped waiting, so nothing after this was removed. Docker may still finish stopping them."
	codeMessages[CodeInUse] = "Something outside this stack still uses it, so it was left in place."
	codeMessages[CodeVolumeNotOwned] = "That volume isn't one of this stack's, so it was left alone."
	codeMessages[CodeDefinitionFailed] = "The stack's definition could not be deleted."
}

// VolumeChoice is a volume of the stack on one node. A swarm stack has one for each node that holds the volume
// (a volume is local to a node), a standalone stack one.
type VolumeChoice struct {
	Name, NodeID, NodeName string
	// UsedOutside counts the containers on the node that mount the volume and are not the stack's own. Zero when
	// nothing does or when Coaming could not tell.
	UsedOutside int
}

// key is what tells two volumes of one stack apart.
func (v VolumeChoice) key() string { return v.NodeID + "/" + v.Name }

// RemovalPreview is what a removal of the stack would delete, read now: the objects the stack owns, the volumes it
// could also delete (each one is the person's to choose), and the objects its file uses that it never touches.
type RemovalPreview struct {
	// Services are the swarm services, Containers the containers of a standalone stack, by name.
	Services, Containers []string
	Networks             []string
	Secrets, Configs     []string
	// Volumes are the stack's own volumes, one for each node that holds one.
	Volumes []VolumeChoice
	// Externals are the networks, secrets, configs and volumes the compose file names as external, by the name it
	// gives. A removal never touches them.
	Externals []string
	// NotReporting names the nodes whose volumes could not be read, so their volumes are not listed.
	NotReporting []string
	// HasDraft is true when the stack has a saved draft that a removal deletes with the definition. The caller
	// fills it: the draft is stored, not on the engine.
	HasDraft bool
}

// ownedStack is what a stack owns on its engine, read once.
type ownedStack struct {
	services   []dockerx.ServiceDetail
	containers []dockerx.ComposeContainer
	networks   []dockerx.NetworkRow
	secrets    []objectRow
	configs    []objectRow
	// volumes are the stack's own volumes by node, and anyVolume every volume the nodes hold, which tells a volume
	// that exists under another owner from one that is gone.
	volumes      []VolumeChoice
	anyVolume    map[string]bool
	notReporting []string
}

// readOwned reads what the stack owns: its services or containers, its networks, and on a swarm its secrets and
// configs, each only when it carries the stack's own labels (a name is never ownership). With volumes it also reads
// the volumes of every node, fresh, and counts the containers outside the stack that mount each.
func (d Deps) readOwned(ctx context.Context, envID string, st Stack, volumes bool) (ownedStack, error) {
	var out ownedStack
	own := OwnershipOf(st)
	if st.Target == stacks.TargetStandalone {
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return out, err
		}
		err = d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
			cs, e := c.ComposeContainers(ctx, st.Name)
			if e != nil {
				return e
			}
			// The stack's own containers and the ones someone added to its project from the host CLI (docker compose up
			// --scale, no coaming.stack.id) are what a removal takes; another stack's and a project the stack owns none
			// of are not (SplitProject, G-08-6).
			mine, added, _ := SplitProject(own, cs, func(c dockerx.ComposeContainer) map[string]string { return c.Labels })
			out.containers = append(mine, added...)
			nets, e := c.NetworksWithLabel(ctx, own.labelKey(), own.Name)
			if e != nil {
				return e
			}
			for _, n := range nets {
				if own.OwnsNetwork(n.Name, n.Labels) {
					out.networks = append(out.networks, n)
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}
	} else {
		if d.Clusters == nil {
			return out, errors.New("stackops: no cluster reader to read the swarm with")
		}
		err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
			out.services, out.networks, out.secrets, out.configs = nil, nil, nil, nil
			svcs, e := c.ServicesWithLabel(ctx, stacks.LabelSwarmNamespace, st.Name)
			if e != nil {
				return e
			}
			for _, s := range svcs {
				if own.OwnsWorkload(s.Labels) {
					out.services = append(out.services, s)
				}
			}
			nets, e := c.NetworksWithLabel(ctx, own.labelKey(), own.Name)
			if e != nil {
				return e
			}
			for _, n := range nets {
				if own.OwnsNetwork(n.Name, n.Labels) {
					out.networks = append(out.networks, n)
				}
			}
			secrets, e := c.SecretsWithLabel(ctx, stacks.LabelSwarmNamespace, st.Name)
			if e != nil {
				return e
			}
			for _, o := range secretRows(secrets) {
				if own.OwnsObject(o.Labels) {
					out.secrets = append(out.secrets, o)
				}
			}
			configs, e := c.ConfigsWithLabel(ctx, stacks.LabelSwarmNamespace, st.Name)
			if e != nil {
				return e
			}
			for _, o := range configRows(configs) {
				if own.OwnsObject(o.Labels) {
					out.configs = append(out.configs, o)
				}
			}
			return nil
		})
		if err != nil {
			return out, err
		}
	}
	if !volumes {
		return out, nil
	}
	agg, _, err := d.Aggregator.FreshSnapshot(ctx, envID)
	if err != nil {
		return out, err
	}
	out.notReporting = append([]string{}, agg.NotReporting...)
	out.anyVolume = map[string]bool{}
	// A container outside the stack is one that is not of the stack: on a host that leaves out the containers the host
	// CLI added to the stack's project, which a removal takes with it.
	inStack := func(c envs.ContainerRow) bool { return own.OwnsWorkload(c.Labels) }
	if st.Target == stacks.TargetStandalone {
		mine, added, _ := SplitProject(own, agg.Containers, func(c envs.ContainerRow) map[string]string { return c.Labels })
		ids := make(map[string]bool, len(mine)+len(added))
		for _, c := range append(mine, added...) {
			ids[c.ID] = true
		}
		inStack = func(c envs.ContainerRow) bool { return ids[c.ID] }
	}
	for _, v := range agg.Volumes {
		out.anyVolume[v.NodeID+"/"+v.Name] = true
		if !own.OwnsVolume(v.Name, v.Labels) {
			continue
		}
		choice := VolumeChoice{Name: v.Name, NodeID: v.NodeID, NodeName: v.Node}
		for _, c := range agg.Containers {
			if c.NodeID != v.NodeID || inStack(c) {
				continue
			}
			if slices.ContainsFunc(c.Mounts, func(m dockerx.MountRow) bool { return m.Type == "volume" && m.Name == v.Name }) {
				choice.UsedOutside++
			}
		}
		out.volumes = append(out.volumes, choice)
	}
	slices.SortFunc(out.volumes, func(a, b VolumeChoice) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.NodeName, b.NodeName), cmp.Compare(a.NodeID, b.NodeID))
	})
	return out, nil
}

// Preview reads what a removal of the stack would delete. decl is what the deployed revision refers to
// (Repo.Declared): the externals it names are listed so the person sees what stays. It is a read.
func (d Deps) Preview(ctx context.Context, envID string, st Stack, decl Declared) (RemovalPreview, error) {
	owned, err := d.readOwned(ctx, envID, st, true)
	if err != nil {
		return RemovalPreview{}, err
	}
	out := RemovalPreview{
		Services: []string{}, Containers: []string{}, Networks: []string{}, Secrets: []string{}, Configs: []string{},
		Volumes: owned.volumes, Externals: []string{}, NotReporting: owned.notReporting,
	}
	if out.Volumes == nil {
		out.Volumes = []VolumeChoice{}
	}
	for _, s := range owned.services {
		out.Services = append(out.Services, s.Name)
	}
	for _, c := range owned.containers {
		out.Containers = append(out.Containers, strings.TrimPrefix(c.Name, "/"))
	}
	for _, n := range owned.networks {
		out.Networks = append(out.Networks, n.Name)
	}
	for _, o := range owned.secrets {
		out.Secrets = append(out.Secrets, o.Name)
	}
	for _, o := range owned.configs {
		out.Configs = append(out.Configs, o.Name)
	}
	for _, group := range [][]DeclaredObject{decl.Networks, decl.Volumes, decl.Secrets, decl.Configs} {
		for _, o := range group {
			if o.External {
				out.Externals = append(out.Externals, cmp.Or(o.Name, o.Key))
			}
		}
	}
	for _, list := range []*[]string{&out.Services, &out.Containers, &out.Networks, &out.Secrets, &out.Configs, &out.Externals} {
		slices.Sort(*list)
		*list = slices.Compact(*list)
	}
	return out, nil
}

// RemoveStore is the stored side of a removal: the stack's definition, which a removal deletes unless the person
// keeps it, and its record of being stopped, which a kept definition no longer has. Repo is the one implementation.
type RemoveStore interface {
	// DeleteStackWithAutomations deletes the stack with its drafts, revisions, files, variables and operations, and
	// forgets the automations that run for it (its webhook and its auto-deploy), in one transaction.
	DeleteStackWithAutomations(ctx context.Context, stackID string) error
	// SetStopped records that the stack is stopped (true) or not (false).
	SetStopped(ctx context.Context, stackID string, stopped bool) error
}

var _ RemoveStore = Repo{}

// The kinds of the automations a git stack can own (internal/gitsync KindGitWebhook and KindGitAutoDeploy; a test keeps
// them equal). They are rows of owned_resources, which has no foreign key to stacks, so a removal forgets them itself.
const (
	ownedKindGitWebhook    = "git_webhook"
	ownedKindGitAutoDeploy = "git_autodeploy"
)

// DeleteStackWithAutomations deletes the stack and the owned_resources rows of its webhook and auto-deploy in one
// transaction, so a removed stack leaves no automation behind for its owner to see, be paused over or hand over. A stack
// that is not there is ErrStackNotFound and changes nothing, so a second removal does nothing.
func (r Repo) DeleteStackWithAutomations(ctx context.Context, stackID string) error {
	tx, err := r.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := r.Store.Q.WithTx(tx)
	n, err := q.DeleteStack(ctx, stackID)
	if err != nil {
		return fmt.Errorf("delete stack: %w", err)
	}
	if n != 1 {
		return ErrStackNotFound
	}
	if _, err := q.DeleteOwnedResourcesOfStack(ctx, sqlc.DeleteOwnedResourcesOfStackParams{
		ResourceID: stackID, KindA: ownedKindGitWebhook, KindB: ownedKindGitAutoDeploy,
	}); err != nil {
		return fmt.Errorf("forget the stack's automations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// RemoveInput is a stack to remove: the environment, the node of a standalone host, the stack, the volumes the
// person ticked (and only those are ever removed), whether the definition stays, who asks and the store.
type RemoveInput struct {
	EnvID string
	// NodeID is the node of a standalone host the stack runs on. A swarm stack leaves it empty.
	NodeID  string
	Stack   Stack
	Volumes []VolumeChoice
	// KeepDefinition keeps the stack's files, variables and revisions: the stack then reads Not deployed.
	KeepDefinition bool
	Caller         Caller
	Store          RemoveStore
	// Masker hides the stack's secret values in every text a remote system sent that the job keeps.
	Masker stacks.Masker
	// OnDone, when set, is called once with the result before the job's last item is finished, so a job that
	// reads as finished has already been recorded by the caller.
	OnDone func(RemoveResult)
}

// RemoveResult is what a removal did, as far as it got.
type RemoveResult struct {
	// Services counts the services or containers removed, Networks the networks, Secrets and Configs the objects.
	Services, Networks, Secrets, Configs int
	// Volumes counts the volumes removed, KeptVolumes the stack's volumes that are still on their hosts: the ones
	// nobody ticked and the ones that could not be removed.
	Volumes, KeptVolumes int
	// DefinitionDeleted is true when the stack's record is gone, DefinitionKept when the person kept it.
	DefinitionDeleted, DefinitionKept bool
	// Code is empty when the removal worked and CodeCancelled when it was cancelled; FailedStep names the step the
	// first failure is about and FailedName the object, when there is one.
	Code, FailedStep, FailedName string
}

// OK reports whether the removal ended without a failure.
func (r RemoveResult) OK() bool { return r.Code == "" }

// RemoveItemSpecs are the items a removal job starts with, in run order: the check, the services or containers,
// the wait for them to go, the networks, on a swarm the secrets and configs, one item for each ticked volume, and
// the deletion of the definition unless it is kept.
func RemoveItemSpecs(target stacks.Target, volumes []VolumeChoice, keep bool) []jobs.ItemSpec {
	remove, wait := "Remove services", "Wait for tasks to stop"
	if target == stacks.TargetStandalone {
		remove, wait = "Remove containers", "Wait for containers to stop"
	}
	specs := []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the stack", Kind: KindStep},
		{ID: ItemServices, Label: remove, Kind: KindStep},
		{ID: ItemWait, Label: wait, Kind: KindStep},
		{ID: ItemRemoveNetworks, Label: "Remove networks", Kind: KindStep},
	}
	if target != stacks.TargetStandalone {
		specs = append(specs, jobs.ItemSpec{ID: ItemRemoveObjects, Label: "Remove secrets and configs", Kind: KindStep})
	}
	for _, v := range volumes {
		label := "Remove volume " + v.Name
		if target != stacks.TargetStandalone && v.NodeName != "" {
			label += " on " + v.NodeName
		}
		specs = append(specs, jobs.ItemSpec{ID: idRemoveVolume + v.key(), Label: label, Kind: KindStep})
	}
	if !keep {
		specs = append(specs, jobs.ItemSpec{ID: ItemDefinition, Label: "Delete the definition", Kind: KindStep})
	}
	return specs
}

// failure is the first object of a step that could not be removed, kept until the step ends.
type failure struct {
	code, name string
	err        error
}

// removal is one removal in progress: it reports on the job, remembers which items have ended and how the last
// one will, which is only after the caller has recorded the result.
type removal struct {
	d      Deps
	in     RemoveInput
	r      *jobs.Reporter
	mask   func(string) string
	order  []string
	labels map[string]string
	ended  map[string]bool
	last   string
	final  itemEnd
	res    RemoveResult
	// ownedVolumes is how many volumes the stack has on its nodes, read by the check.
	ownedVolumes int
	// serviceIDs are the IDs of the swarm services the check found the stack owns: the tasks that outlive a removed
	// service are the stack's by them.
	serviceIDs map[string]bool
	// containerIDs are the IDs of the containers the check found a standalone removal takes, the stack's own and the
	// ones the host CLI added: the wait keeps counting them by ID after the stack's own are gone.
	containerIDs map[string]bool
	// failures are the first object of each step that stayed, until the step ends.
	failures map[string]failure
}

func (d Deps) newRemoval(in RemoveInput, r *jobs.Reporter) *removal {
	specs := RemoveItemSpecs(in.Stack.Target, in.Volumes, in.KeepDefinition)
	x := &removal{d: d, in: in, r: r, mask: in.Masker.Mask, ended: map[string]bool{}, labels: map[string]string{}, final: itemEnd{state: jobs.ItemDone}}
	for _, s := range specs {
		x.order = append(x.order, s.ID)
		x.labels[s.ID] = s.Label
	}
	x.last = x.order[len(x.order)-1]
	return x
}

// end tells the caller the result and finishes the last item.
func (x *removal) end() {
	x.res.DefinitionKept = x.in.KeepDefinition
	x.res.KeptVolumes = max(0, x.ownedVolumes-x.res.Volumes)
	if x.in.OnDone != nil {
		x.in.OnDone(x.res)
	}
	x.r.Finish(x.last, x.final.state, x.final.code, x.final.message, x.final.result)
}

func (x *removal) begin(id string) {
	x.r.Update(id, func(it *jobs.Item) { it.State = jobs.ItemRunning })
}

// finish ends an item. The last item is only remembered: end finishes it after the caller has recorded the result.
func (x *removal) finish(id, state, code, message string, result map[string]string) {
	x.ended[id] = true
	if id == x.last {
		x.final = itemEnd{state: state, code: code, message: message, result: result}
		return
	}
	x.r.Finish(id, state, code, message, result)
}

func (x *removal) done(id string, result map[string]string) {
	x.finish(id, jobs.ItemDone, "", "", result)
}

func (x *removal) line(text string) { x.r.Output(x.mask(text)) }

// fail ends an item as failed with a code and the engine's words, and notes the first failure of the removal.
func (x *removal) fail(id, code, name string, err error, result map[string]string) {
	if x.res.Code == "" {
		x.res.Code, x.res.FailedStep, x.res.FailedName = code, id, name
	}
	if err != nil {
		if words := engineWords(err); words != "" {
			x.r.Detail(id, x.mask(words))
			x.line("Failed: " + x.labels[id] + ": " + words)
		}
	}
	// The step says which object it could not remove, so the page can name it beside the sentence.
	if name != "" {
		withName := map[string]string{"name": name}
		for k, v := range result {
			withName[k] = v
		}
		result = withName
	}
	x.finish(id, jobs.ItemFailed, code, CodeMessage(code), result)
}

// skipRest ends every item that has not ended as not run.
func (x *removal) skipRest() {
	for _, id := range x.order {
		if !x.ended[id] {
			x.finish(id, jobs.ItemSkipped, CodeNotRun, CodeMessage(CodeNotRun), nil)
		}
	}
}

// cancelled ends the item that was running and every one after it as cancelled.
func (x *removal) cancelled() {
	if x.res.Code == "" {
		x.res.Code = CodeCancelled
	}
	for _, id := range x.order {
		if !x.ended[id] {
			x.finish(id, jobs.ItemCancelled, CodeCancelled, CodeMessage(CodeCancelled), nil)
		}
	}
}

// Remove deletes everything the stack owns, in an order that never removes a network under a task that is still
// stopping (STK-11, D-20): the services or containers, then a bounded wait until the stack's tasks or containers
// are gone, then the networks, on a swarm the secrets and configs, then the volumes the person ticked, then the
// definition. Only objects that carry the stack's own labels are removed; an external network, secret, config or
// volume is never touched, and a volume only when it is ticked and is the stack's on its node. A failure ends the
// steps that depend on the one that failed; what was removed stays removed, and the definition is kept so the
// person can remove the rest from the stack's page. Removing what is already gone is a success. Every write is
// made once and never retried (P2 D-42).
//
// The last item is finished only after in.OnDone returns.
func (d Deps) Remove(ctx context.Context, in RemoveInput, r *jobs.Reporter) (res RemoveResult) {
	x := d.newRemoval(in, r)
	// end completes the result (the counts of what stayed), tells the caller and finishes the last item.
	defer func() { x.end(); res = x.res }()
	owned, ok := x.check(ctx)
	if !ok {
		return x.res
	}
	if !x.removeWorkload(ctx, owned) || !x.waitGone(ctx) {
		x.skipRest()
		return x.res
	}
	// The steps from here on do not depend on each other: a network that stays does not stop the secrets or the
	// volumes from being removed. A cancel ends them all.
	steps := []func(){func() { x.removeNetworks(ctx) }}
	if in.Stack.Target != stacks.TargetStandalone {
		steps = append(steps, func() { x.removeObjects(ctx) })
	}
	steps = append(steps, func() { x.removeVolumes(ctx) })
	for _, step := range steps {
		step()
		if x.res.Code == CodeCancelled {
			return x.res
		}
	}
	x.conclude(ctx)
	return x.res
}

// check reads what the stack owns and refuses a removal that would touch something that is not the stack's: a
// ticked volume that exists on its node under another owner, a host that is read-only, a Compose project of the
// same name that Coaming does not manage.
func (x *removal) check(ctx context.Context) (ownedStack, bool) {
	d, in := x.d, x.in
	x.begin(ItemCheck)
	if in.Stack.Target == stacks.TargetStandalone {
		if err := d.writable(ctx, in.EnvID, in.NodeID); err != nil {
			if errors.Is(err, envs.ErrHostReadOnly) {
				x.fail(ItemCheck, CodeHostReadOnly, "", nil, nil)
			} else {
				d.log().Warn("a standalone host could not be written to before a removal", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
				x.fail(ItemCheck, codeOf(ctx, err), "", err, nil)
			}
			x.skipRest()
			return ownedStack{}, false
		}
		live, err := d.readStandalone(ctx, in.EnvID, in.NodeID, in.Stack.Name)
		if err == nil && foreignLive(live, OwnershipOf(in.Stack)) {
			x.fail(ItemCheck, CodeNameClash, "", nil, nil)
			x.skipRest()
			return ownedStack{}, false
		}
		if err != nil {
			d.log().Warn("could not read a standalone host before a removal", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
			x.fail(ItemCheck, codeOf(ctx, err), "", err, nil)
			x.skipRest()
			return ownedStack{}, false
		}
	}
	owned, err := d.readOwned(ctx, in.EnvID, in.Stack, true)
	if err != nil {
		d.log().Warn("could not read a stack before a removal", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		x.fail(ItemCheck, codeOf(ctx, err), "", err, nil)
		x.skipRest()
		return ownedStack{}, false
	}
	ours := map[string]bool{}
	for _, v := range owned.volumes {
		ours[v.key()] = true
	}
	for _, v := range in.Volumes {
		if !ours[v.key()] && owned.anyVolume[v.key()] {
			// It exists on that node and is not this stack's: never removed on the strength of a request.
			x.fail(ItemCheck, CodeVolumeNotOwned, v.Name, nil, nil)
			x.skipRest()
			return ownedStack{}, false
		}
	}
	x.ownedVolumes = len(owned.volumes)
	x.containerIDs = make(map[string]bool, len(owned.containers))
	for _, c := range owned.containers {
		x.containerIDs[c.ID] = true
	}
	x.serviceIDs = make(map[string]bool, len(owned.services))
	for _, s := range owned.services {
		x.serviceIDs[s.ID] = true
	}
	x.line(fmt.Sprintf("Found %d services or containers, %d networks, %d secrets and %d configs", len(owned.services)+len(owned.containers), len(owned.networks), len(owned.secrets), len(owned.configs)))
	x.done(ItemCheck, nil)
	return owned, true
}

// removeWorkload removes the services of a swarm stack, or runs the Compose SDK's down on a standalone one
// (dockerx ComposeDown(ctx, project), through Deps.downProject), which removes the containers and the networks of
// the project and never a volume or an external network. It reports false when it failed.
func (x *removal) removeWorkload(ctx context.Context, owned ownedStack) bool {
	d, in := x.d, x.in
	x.begin(ItemServices)
	if in.Stack.Target == stacks.TargetStandalone {
		if len(owned.containers) == 0 && len(owned.networks) == 0 {
			x.line("No containers of " + in.Stack.Name + " were found")
			x.done(ItemServices, map[string]string{"removed": "0"})
			return true
		}
		if ctx.Err() != nil {
			x.cancelled()
			return false
		}
		// The SDK acts on the whole Compose project, so it runs on the job's context: a cancel stops it where it is.
		err := d.Aggregator.WriteOnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
			return d.downProject(ctx, c, in.Stack.Name)
		})
		if err != nil {
			d.log().Warn("the Compose SDK's down failed", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
			x.fail(ItemServices, codeOf(ctx, err), "", err, nil)
			return false
		}
		x.res.Services = len(owned.containers)
		x.line("Removed " + strconv.Itoa(len(owned.containers)) + " containers")
		x.done(ItemServices, map[string]string{"removed": strconv.Itoa(len(owned.containers))})
		return true
	}
	for _, s := range owned.services {
		if ctx.Err() != nil {
			x.cancelled()
			return false
		}
		err := d.write(ctx, in.EnvID, func(wctx context.Context, c *dockerx.Client) error { return c.ServiceRemove(wctx, s.ID) })
		switch {
		case err == nil:
			x.res.Services++
			x.line("Removed service " + s.Name)
		case errors.Is(err, dockerx.ErrServiceNotFound):
			x.line("Service " + s.Name + " was already gone")
		default:
			d.log().Warn("could not remove a stack service", "environment", in.EnvID, "stack", in.Stack.Name, "service", s.Name, "error", err)
			x.fail(ItemServices, codeOf(ctx, err), s.Name, err, map[string]string{"removed": strconv.Itoa(x.res.Services)})
			return false
		}
	}
	x.done(ItemServices, map[string]string{"removed": strconv.Itoa(x.res.Services)})
	return true
}

// terminalTask is a task state that holds nothing: the task has ended, so no container of it is attached to a
// network.
func terminalTask(state string) bool {
	switch state {
	case "complete", "failed", "shutdown", "rejected", "orphaned", "remove":
		return true
	}
	return false
}

// remaining counts what the stack still holds on its target after its services are gone. On a swarm that is
// its tasks that have not ended (the swarm keeps a task after its service is removed until the node has stopped
// the container, and the task still carries the labels its service had) and the containers of those tasks that
// the nodes have not removed yet: a network or a volume is in use until they are. On a standalone host it is the
// containers of the Compose project.
func (x *removal) remaining(ctx context.Context) (int, error) {
	d, in := x.d, x.in
	own := OwnershipOf(in.Stack)
	rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
	defer cancel()
	if in.Stack.Target == stacks.TargetStandalone {
		var n int
		err := d.Aggregator.OnNode(rctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
			// The project's containers, as the SDK counts them: a one-off container (compose run) is not one of its services.
			// The stack's own and the ones added from the host CLI are waited for; another stack's are not. Once the
			// stack's own are gone a container the host CLI added no longer has one beside it to be told apart by, so
			// the containers the check found stay counted by their ID.
			cs, e := c.ComposeContainers(rctx, in.Stack.Name)
			if e != nil {
				return e
			}
			mine, added, _ := SplitProject(own, cs, func(c dockerx.ComposeContainer) map[string]string { return c.Labels })
			counted := make(map[string]bool, len(mine)+len(added))
			for _, row := range slices.Concat(mine, added) {
				counted[row.ID] = true
			}
			n = len(counted)
			for _, row := range cs {
				if x.containerIDs[row.ID] && !counted[row.ID] {
					n++
				}
			}
			return nil
		})
		return n, err
	}
	var tasks int
	err := d.Clusters.ClusterRead(rctx, in.EnvID, func(c *dockerx.Client) error {
		// A task keeps the labels its service had, so the namespace finds the tasks of a service that is gone; a task is
		// the stack's when its service was one the check found the stack owns, since a task row names no owner.
		rows, e := c.Tasks(rctx, dockerx.TaskFilter{Labels: []string{own.labelKey() + "=" + own.Name}})
		if e != nil {
			return e
		}
		tasks = 0
		for _, t := range rows {
			if x.serviceIDs[t.ServiceID] && !terminalTask(t.State) {
				tasks++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	// The nodes that answer are the ones whose containers can be looked at; a node nobody reaches is not waited for.
	agg, _, err := d.Aggregator.Snapshot(rctx, in.EnvID)
	if err != nil {
		return 0, err
	}
	containers := 0
	for _, n := range agg.Nodes {
		c, e := x.containersOn(rctx, n.ID)
		if e != nil {
			return 0, e
		}
		containers += c
	}
	return tasks + containers, nil
}

// containersOn counts the containers of the swarm stack on one node, in any state: they are found by the stack's
// namespace and kept only when they also carry its Coaming ID.
func (x *removal) containersOn(ctx context.Context, nodeID string) (int, error) {
	var n int
	err := x.d.Aggregator.OnNode(ctx, x.in.EnvID, nodeID, func(c *dockerx.Client) error {
		rows, e := c.ContainersWithLabel(ctx, stacks.LabelSwarmNamespace, x.in.Stack.Name)
		if e != nil {
			return e
		}
		n = 0
		own := OwnershipOf(x.in.Stack)
		for _, row := range rows {
			if own.OwnsWorkload(row.Labels) {
				n++
			}
		}
		return nil
	})
	return n, err
}

// waitGone polls until the stack's tasks or containers are gone, until the convergence timeout has passed on the
// clock (CodeTasksRemain), until ctx ends or until reads have failed maxPollErrors times in a row. The item shows
// how many are left. It reports false when it failed. It writes nothing.
func (x *removal) waitGone(ctx context.Context) bool {
	d := x.d
	x.begin(ItemWait)
	clock := d.clock()
	deadline := clock.Now().Add(d.convergeTimeout())
	failures := 0
	for {
		if ctx.Err() != nil {
			x.cancelled()
			return false
		}
		n, err := x.remaining(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				x.cancelled()
				return false
			}
			if failures++; failures >= maxPollErrors {
				d.log().Warn("could not read what is left of a stack to remove", "environment", x.in.EnvID, "stack", x.in.Stack.Name, "error", err)
				x.fail(ItemWait, codeOf(ctx, err), "", err, nil)
				return false
			}
		case n == 0:
			x.done(ItemWait, map[string]string{"left": "0"})
			return true
		default:
			failures = 0
			x.r.Update(ItemWait, func(it *jobs.Item) { it.Result = map[string]string{"left": strconv.Itoa(n)} })
		}
		if !clock.Now().Before(deadline) {
			x.fail(ItemWait, CodeTasksRemain, "", nil, map[string]string{"left": strconv.Itoa(n)})
			return false
		}
		select {
		case <-ctx.Done():
			x.cancelled()
			return false
		case <-clock.After(PollInterval):
		}
	}
}

// writeEngine runs one write on the engine that holds the stack's networks, secrets and configs: a manager for a
// swarm, the host for a standalone stack. It is made once, on a context a cancel cannot reach.
func (x *removal) writeEngine(ctx context.Context, fn func(ctx context.Context, c *dockerx.Client) error) error {
	if x.in.Stack.Target == stacks.TargetStandalone {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		defer cancel()
		return x.d.Aggregator.WriteOnNode(wctx, x.in.EnvID, x.in.NodeID, func(c *dockerx.Client) error { return fn(wctx, c) })
	}
	return x.d.write(ctx, x.in.EnvID, fn)
}

// removeNetworks removes the networks that carry the stack's labels, read again now that its tasks are gone. A
// network something still uses is left in place and named; the others are still removed.
func (x *removal) removeNetworks(ctx context.Context) {
	d, in := x.d, x.in
	x.begin(ItemRemoveNetworks)
	owned, err := d.readOwned(ctx, in.EnvID, in.Stack, false)
	if err != nil {
		d.log().Warn("could not read a stack's networks before removing them", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		x.fail(ItemRemoveNetworks, codeOf(ctx, err), "", err, nil)
		return
	}
	removed, left := 0, 0
	for _, n := range owned.networks {
		if ctx.Err() != nil {
			x.cancelled()
			return
		}
		err := x.writeEngine(ctx, func(wctx context.Context, c *dockerx.Client) error { return c.NetworkRemove(wctx, n.ID) })
		switch {
		case err == nil:
			removed++
			x.line("Removed network " + n.Name)
		case dockerx.IsNotFound(err):
			x.line("Network " + n.Name + " was already gone")
		case dockerx.IsNetworkInUse(err):
			left++
			x.line("Left network " + n.Name + " in place: something still uses it")
			x.noteFailure(ItemRemoveNetworks, CodeInUse, n.Name, err)
		default:
			left++
			d.log().Warn("could not remove a stack network", "environment", in.EnvID, "stack", in.Stack.Name, "network", n.Name, "error", err)
			x.noteFailure(ItemRemoveNetworks, CodeRemoveFailed, n.Name, err)
		}
	}
	x.res.Networks = removed
	x.endGroup(ItemRemoveNetworks, removed, left)
}

// noteFailure remembers the first object of a step that stayed, so the step can end naming it.
func (x *removal) noteFailure(step, code, name string, err error) {
	if x.failures == nil {
		x.failures = map[string]failure{}
	}
	if _, ok := x.failures[step]; !ok {
		x.failures[step] = failure{code: code, name: name, err: err}
	}
}

// endGroup ends a step that removed several objects: done when none stayed, failed with the first object that did.
func (x *removal) endGroup(step string, removed, left int) {
	result := map[string]string{"removed": strconv.Itoa(removed), "left": strconv.Itoa(left)}
	if f, ok := x.failures[step]; ok {
		x.fail(step, f.code, f.name, f.err, result)
		return
	}
	x.done(step, result)
}

// removeObjects removes the secrets and configs that carry the stack's labels. One a service outside the stack
// still uses is left in place and named.
func (x *removal) removeObjects(ctx context.Context) {
	d, in := x.d, x.in
	x.begin(ItemRemoveObjects)
	owned, err := d.readOwned(ctx, in.EnvID, in.Stack, false)
	if err != nil {
		d.log().Warn("could not read a stack's secrets and configs before removing them", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		x.fail(ItemRemoveObjects, codeOf(ctx, err), "", err, nil)
		return
	}
	removed, left := 0, 0
	for _, o := range append(slices.Clone(owned.secrets), owned.configs...) {
		if ctx.Err() != nil {
			x.cancelled()
			return
		}
		word := "config"
		if o.Kind == stacks.KindSecret {
			word = "secret"
		}
		err := x.writeEngine(ctx, func(wctx context.Context, c *dockerx.Client) error {
			if o.Kind == stacks.KindSecret {
				return c.SecretRemove(wctx, o.ID)
			}
			return c.ConfigRemove(wctx, o.ID)
		})
		switch {
		case err == nil:
			removed++
			if o.Kind == stacks.KindSecret {
				x.res.Secrets++
			} else {
				x.res.Configs++
			}
			x.line("Removed " + word + " " + o.Name)
		case errors.Is(err, dockerx.ErrObjectNotFound):
			x.line(strings.ToUpper(word[:1]) + word[1:] + " " + o.Name + " was already gone")
		case errors.Is(err, dockerx.ErrObjectInUse):
			left++
			x.line("Left " + word + " " + o.Name + " in place: a service uses it")
			x.noteFailure(ItemRemoveObjects, CodeInUse, o.Name, err)
		default:
			left++
			d.log().Warn("could not remove a stack object", "environment", in.EnvID, "stack", in.Stack.Name, "object", o.Name, "error", err)
			x.noteFailure(ItemRemoveObjects, CodeRemoveFailed, o.Name, err)
		}
	}
	x.endGroup(ItemRemoveObjects, removed, left)
}

// errVolumeNotOwned is what the volume step answers when the volume on the node is not the stack's.
var errVolumeNotOwned = errors.New("stackops: the volume is not the stack's")

// volumeBusyError is what the volume step answers when something outside the stack mounts the volume.
type volumeBusyError struct{ users []string }

func (e *volumeBusyError) Error() string {
	return "stackops: volume is in use by " + strings.Join(e.users, ", ")
}

// removeVolumes removes the volumes the person ticked, each on its own node, and no other. Before each removal the
// node is asked again whether the volume carries the stack's labels and whether any container, or on a swarm any
// service, mounts it; a volume used outside the stack is left, named, and the others are still removed.
func (x *removal) removeVolumes(ctx context.Context) {
	d, in := x.d, x.in
	own := OwnershipOf(in.Stack)
	for _, v := range in.Volumes {
		id := idRemoveVolume + v.key()
		if ctx.Err() != nil {
			x.cancelled()
			return
		}
		x.begin(id)
		var serviceUsers []string
		if in.Stack.Target != stacks.TargetStandalone {
			users, err := x.serviceUsers(ctx, v.Name)
			if err != nil {
				x.fail(id, CodeNoManagerConnected, v.Name, err, nil)
				continue
			}
			serviceUsers = users
		}
		removed := false
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
		err := d.Aggregator.WriteOnNode(wctx, in.EnvID, v.NodeID, func(c *dockerx.Client) error {
			mine, e := c.VolumesWithLabel(wctx, own.labelKey(), own.Name)
			if e != nil {
				return e
			}
			if !slices.ContainsFunc(mine, func(row dockerx.VolumeRow) bool { return row.Name == v.Name && own.OwnsVolume(row.Name, row.Labels) }) {
				_, exists, e := c.VolumeDriver(wctx, v.Name)
				if e != nil {
					return e
				}
				if exists {
					return errVolumeNotOwned
				}
				return nil // already gone
			}
			containers, e := c.VolumeUsers(wctx, v.Name)
			if e != nil {
				return e
			}
			var users []string
			for _, name := range containers {
				users = append(users, "container "+name)
			}
			for _, name := range serviceUsers {
				users = append(users, "service "+name)
			}
			if len(users) > 0 {
				return &volumeBusyError{users: users}
			}
			if e := c.VolumeRemove(wctx, v.Name); e != nil {
				if dockerx.IsNotFound(e) {
					return nil
				}
				return e
			}
			removed = true
			return nil
		})
		cancel()
		var busy *volumeBusyError
		switch {
		case err == nil && removed:
			x.res.Volumes++
			x.line("Removed volume " + v.Name + " on " + cmp.Or(v.NodeName, v.NodeID))
			x.done(id, map[string]string{"removed": "true"})
		case err == nil:
			x.line("Volume " + v.Name + " was already gone")
			x.done(id, map[string]string{"removed": "false"})
		case errors.Is(err, errVolumeNotOwned):
			x.fail(id, CodeVolumeNotOwned, v.Name, nil, nil)
		case errors.As(err, &busy), dockerx.IsConflict(err):
			x.line("Left volume " + v.Name + " in place: something still uses it")
			x.fail(id, CodeInUse, v.Name, nil, map[string]string{"users": strconv.Itoa(len(busyUsers(err)))})
		default:
			d.log().Warn("could not remove a stack volume", "environment", in.EnvID, "stack", in.Stack.Name, "volume", v.Name, "error", err)
			x.fail(id, codeOf(ctx, err), v.Name, err, nil)
		}
	}
}

// busyUsers are the users a volumeBusyError names, none for any other error.
func busyUsers(err error) []string {
	var busy *volumeBusyError
	if errors.As(err, &busy) {
		return busy.users
	}
	return nil
}

// serviceUsers names the services of the swarm that mount the volume, read from a manager now. A service spec
// counts even when it has no running task.
func (x *removal) serviceUsers(ctx context.Context, name string) ([]string, error) {
	if x.d.Clusters == nil {
		return nil, errors.New("stackops: no cluster reader to read the swarm with")
	}
	rctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var refs []dockerx.ServiceRef
	if err := x.d.Clusters.ClusterRead(rctx, x.in.EnvID, func(c *dockerx.Client) error {
		var e error
		refs, e = c.ServiceRefs(rctx)
		return e
	}); err != nil {
		return nil, err
	}
	var users []string
	for _, ref := range refs {
		if slices.Contains(ref.Volumes, name) {
			users = append(users, ref.Name)
		}
	}
	return users, nil
}

// conclude ends the removal: it counts the volumes that stayed, then deletes the definition, or keeps it, when
// every step worked. A removal that failed anywhere keeps the definition so the person can remove the rest from the
// stack's page.
func (x *removal) conclude(ctx context.Context) {
	d, in := x.d, x.in
	if !x.res.OK() {
		x.skipRest()
		return
	}
	wctx := context.WithoutCancel(ctx)
	if in.KeepDefinition {
		if in.Store != nil {
			if err := in.Store.SetStopped(wctx, in.Stack.ID, false); err != nil {
				d.log().Warn("could not clear a kept stack's stopped record", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
			}
		}
		return
	}
	x.begin(ItemDefinition)
	if in.Store == nil {
		x.fail(ItemDefinition, CodeDefinitionFailed, "", errors.New("stackops: no store to delete the definition from"), nil)
		return
	}
	if err := in.Store.DeleteStackWithAutomations(wctx, in.Stack.ID); err != nil {
		d.log().Error("could not delete a removed stack's definition", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		x.fail(ItemDefinition, CodeDefinitionFailed, "", nil, nil)
		return
	}
	x.res.DefinitionDeleted = true
	x.line("Deleted the definition of " + in.Stack.Name)
	x.done(ItemDefinition, nil)
}
