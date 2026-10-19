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

// progress reports the items a deploy adds to its job. A nil *progress reports nothing, so the
// steps that take one work unchanged for a caller that has no job.
type progress struct {
	r *jobs.Reporter
	// added are the items this deploy added that have not ended, in the order they were added.
	added []addedItem
}

type addedItem struct {
	id, parent, name string
}

func newProgress(r *jobs.Reporter) *progress { return &progress{r: r} }

// add puts a waiting item in the job and remembers it under name, the name the engine knows the
// network, object or service by, which is also what a failed step reports.
func (p *progress) add(spec jobs.ItemSpec, name string) {
	if p == nil {
		return
	}
	p.r.Add(spec)
	p.added = append(p.added, addedItem{id: spec.ID, parent: spec.Parent, name: name})
}

// addNetwork adds the step that creates a network.
func (p *progress) addNetwork(name string) {
	p.add(jobs.ItemSpec{ID: idNetwork + name, Label: "Create network " + name, Parent: ItemNetworks, Kind: KindStep}, name)
}

// addObject adds the step that creates a secret or config. name is the compose name, full the
// hashed name it is created under.
func (p *progress) addObject(kind stacks.ObjectKind, name, full string) {
	id, word := idConfig+full, "config"
	if kind == stacks.KindSecret {
		id, word = idSecret+full, "secret"
	}
	p.add(jobs.ItemSpec{ID: id, Label: "Create " + word + " " + name, Parent: ItemObjects, Kind: KindStep}, full)
}

// addService adds the row of a service.
func (p *progress) addService(sp convert.ServicePlan) {
	label := sp.Short
	if label == "" {
		label = sp.Name
	}
	p.add(jobs.ItemSpec{ID: idService + sp.Name, Label: label, Parent: ItemServices, Kind: KindService}, sp.Name)
}

// addRemoval adds the step that removes an old secret or config the deploy replaced.
func (p *progress) addRemoval(kind stacks.ObjectKind, label, full string) {
	id, word := idRemoveConfig+full, "config"
	if kind == stacks.KindSecret {
		id, word = idRemoveSecret+full, "secret"
	}
	p.add(jobs.ItemSpec{ID: id, Label: "Remove old " + word + " " + label, Parent: ItemCleanup, Kind: KindStep}, full)
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
// failed step reported, with its code, and leaves the others as they are. A step that names
// nothing, or names an item this deploy did not add (an external object), ends no item.
func (p *progress) failed(step, name, state, code, message string) {
	if p == nil || name == "" {
		return
	}
	for _, a := range p.added {
		if a.parent == step && a.name == name {
			p.r.Finish(a.id, state, code, message, nil)
			return
		}
	}
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
