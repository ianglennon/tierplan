// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// errNoRevision means a stack has no revision that ran well, so there is no project to stop or start.
var errNoRevision = errors.New("stackops: the stack has no deployed version")

// stopProject runs the Compose SDK's stop on c, or the test's stand-in for it.
func (d Deps) stopProject(ctx context.Context, c *dockerx.Client, p *types.Project) error {
	if d.StopProject != nil {
		return d.StopProject(ctx, c, p)
	}
	return c.ComposeStop(ctx, p)
}

// startProject runs the Compose SDK's start on c, or the test's stand-in for it.
func (d Deps) startProject(ctx context.Context, c *dockerx.Client, p *types.Project) error {
	if d.StartProject != nil {
		return d.StartProject(ctx, c, p)
	}
	return c.ComposeStart(ctx, p)
}

// standaloneProject is the project of the stack's last good revision, taken from the check pipeline
// (stacks.Deployable: env_file resolved, build stripped, the same project the review and the standalone
// deploy judge) and made ready the way a deploy makes it (stacks.PrepareStandalone): the services with
// the images the revision ran, and the content of the stack file's secrets and configs, which the SDK
// writes into a container again before it starts. The Compose SDK needs it to stop the containers in
// depends_on order and to inject the secrets and configs at a start; it creates nothing from it, so no
// image is looked up and none is pulled.
func (d Deps) standaloneProject(ctx context.Context, st Stack, store LifecycleStore) (*types.Project, stacks.Masker, error) {
	none := stacks.NewMasker(nil, nil)
	if store == nil {
		return nil, none, errors.New("stackops: no store to read the last deployed version from")
	}
	rev, ok, err := store.LatestGoodRevision(ctx, st.ID)
	if err != nil {
		return nil, none, err
	}
	if !ok {
		return nil, none, errNoRevision
	}
	masker := stacks.NewMasker(rev.Copy.Vars, rev.Copy.Files)
	pins, err := store.RevisionPins(ctx, rev.ID)
	if err != nil {
		return nil, masker, err
	}
	loaded, err := stacks.Deployable(ctx, stacks.CheckInput{Name: st.Name, Target: stacks.TargetStandalone, Copy: rev.Copy})
	if err != nil {
		return nil, masker, err
	}
	project := loaded.Project
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, project, stacks.NewFileSet(rev.Copy.Files), stacks.ObjectVars(rev.Copy))
	if err != nil {
		return nil, masker, err
	}
	refs := map[string]string{}
	for _, it := range standaloneItems(project) {
		refs[it.Name] = it.AsWritten
		if ref := pins[it.Name]; ref != "" {
			refs[it.Name] = ref
		}
	}
	if err := stacks.PrepareStandalone(project, st.ID, refs, objs); err != nil {
		return nil, masker, err
	}
	return project, masker, nil
}

// standaloneLifecycle checks the host and the stack for a stop or a start, and runs the SDK's call on the
// project. It is the whole of a standalone operation: the Compose SDK does the work and the result is the
// error it returned.
func (l *lifecycle) standaloneLifecycle(ctx context.Context, mode string) {
	d, in := l.d, l.in
	l.r.Update(ItemCheck, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	// A host its owner made read-only is the first thing to learn: nothing is asked of it.
	if err := d.writable(ctx, in.EnvID, in.NodeID); err != nil {
		if errors.Is(err, envs.ErrHostReadOnly) {
			l.fail(ItemCheck, CodeHostReadOnly, "")
			return
		}
		d.log().Warn("a standalone host could not be written to before a "+mode, "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemCheck, codeOf(ctx, err), "", err)
		return
	}
	live, err := d.readStandalone(ctx, in.EnvID, in.NodeID, in.Stack.Name)
	if err != nil {
		d.log().Warn("could not read a standalone host before a "+mode, "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemCheck, codeOf(ctx, err), "", err)
		return
	}
	// The SDK acts on every container of the Compose project: one that Bayplan does not manage is never
	// stopped or started by it.
	if foreignLive(live, OwnershipOf(in.Stack)) {
		l.fail(ItemCheck, CodeNameClash, "")
		return
	}
	project, masker, err := d.standaloneProject(ctx, in.Stack, in.Store)
	// The SDK's own words can echo a value the project carries (a stack file's secret rides in its environment).
	l.prog.mask = masker.Mask
	if err != nil {
		code := CodeStackInvalid
		if errors.Is(err, errNoRevision) {
			code = CodeNoRevision
		}
		d.log().Warn("a standalone stack could not be prepared for a "+mode, "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.fail(ItemCheck, code, "")
		return
	}
	for _, name := range project.ServiceNames() {
		l.prog.addStandaloneService(name)
	}
	l.r.Finish(ItemCheck, jobs.ItemDone, "", "", nil)

	l.r.Update(ItemServices, func(it *jobs.Item) {
		it.State, it.Unit, it.Total = jobs.ItemRunning, "services", int64(len(project.Services))
	})
	for _, name := range project.ServiceNames() {
		l.prog.start(idService + name)
	}
	if ctx.Err() != nil {
		l.fail(ItemServices, CodeCancelled, "")
		return
	}
	// The write goes through the node, so a read-only host is refused before the SDK is built; it runs on the
	// job's context, so a cancel stops the SDK where it is. Nothing is retried.
	err = d.Aggregator.WriteOnNode(ctx, in.EnvID, in.NodeID, func(c *dockerx.Client) error {
		if mode == LifecycleStop {
			return d.stopProject(ctx, c, project)
		}
		return d.startProject(ctx, c, project)
	})
	if err != nil {
		d.log().Warn("the Compose SDK's "+mode+" failed", "environment", in.EnvID, "stack", in.Stack.Name, "error", err)
		l.failErr(ItemServices, codeOf(ctx, err), "", err)
		return
	}
	for _, name := range project.ServiceNames() {
		word := "stopped"
		if mode == LifecycleStart {
			word = "started"
		}
		l.prog.line(word + " service " + name)
		l.prog.done(idService+name, map[string]string{"action": word})
	}
	l.res.Containers = len(project.Services)
	l.markStopped(ctx, mode == LifecycleStop)
}

func (l *lifecycle) stopStandalone(ctx context.Context)  { l.standaloneLifecycle(ctx, LifecycleStop) }
func (l *lifecycle) startStandalone(ctx context.Context) { l.standaloneLifecycle(ctx, LifecycleStart) }
