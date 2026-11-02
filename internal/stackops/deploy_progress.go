// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"maps"
	"strconv"
	"sync"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

// The kinds of item a swarm deploy puts in its job. A step is a line of the console's step list, a
// service is a row under Deploy services, and a group only holds steps and is not shown itself.
const (
	KindStep    = "step"
	KindService = "service"
	KindGroup   = "group"
	// KindTask is a swarm task of a service, under the service's row while the deploy waits for it.
	KindTask = "task"
	// KindContainer is a container of a service of a standalone host, under the service's row.
	KindContainer = "container"
)

// DeployItemSpecs are the items a swarm deploy job starts with, in run order, with the words the
// console shows. The networks, the secrets and configs, the services and the old objects the deploy
// removes are added to the job while it runs, once they are known (Reporter.Add): a network or an
// object under its group, a service under Deploy services, an old secret or config under the
// cleanup step.
func DeployItemSpecs() []jobs.ItemSpec {
	return []jobs.ItemSpec{
		{ID: ItemCheck, Label: "Check the stack", Kind: KindStep},
		{ID: ItemLookup, Label: "Look up images", Kind: KindStep},
		{ID: ItemNetworks, Label: "Networks", Kind: KindGroup},
		{ID: ItemObjects, Label: "Secrets and configs", Kind: KindGroup},
		{ID: ItemServices, Label: "Deploy services", Kind: KindStep},
		{ID: ItemCleanup, Label: "Remove services no longer in the stack", Kind: KindStep},
	}
}

// The ID prefixes of the items a deploy adds.
const (
	idNetwork      = "network:"
	idSecret       = "secret:"
	idConfig       = "config:"
	idService      = "service:"
	idTask         = "task:"
	idRemoveSecret = "remove-secret:"
	idRemoveConfig = "remove-config:"
)

// progress reports the items a deploy adds to its job, and the words it writes in the job's output.
// A nil *progress reports nothing, so the steps that take one work unchanged for a caller that has no
// job. Every remote text goes through mask before it is stored: Docker's own words can echo a secret.
type progress struct {
	r    *jobs.Reporter
	mask func(string) string
	// added are the items this deploy added, in the order they were added.
	added []addedItem

	// The rest is touched by the goroutines that wait for services, so mu guards it.
	mu sync.Mutex
	// accepted are the service rows whose write the swarm accepted and that have not ended, by
	// item ID, with what they will read if the deploy ends before the wait for them does.
	accepted map[string]map[string]string
	// tasks are the task items under each service row, by the row's item ID, and shown is what each
	// of them last published, so a poll that changed nothing publishes nothing.
	tasks map[string][]string
	shown map[string]taskLine
	// rows is what each service row last published.
	rows map[string]rowShown
	// ended is the state each added item ended in, by item ID, for a group to be ended by its children.
	ended map[string]string
}

// taskLine is one task of a service as the console shows it.
type taskLine struct {
	id, label, state, node, errText string
}

// rowShown is the counts and phase a service row last published.
type rowShown struct {
	running, want int
	pulling       bool
}

// maxTaskItems caps the task items under one service, so a service that crash-loops for the whole
// timeout cannot fill the job with the tasks it keeps replacing.
const maxTaskItems = 12

type addedItem struct {
	id, parent, name, noun string
}

func newProgress(r *jobs.Reporter, m stacks.Masker) *progress {
	return &progress{
		r: r, mask: m.Mask, accepted: map[string]map[string]string{}, tasks: map[string][]string{},
		shown: map[string]taskLine{}, rows: map[string]rowShown{}, ended: map[string]string{},
	}
}

// line writes one line of the job's output.
func (p *progress) line(text string) {
	if p == nil {
		return
	}
	p.r.Output(p.mask(text))
}

// detail puts the engine's own words on an item.
func (p *progress) detail(id, text string) {
	if p == nil || text == "" {
		return
	}
	p.r.Detail(id, p.mask(text))
}

// add puts a waiting item in the job and remembers it under name, the name the engine knows the
// network, object or service by, which is also what a failed step reports, and noun, the word the
// output uses for it.
func (p *progress) add(spec jobs.ItemSpec, name, noun string) {
	if p == nil {
		return
	}
	p.r.Add(spec)
	p.added = append(p.added, addedItem{id: spec.ID, parent: spec.Parent, name: name, noun: noun})
}

// addNetwork adds the step that creates a network.
func (p *progress) addNetwork(name string) {
	p.add(jobs.ItemSpec{ID: idNetwork + name, Label: "Create network " + name, Parent: ItemNetworks, Kind: KindStep}, name, "network")
}

// addObject adds the step that creates a secret or config. name is the compose name, full the
// hashed name it is created under.
func (p *progress) addObject(kind stacks.ObjectKind, name, full string) {
	id, word := idConfig+full, "config"
	if kind == stacks.KindSecret {
		id, word = idSecret+full, "secret"
	}
	p.add(jobs.ItemSpec{ID: id, Label: "Create " + word + " " + name, Parent: ItemObjects, Kind: KindStep}, full, word)
}

// addService adds the row of a service.
func (p *progress) addService(sp convert.ServicePlan) {
	label := sp.Short
	if label == "" {
		label = sp.Name
	}
	p.add(jobs.ItemSpec{ID: idService + sp.Name, Label: label, Parent: ItemServices, Kind: KindService}, sp.Name, "service")
}

// addRemoval adds the step that removes an old secret or config the deploy replaced.
func (p *progress) addRemoval(kind stacks.ObjectKind, label, full string) {
	id, word := idRemoveConfig+full, "config"
	if kind == stacks.KindSecret {
		id, word = idRemoveSecret+full, "secret"
	}
	p.add(jobs.ItemSpec{ID: id, Label: "Remove old " + word + " " + label, Parent: ItemCleanup, Kind: KindStep}, full, word)
}

// start marks an added item as working.
func (p *progress) start(id string) {
	if p == nil {
		return
	}
	p.r.Update(id, func(it *jobs.Item) { it.State = jobs.ItemRunning })
}

// done ends an added item as done, with a result.
func (p *progress) done(id string, result map[string]string) {
	if p == nil {
		return
	}
	p.r.Finish(id, jobs.ItemDone, "", "", result)
	p.setEnded(id, jobs.ItemDone)
}

// setEnded records the state an item ended in.
func (p *progress) setEnded(id, state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ended[id] = state
}

// groupState is the state a group item ends in once its children have: done when it has none or every one
// ended done, skipped otherwise.
func (p *progress) groupState(parent string) string {
	if p == nil {
		return jobs.ItemDone
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.added {
		if a.parent == parent && p.ended[a.id] != jobs.ItemDone {
			return jobs.ItemSkipped
		}
	}
	return jobs.ItemDone
}

// failed ends the item of the step (networks, objects, services or cleanup) whose name is the one a
// failed step reported, with its code and the engine's words, and writes them to the output. The
// other items are left as they are. A step that names nothing, or names an item this deploy did not
// add (an external object), ends no item, and failed says so.
func (p *progress) failed(step, name, state, code, message, detail string) bool {
	if p == nil || name == "" {
		return false
	}
	for _, a := range p.added {
		if a.parent == step && a.name == name {
			p.detail(a.id, detail)
			if detail != "" {
				p.line("Failed " + a.noun + " " + a.name + ": " + detail)
			}
			p.r.Finish(a.id, state, code, message, nil)
			return true
		}
	}
	return false
}

// rest ends every added item that has not ended: the ones after a failure were not run. A service
// the swarm accepted is not one of them: it was written, so it ends done, and reads Updated or Created
// rather than Running because nothing waited for it. The tasks under a row end with it.
func (p *progress) rest(state, code, message string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.added {
		if result, ok := p.accepted[a.id]; ok {
			result["settled"] = "false"
			p.endTasksLocked(a.id)
			p.r.Finish(a.id, jobs.ItemDone, "", "", result)
			delete(p.accepted, a.id)
			continue
		}
		p.r.Finish(a.id, state, code, message, nil)
		if p.ended[a.id] == "" {
			p.ended[a.id] = state
		}
	}
}

// accept records that the swarm accepted the write of a service: the row stays working, and the
// wait for it reads the rest. action is created or updated.
func (p *progress) accept(name, action string) {
	if p == nil {
		return
	}
	id := idService + name
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accepted[id] = map[string]string{"action": action}
	p.r.Update(id, func(it *jobs.Item) { it.Result = map[string]string{"action": action} })
}

// converging publishes how far a service has got and its tasks, when that changed since the last
// time. want is what the row counts against.
func (p *progress) converging(name string, c Convergence, tasks []taskLine) {
	if p == nil {
		return
	}
	id := idService + name
	want := c.Want
	if c.Mode != modeReplicated {
		want = c.Desired
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := (rowShown{running: c.Running, want: want, pulling: c.Pulling}); p.rows[id] != now {
		p.rows[id] = now
		result := maps0(p.accepted[id])
		if c.Pulling {
			result["phase"] = "pulling"
		}
		p.r.Update(id, func(it *jobs.Item) {
			it.Current, it.Total, it.Unit, it.Result = int64(c.Running), int64(want), "tasks", result
		})
	}
	for _, t := range tasks {
		tid := idTask + t.id
		old, seen := p.shown[tid]
		if !seen {
			if len(p.tasks[id]) >= maxTaskItems {
				continue
			}
			p.r.Add(jobs.ItemSpec{ID: tid, Label: t.label, Parent: id, Kind: KindTask})
			p.tasks[id] = append(p.tasks[id], tid)
		}
		if seen && old == t {
			continue
		}
		p.shown[tid] = t
		detail := jobs.Clean(t.errText, p.mask)
		p.r.Update(tid, func(it *jobs.Item) {
			it.State = jobs.ItemRunning
			it.Result = map[string]string{"state": t.state, "node": t.node}
			it.Detail = detail
		})
	}
}

// settled ends the row of a service that runs what its spec says: done, with the counts it settled at.
func (p *progress) settled(name string, c Convergence) {
	if p == nil {
		return
	}
	id := idService + name
	p.mu.Lock()
	defer p.mu.Unlock()
	result := maps0(p.accepted[id])
	result["settled"] = "true"
	result["running"], result["desired"] = strconv.Itoa(c.Running), strconv.Itoa(max(c.Want, c.Desired))
	delete(p.accepted, id)
	p.endTasksLocked(id)
	p.r.Finish(id, jobs.ItemDone, "", "", result)
	p.line("Service " + name + " is running")
}

// notWaited ends the row of a service the deploy does not wait for (a job, which runs to its end on its
// own): done as created or updated, never as running.
func (p *progress) notWaited(name string) {
	if p == nil {
		return
	}
	id := idService + name
	p.mu.Lock()
	defer p.mu.Unlock()
	result := maps0(p.accepted[id])
	result["settled"] = "false"
	delete(p.accepted, id)
	p.r.Finish(id, jobs.ItemDone, "", "", result)
}

// unsettled ends the row of a service the wait gave up on, failed, with its code, the swarm's own
// words and what the console needs to word it.
func (p *progress) unsettled(name, code, words string, result map[string]string) {
	if p == nil {
		return
	}
	id := idService + name
	p.mu.Lock()
	defer p.mu.Unlock()
	merged := maps0(p.accepted[id])
	for k, v := range result {
		merged[k] = v
	}
	delete(p.accepted, id)
	p.endTasksLocked(id)
	p.detail(id, words)
	if words != "" {
		p.line("Service " + name + ": " + words)
	}
	p.r.Finish(id, jobs.ItemFailed, code, CodeMessage(code), merged)
}

// endTasksLocked ends the task items under a row: they were shown while the wait lasted, and what
// they said last stays in them.
func (p *progress) endTasksLocked(rowID string) {
	for _, tid := range p.tasks[rowID] {
		p.r.Finish(tid, jobs.ItemDone, "", "", nil)
	}
	delete(p.tasks, rowID)
}

// maps0 is a copy of m that can be written to, empty for a nil m.
func maps0(m map[string]string) map[string]string {
	out := maps.Clone(m)
	if out == nil {
		out = map[string]string{}
	}
	return out
}
