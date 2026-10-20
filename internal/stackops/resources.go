// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The ownership of an object on the Resources tab. An object is the stack's only when it carries the
// stack's own labels; one the compose file refers to that was made outside the stack is external, and
// Bayplan never changes or removes it (STK-10, D-22).
const (
	OwnerStack    = "stack"
	OwnerExternal = "external"
)

// The codes a card's Error holds when its read failed. The page words them; the engine's own text is
// logged on the server and never sent.
const (
	// ReadNoManager means no manager of the swarm is connected.
	ReadNoManager = "no_manager"
	// ReadNoQuorum means the swarm has lost its quorum.
	ReadNoQuorum = "no_quorum"
	// ReadNodeUnavailable means the standalone host is not connected.
	ReadNodeUnavailable = "node_unavailable"
	// ReadFailed is any other read that failed.
	ReadFailed = "read_failed"
)

// ResourceCard is one card of the Resources tab: its rows, or the code of the read that failed. Each
// card is read on its own, so one that fails never takes the others with it.
type ResourceCard[T any] struct {
	Items []T
	Error string
}

// TaskResource is the current task of one slot of a swarm stack's service (or of one node, for a global
// service), with the node it is on.
type TaskResource struct {
	Task dockerx.TaskRow
	// ServiceID is the swarm service ID, Service its name in the compose file (without the stack prefix).
	ServiceID, Service string
	// Global is true for a global service, whose rows are nodes rather than slots.
	Global bool
	// Node is the swarm's row for the task's node, NodeListed whether the swarm lists it (a removed node's
	// ID is kept on its tasks).
	Node       dockerx.NodeRow
	NodeListed bool
}

// ContainerResource is one container of a standalone stack.
type ContainerResource struct {
	ID, Name, Service, State, Health, Image string
	// Ports are the published ports as published->target/protocol, without the host address.
	Ports   []string
	Created time.Time
}

// NetworkResource is one network the stack owns or uses.
type NetworkResource struct {
	ID, Name, Driver, Scope string
	// Ownership is OwnerStack or OwnerExternal.
	Ownership string
}

// Resources is everything a stack owns on its environment, in cards: the swarm's tasks or the
// standalone host's containers, and the networks. A card is left empty (not nil) when nothing is there.
type Resources struct {
	Tasks      ResourceCard[TaskResource]
	Containers ResourceCard[ContainerResource]
	Networks   ResourceCard[NetworkResource]
}

// readCode names the failure of a read for a card.
func readCode(err error) string {
	switch {
	case errors.Is(err, envs.ErrNoManager):
		return ReadNoManager
	case envs.ReasonFor(err) == envs.ReasonNoQuorum:
		return ReadNoQuorum
	case errors.Is(err, envs.ErrNodeUnavailable):
		return ReadNodeUnavailable
	}
	return ReadFailed
}

// cardOf turns the result of one read into a card. The engine's text is logged and the card carries a code.
func cardOf[T any](d Deps, envID string, stack Stack, card string, items []T, err error) ResourceCard[T] {
	if err != nil {
		d.log().Warn("could not read a card of a stack's resources", "environment", envID, "stack", stack.Name, "card", card, "error", err)
		return ResourceCard[T]{Items: []T{}, Error: readCode(err)}
	}
	if items == nil {
		items = []T{}
	}
	return ResourceCard[T]{Items: items}
}

// ownsLabels reports whether an object's labels are the stack's own: its swarm namespace or Compose
// project and the ID of its Bayplan record. A name alone is never ownership: another stack, or a person,
// can make an object under the same namespace.
func ownsLabels(stack Stack, labels map[string]string) bool {
	if labels[stacks.LabelStackID] != stack.ID {
		return false
	}
	if stack.Target == stacks.TargetSwarm {
		return labels[stacks.LabelSwarmNamespace] == stack.Name
	}
	return labels[stacks.LabelComposeProject] == stack.Name
}

// Resources reads the cards of a stack's Resources tab now. Each card is its own read and keeps its own
// error, so a secrets list the engine refuses leaves the tasks and the networks in place. It is a read.
func (d Deps) Resources(ctx context.Context, envID string, stack Stack) Resources {
	if stack.Target == stacks.TargetSwarm {
		tasks, err := d.swarmTasks(ctx, envID, stack)
		nets, nerr := d.networksOf(ctx, envID, stack)
		return Resources{
			Tasks:      cardOf(d, envID, stack, "tasks", tasks, err),
			Containers: ResourceCard[ContainerResource]{Items: []ContainerResource{}},
			Networks:   cardOf(d, envID, stack, "networks", nets, nerr),
		}
	}
	containers, err := d.standaloneContainers(ctx, envID, stack)
	nets, nerr := d.networksOf(ctx, envID, stack)
	return Resources{
		Tasks:      ResourceCard[TaskResource]{Items: []TaskResource{}},
		Containers: cardOf(d, envID, stack, "containers", containers, err),
		Networks:   cardOf(d, envID, stack, "networks", nets, nerr),
	}
}

// currentDesired are the desired states of a task that still belongs on the page: one that is wanted
// running, one the swarm holds ready for its restart delay, and one a job completed.
func currentDesired(state string) bool {
	switch state {
	case "running", "ready", "completed":
		return true
	}
	return false
}

// currentTasks picks the current task of each slot (a replicated service) or each node (a global one) from
// tasks that are newest first: the newest the swarm still wants, so the page shows what runs and not the
// history Docker keeps (the service's Tasks tab has that). A slot with no task the swarm wants, as after
// a scale down, has no row.
func currentTasks(rows []dockerx.TaskRow, services map[string]ServiceRow, nodes map[string]dockerx.NodeRow) []TaskResource {
	seen := map[string]bool{}
	out := make([]TaskResource, 0)
	for _, r := range rows {
		svc, ok := services[r.ServiceID]
		if !ok || !currentDesired(r.DesiredState) {
			continue
		}
		global := svc.Mode == "global" || svc.Mode == "global-job"
		key := r.ServiceID + "|slot|" + strconv.Itoa(r.Slot)
		if global {
			key = r.ServiceID + "|node|" + r.NodeID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		n, listed := nodes[r.NodeID]
		out = append(out, TaskResource{Task: r, ServiceID: svc.ID, Service: svc.Short, Global: global, Node: n, NodeListed: listed})
	}
	slices.SortStableFunc(out, func(a, b TaskResource) int {
		return cmp.Or(
			cmp.Compare(a.Service, b.Service),
			cmp.Compare(a.Task.Slot, b.Task.Slot),
			cmp.Compare(a.Node.Hostname, b.Node.Hostname),
			cmp.Compare(a.Task.NodeID, b.Task.NodeID),
			cmp.Compare(a.Task.ID, b.Task.ID),
		)
	})
	return out
}

// swarmTasks reads the current tasks of the stack's services, in one cluster read.
func (d Deps) swarmTasks(ctx context.Context, envID string, stack Stack) ([]TaskResource, error) {
	if d.Clusters == nil {
		return nil, errors.New("stackops: no cluster reader to read the swarm with")
	}
	var out []TaskResource
	err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		read, e := c.Cluster(ctx)
		if e != nil {
			return e
		}
		svcs := SwarmRows(stack, read.Services)
		out = []TaskResource{}
		if len(svcs) == 0 {
			return nil
		}
		byID := make(map[string]ServiceRow, len(svcs))
		ids := make([]string, 0, len(svcs))
		for _, s := range svcs {
			byID[s.ID] = s
			ids = append(ids, s.ID)
		}
		tasks, e := c.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: ids})
		if e != nil {
			return e
		}
		nodes := make(map[string]dockerx.NodeRow, len(read.Nodes))
		for _, n := range read.Nodes {
			nodes[n.ID] = n
		}
		out = currentTasks(tasks, byID, nodes)
		return nil
	})
	return out, err
}

// networksOf lists the networks the stack owns, found by its labels, in one read of the engine that
// holds them: a manager for a swarm (a stack's networks are overlay networks of the cluster), the host's
// own engine for a standalone stack.
func (d Deps) networksOf(ctx context.Context, envID string, stack Stack) ([]NetworkResource, error) {
	var (
		key  string
		read func(fn func(*dockerx.Client) error) error
	)
	if stack.Target == stacks.TargetSwarm {
		if d.Clusters == nil {
			return nil, errors.New("stackops: no cluster reader to read the swarm with")
		}
		key = stacks.LabelSwarmNamespace
		read = func(fn func(*dockerx.Client) error) error { return d.Clusters.ClusterRead(ctx, envID, fn) }
	} else {
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return nil, err
		}
		key = stacks.LabelComposeProject
		read = func(fn func(*dockerx.Client) error) error { return d.Aggregator.OnNode(ctx, envID, node, fn) }
	}
	var out []NetworkResource
	err := read(func(c *dockerx.Client) error {
		rows, e := c.NetworksWithLabel(ctx, key, stack.Name)
		if e != nil {
			return e
		}
		out = []NetworkResource{}
		for _, r := range rows {
			if !ownsLabels(stack, r.Labels) {
				continue
			}
			out = append(out, NetworkResource{ID: r.ID, Name: r.Name, Driver: r.Driver, Scope: r.Scope, Ownership: OwnerStack})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b NetworkResource) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

// standaloneContainers lists the containers of the stack's Compose project, stopped ones too, one-off
// containers (`compose run`) left out.
func (d Deps) standaloneContainers(ctx context.Context, envID string, stack Stack) ([]ContainerResource, error) {
	node, err := d.StandaloneNode(ctx, envID)
	if err != nil {
		return nil, err
	}
	var rows []dockerx.ContainerRow
	if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
		var e error
		rows, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, stack.Name)
		return e
	}); err != nil {
		return nil, err
	}
	out := make([]ContainerResource, 0, len(rows))
	for _, r := range rows {
		if !ownsLabels(stack, r.Labels) || strings.EqualFold(r.Labels[stacks.LabelComposeOneoff], "true") {
			continue
		}
		seen := map[string]bool{}
		ports := []string{}
		for _, p := range r.Ports {
			if h := hostPort(p); h != "" && !seen[h] {
				seen[h] = true
				ports = append(ports, h)
			}
		}
		slices.Sort(ports)
		out = append(out, ContainerResource{
			ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Service: r.Labels[stacks.LabelComposeService],
			State: r.State, Health: r.Health, Image: r.Image, Ports: ports, Created: r.Created,
		})
	}
	slices.SortFunc(out, func(a, b ContainerResource) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}
