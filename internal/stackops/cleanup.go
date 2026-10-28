// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stacks"
)

const (
	// writeTimeout bounds one write of a deploy. A write that has begun is not cut short when the job
	// is cancelled, so it carries its own limit.
	writeTimeout = 60 * time.Second
	// cleanupTimeout bounds the whole cleanup of a deploy that stopped.
	cleanupTimeout = 2 * time.Minute
)

// CodeRemoveFailed ends the cleanup step of an object the deploy created and could not remove.
const CodeRemoveFailed = "remove_failed"

func init() {
	codeMessages[CodeRemoveFailed] = "Bayplan couldn't remove this. It is still there."
}

// write runs one write on a manager, once, on a context the job's cancel cannot reach: the deploy
// asks whether it was cancelled between writes, never in the middle of one, so a service is not left
// half written and a write whose answer a cancel cut off is never repeated (P2 D-42). The write has
// a limit of its own instead.
func (d Deps) write(ctx context.Context, envID string, fn func(ctx context.Context, c *dockerx.Client) error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	return d.Aggregator.WriteOnManager(wctx, envID, func(c *dockerx.Client) error { return fn(wctx, c) })
}

// cleanupCreated removes the networks, secrets and configs this deploy created that no service uses,
// after a failure or a cancel left them behind (D-14, leave nothing behind). It reads the stack's
// services, networks, secrets and configs again first, because the deploy's own picture is from
// before it wrote anything, and removes a created object only when no service spec names it. A
// created object a service uses stays (it is the deploy's, and the service that was updated needs
// it); one that could not be removed stays and is named. Nothing the deploy did not create is
// touched, and no service is changed.
//
// It runs on a context the cancel cannot reach, adds a step under the cleanup step for each object
// ("Remove network web_default", labelled Cleaned up) and records what it removed in res.CleanedUp
// and what stays in res.LeftInPlace. If the stack cannot be read, nothing is judged unused: every
// created object stays and is named.
func (d Deps) cleanupCreated(ctx context.Context, envID string, stack Stack, res *DeployResult, prog *progress) {
	if len(res.CreatedNetworks) == 0 && len(res.CreatedObjects) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	var (
		svcs     []dockerx.ServiceDetail
		networks []dockerx.NetworkRow
		secrets  []objectRow
		configs  []objectRow
	)
	own := OwnershipOf(stack)
	readErr := d.Clusters.ClusterRead(cctx, envID, func(c *dockerx.Client) error {
		var e error
		networks, secrets, configs = nil, nil, nil // a read that is tried on another manager starts again
		if svcs, e = c.ServicesWithLabel(cctx, stacks.LabelSwarmNamespace, stack.Name); e != nil {
			return e
		}
		nets, e := c.NetworksWithLabel(cctx, own.labelKey(), own.Name)
		if e != nil {
			return e
		}
		for _, n := range nets {
			if own.OwnsNetwork(n.Name, n.Labels) {
				networks = append(networks, n)
			}
		}
		srows, e := c.SecretsWithLabel(cctx, own.labelKey(), own.Name)
		if e != nil {
			return e
		}
		crows, e := c.ConfigsWithLabel(cctx, own.labelKey(), own.Name)
		if e != nil {
			return e
		}
		for _, r := range secretRows(srows) {
			if own.OwnsObject(r.Labels) {
				secrets = append(secrets, r)
			}
		}
		for _, r := range configRows(crows) {
			if own.OwnsObject(r.Labels) {
				configs = append(configs, r)
			}
		}
		return nil
	})
	if readErr != nil {
		d.log().Warn("could not read a stack again to clean up after a deploy that stopped", "environment", envID, "stack", stack.Name, "error", readErr)
	}
	usedNets := networkTargets(svcs)
	usedObjs := referencedObjects(svcs)

	for _, name := range res.CreatedNetworks {
		item := prog.addCleanup("network", name)
		if readErr != nil {
			d.keepCleanup(res, prog, item, "network", name, "failed", readErr)
			continue
		}
		row, ok := findNetwork(networks, name)
		switch {
		case !ok:
			// Already gone: nothing is left behind.
			prog.cleaned(item, "network", name, res)
		case usedNets[row.ID] || usedNets[row.Name]:
			d.keepCleanup(res, prog, item, "network", name, "in_use", nil)
		default:
			prog.startCleanup(item)
			err := d.write(cctx, envID, func(wctx context.Context, c *dockerx.Client) error { return c.NetworkRemove(wctx, row.ID) })
			d.afterRemoval(res, prog, item, "network", name, err, dockerx.IsNetworkInUse(err), dockerx.IsNotFound(err))
		}
	}
	for _, name := range res.CreatedObjects {
		kind := prog.objectNoun(name)
		item := prog.addCleanup(kind, name)
		if readErr != nil {
			d.keepCleanup(res, prog, item, kind, name, "failed", readErr)
			continue
		}
		rows := secrets
		if kind == "config" {
			rows = configs
		}
		idx := slices.IndexFunc(rows, func(r objectRow) bool { return r.Name == name })
		switch {
		case idx < 0:
			prog.cleaned(item, kind, name, res)
		case usedObjs.has(rows[idx]):
			d.keepCleanup(res, prog, item, kind, name, "in_use", nil)
		default:
			row := rows[idx]
			prog.startCleanup(item)
			err := d.write(cctx, envID, func(wctx context.Context, c *dockerx.Client) error {
				if row.Kind == stacks.KindSecret {
					return c.SecretRemove(wctx, row.ID)
				}
				return c.ConfigRemove(wctx, row.ID)
			})
			d.afterRemoval(res, prog, item, kind, name, err, errors.Is(err, dockerx.ErrObjectInUse), errors.Is(err, dockerx.ErrObjectNotFound))
		}
	}
}

// afterRemoval records how one removal ended: removed (or already gone), still in use, or failed.
func (d Deps) afterRemoval(res *DeployResult, prog *progress, item, kind, name string, err error, inUse, gone bool) {
	switch {
	case err == nil || gone:
		prog.cleaned(item, kind, name, res)
	case inUse:
		d.keepCleanup(res, prog, item, kind, name, "in_use", nil)
	default:
		d.log().Warn("could not remove an object a deploy that stopped had created", "kind", kind, "object", name, "error", err)
		d.keepCleanup(res, prog, item, kind, name, "failed", err)
	}
}

// keepCleanup records an object the cleanup leaves: why is in_use (a service uses it) or failed
// (it could not be removed, or could not be judged). Both are named in res.LeftInPlace.
func (d Deps) keepCleanup(res *DeployResult, prog *progress, item, kind, name, why string, err error) {
	res.LeftInPlace = append(res.LeftInPlace, name)
	prog.kept(item, kind, name, why, engineWords(err))
}

// networkTargets are the networks the specs of some services attach to, by the ID or name each spec
// uses.
func networkTargets(svcs []dockerx.ServiceDetail) map[string]bool {
	out := map[string]bool{}
	for _, s := range svcs {
		var spec swarm.ServiceSpec
		if json.Unmarshal(s.Spec, &spec) != nil {
			continue
		}
		for _, n := range spec.TaskTemplate.Networks {
			out[n.Target] = true
		}
	}
	return out
}

// The ids and results of the steps the cleanup adds.
const (
	idCleanNetwork = "clean-network:"
	idCleanSecret  = "clean-secret:"
	idCleanConfig  = "clean-config:"
	// idCleanContainer is a container a standalone deploy that stopped had created and removes again.
	idCleanContainer = "clean-container:"
)

// addCleanup adds the step that removes an object this deploy created, under the cleanup step.
func (p *progress) addCleanup(kind, name string) string {
	if p == nil {
		return ""
	}
	id := idCleanConfig + name
	switch kind {
	case "network":
		id = idCleanNetwork + name
	case "secret":
		id = idCleanSecret + name
	case "container":
		id = idCleanContainer + name
	}
	p.r.Add(jobs.ItemSpec{ID: id, Label: "Remove " + kind + " " + name, Parent: ItemCleanup, Kind: KindStep})
	return id
}

func (p *progress) startCleanup(id string) {
	if p != nil {
		p.start(id)
	}
}

// cleaned ends a cleanup step as removed.
func (p *progress) cleaned(id, kind, name string, res *DeployResult) {
	res.CleanedUp = append(res.CleanedUp, name)
	if p == nil {
		return
	}
	p.line("Removed " + kind + " " + name)
	p.r.Finish(id, jobs.ItemDone, "", "", map[string]string{"cleaned": "true", "kind": kind, "name": name})
}

// kept ends a cleanup step for an object that stays. A service using it is not a failure, so the step
// is done; one that could not be removed fails, with the engine's words.
func (p *progress) kept(id, kind, name, why, words string) {
	if p == nil {
		return
	}
	result := map[string]string{"cleaned": "false", "left": why, "kind": kind, "name": name}
	if why == "in_use" {
		p.line("Kept " + kind + " " + name + ": a service uses it")
		p.r.Finish(id, jobs.ItemDone, "", "", result)
		return
	}
	p.detail(id, words)
	p.line("Could not remove " + kind + " " + name)
	p.r.Finish(id, jobs.ItemFailed, CodeRemoveFailed, CodeMessage(CodeRemoveFailed), result)
}

// objectNoun is the word (secret or config) the deploy used for an object it created.
func (p *progress) objectNoun(name string) string {
	if p == nil {
		return "secret"
	}
	for _, a := range p.added {
		if a.parent == ItemObjects && a.name == name {
			return a.noun
		}
	}
	return "secret"
}

// cleanupStandalone removes what a standalone deploy that stopped (a failure or a cancel in the middle of
// the SDK's up) created and left behind, and nothing else (D-14, D-20, leave nothing behind). It reads the
// stack's project again, because the deploy's picture is from before the SDK wrote anything, and removes:
//
//   - a container of the stack that was not there before the up and never started (state created): the
//     container of a service that did not get as far as its start, and the SDK's temporary container of a
//     recreate, which it names <12 hex>_<name> until the old one is gone;
//   - a network the deploy meant to create (createdNetworks) that now exists, carries the stack's ID and
//     nothing is attached to: the engine itself refuses a network that has an endpoint, and that refusal is
//     the network left in place, named as such.
//
// Containers that run, or ran and ended, the containers that were there before, every volume (a volume
// holds data, and anonymous ones of a container that never started are left with it), and anything of
// another project stay. It runs on a context the cancel cannot reach, adds a step under the cleanup step for
// each thing it removes or keeps ("Remove container web-1", labelled Cleaned up), and records them in
// res.CleanedUp and res.LeftInPlace. A host that cannot be read again is left as it is and the output says
// so. Nothing is retried.
func (d Deps) cleanupStandalone(ctx context.Context, envID, nodeID string, stack Stack, before []dockerx.ComposeContainer, createdNetworks []string, res *DeployResult, prog *progress) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	after, err := d.readStandalone(cctx, envID, nodeID, stack.Name)
	if err != nil {
		d.log().Warn("could not read a standalone host again to clean up after a deploy that stopped", "environment", envID, "stack", stack.Name, "error", err)
		prog.line("Could not read the host again, so nothing the deploy created was cleaned up")
		return
	}
	had := make(map[string]bool, len(before))
	for _, c := range before {
		had[c.ID] = true
	}
	own := OwnershipOf(stack)
	var stray []dockerx.ComposeContainer
	for _, c := range after.Containers {
		// Only a container this deploy created (its ID was not on the host before) and that the stack owns: one that
		// existed at adoption, or that belongs to nobody the stack knows, is never removed (T-08-66).
		if !had[c.ID] && c.State == "created" && own.OwnsWorkload(c.Labels) {
			stray = append(stray, c)
		}
	}
	sort.Slice(stray, func(i, j int) bool { return stray[i].Name < stray[j].Name })
	write := func(fn func(wctx context.Context, c *dockerx.Client) error) error {
		wctx, wcancel := context.WithTimeout(cctx, writeTimeout)
		defer wcancel()
		return d.Aggregator.WriteOnNode(wctx, envID, nodeID, func(c *dockerx.Client) error { return fn(wctx, c) })
	}
	for _, c := range stray {
		item := prog.addCleanup("container", c.Name)
		prog.startCleanup(item)
		// No force, and the container's anonymous volumes stay: it never ran, and a volume is never removed.
		err := write(func(wctx context.Context, cl *dockerx.Client) error {
			return cl.ContainerRemove(wctx, c.ID, dockerx.RemoveOptions{})
		})
		d.afterRemoval(res, prog, item, "container", c.Name, err, false, dockerx.IsNotFound(err))
	}
	names := slices.Clone(createdNetworks)
	sort.Strings(names)
	for _, name := range names {
		row, ok := findNetwork(after.Networks, name)
		if !ok || !own.OwnsNetwork(row.Name, row.Labels) {
			continue // the up never got as far as this network, or it is not the stack's
		}
		item := prog.addCleanup("network", name)
		prog.startCleanup(item)
		err := write(func(wctx context.Context, cl *dockerx.Client) error { return cl.NetworkRemove(wctx, row.ID) })
		d.afterRemoval(res, prog, item, "network", name, err, dockerx.IsNetworkInUse(err), dockerx.IsNotFound(err))
	}
}
