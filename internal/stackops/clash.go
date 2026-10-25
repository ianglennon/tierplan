// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"errors"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// noStack is the stack ID of a stack that does not exist yet. No label holds it, so everything
// that carries the stack's name is something else's.
const noStack = "\x00"

// NameClashMessage is the sentence for a name that something Bayplan does not manage holds.
func NameClashMessage(name string) string {
	return "A stack named " + name + " already exists on this environment, not managed by Bayplan."
}

// ForeignStack reports whether the environment already holds a stack of that name that Bayplan
// does not manage (D-12): on a swarm a service, network, secret or config under the namespace
// label, on a standalone host a container, network or volume under the Compose project label,
// that does not carry stackID in bayplan.stack.id. Bayplan never deploys into, takes over or
// changes such a stack. stackID is empty for a stack being created, for which anything of that
// name is foreign; the objects of an existing Bayplan stack of the same name never count. The
// labels cross to the engine through the client's filter type, never joined into a query.
// It is a read.
func (d Deps) ForeignStack(ctx context.Context, envID string, target stacks.Target, name, stackID string) (bool, error) {
	if stackID == "" {
		stackID = noStack
	}
	return d.foreignUnder(ctx, envID, Ownership{StackID: stackID, Name: name, Target: target})
}

// ForeignStackOf is ForeignStack for a stack Bayplan holds, judged by the stack's own ownership rule (D-16): an
// adopted stack that has not been deployed yet owns the workloads that carry no ID of its own, and the networks,
// secrets and configs it recorded when it was adopted, so its first deploy is no name clash. A swarm only; a
// standalone host still compares the stack's ID (plan 08-17).
func (d Deps) ForeignStackOf(ctx context.Context, envID string, st Stack) (bool, error) {
	return d.foreignUnder(ctx, envID, OwnershipOf(st))
}

// foreignUnder reads the environment for objects of the stack's name that own does not own.
func (d Deps) foreignUnder(ctx context.Context, envID string, own Ownership) (bool, error) {
	if own.Target == stacks.TargetStandalone {
		return d.foreignProject(ctx, envID, own.Name, own.StackID)
	}
	live, err := d.readLive(ctx, envID, own.Name, liveWant{})
	if err != nil {
		return false, err
	}
	own.Target = stacks.TargetSwarm
	return clash(own, live.Networks, live.Services, live.Objects), nil
}

// foreignProject reads the one engine of a standalone environment for a Compose project of this
// name.
func (d Deps) foreignProject(ctx context.Context, envID, name, stackID string) (bool, error) {
	if d.Aggregator == nil {
		return false, errors.New("stackops: no aggregator to read the host with")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return false, err
	}
	if len(agg.Nodes) == 0 {
		return false, envs.ErrNodeUnavailable
	}
	foreign := false
	err = d.Aggregator.OnNode(ctx, envID, agg.Nodes[0].ID, func(c *dockerx.Client) error {
		containers, e := c.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
		if e != nil {
			return e
		}
		networks, e := c.NetworksWithLabel(ctx, stacks.LabelComposeProject, name)
		if e != nil {
			return e
		}
		volumes, e := c.VolumesWithLabel(ctx, stacks.LabelComposeProject, name)
		if e != nil {
			return e
		}
		for _, r := range containers {
			foreign = foreign || r.Labels[stacks.LabelStackID] != stackID
		}
		for _, r := range networks {
			foreign = foreign || r.Labels[stacks.LabelStackID] != stackID
		}
		for _, r := range volumes {
			foreign = foreign || r.Labels[stacks.LabelStackID] != stackID
		}
		return nil
	})
	return foreign, err
}

// CheckTarget says whether the stack can be deployed to the environment as it is now. A stack is
// made for the kind of environment it was created on (swarm or standalone host), and the kind
// can change under it: it is ErrTargetChanged when it has. A failure to tell what the environment
// is, is that error.
func (d Deps) CheckTarget(ctx context.Context, envID string, stack Stack) error {
	if d.Clusters == nil {
		return errors.New("stackops: no cluster reader to tell what the environment is")
	}
	isSwarm, err := d.Clusters.IsSwarm(ctx, envID)
	if err != nil {
		return err
	}
	if isSwarm != (stack.Target == stacks.TargetSwarm) {
		return ErrTargetChanged
	}
	return nil
}
