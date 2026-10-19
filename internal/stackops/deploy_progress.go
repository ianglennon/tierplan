// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
)

// The kinds of item a swarm deploy puts in its job. A step is a line of the console's step list, a
// service is a row under Deploy services, and a group only holds steps and is not shown itself.
const (
	KindStep    = "step"
	KindService = "service"
	KindGroup   = "group"
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
}

type addedItem struct {
	id, parent, name, noun string
}

func newProgress(r *jobs.Reporter, m stacks.Masker) *progress {
	return &progress{r: r, mask: m.Mask}
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

// rest ends every added item that has not ended: the ones after a failure were not run.
func (p *progress) rest(state, code, message string) {
	if p == nil {
		return
	}
	for _, a := range p.added {
		p.r.Finish(a.id, state, code, message, nil)
	}
}
