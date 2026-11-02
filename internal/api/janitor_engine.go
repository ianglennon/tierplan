// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// inspectNodeTimeout is how long the inspects of one node may take before the node's containers
// are left un-inspected, which the planner reads as cant_tell.
const inspectNodeTimeout = time.Minute

// nodeTimeTimeout is how long reading one node's clock may take. A node that does not answer in
// time has no clock in the snapshot, which the planner reads as unknown (RESEARCH A9).
const nodeTimeTimeout = 10 * time.Second

// nodeTimeFanout is how many nodes are asked for their clock at once.
const nodeTimeFanout = 4

// janitorEngine is janitor.Engine over the server's aggregator: a snapshot is read fresh from
// every node, never from the cache, and the swarm's services and tasks come from one manager.
type janitorEngine struct {
	agg      *envs.Aggregator
	clusters *envs.Clusters
	// facts are TierPlan's own stacks, which say whether the stack or project a container is
	// labelled with still exists. Nil means they are not read, and no compose container is removable.
	facts envs.StackFacts
	log   *slog.Logger
}

var _ janitor.Engine = (*janitorEngine)(nil)

// NewJanitorEngine is the janitor's engine over the server's aggregator and cluster reader, for
// the Runner the server builds. clusters may be nil on a server with no swarm reader; facts are the
// stacks of the environment (the same ones the Address space check reads), nil when there are none to
// ask, in which case no compose container is ever removable; log nil means slog.Default().
func NewJanitorEngine(agg *envs.Aggregator, clusters *envs.Clusters, facts envs.StackFacts, log *slog.Logger) janitor.Engine {
	if log == nil {
		log = slog.Default()
	}
	return &janitorEngine{agg: agg, clusters: clusters, facts: facts, log: log}
}

// Snapshot implements janitor.Engine.
func (e *janitorEngine) Snapshot(ctx context.Context, envID string) (janitor.Snapshot, error) {
	agg, _, err := e.agg.FreshSnapshot(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap := janitor.Snapshot{
		// An environment that is not a swarm has no service specs to read.
		ServicesRead: true, ReadAt: agg.FetchedAt, NotReporting: append([]string{}, agg.NotReporting...),
		Nodes: make([]janitor.Node, 0, len(agg.Nodes)), Containers: make([]janitor.Container, 0, len(agg.Containers)),
	}
	for _, n := range agg.Nodes {
		snap.Nodes = append(snap.Nodes, janitor.Node{ID: n.ID, Name: n.Name})
	}
	for _, n := range agg.Unreachable {
		snap.Unreachable = append(snap.Unreachable, janitor.Node{ID: n.ID, Name: n.Name})
	}
	for _, c := range agg.Containers {
		snap.Containers = append(snap.Containers, janitor.Container{
			ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), NodeID: c.NodeID, Node: c.Node, State: c.State,
			ImageID: c.ImageID, Created: c.Created, Labels: c.Labels,
		})
	}
	for _, img := range agg.Images {
		snap.Images = append(snap.Images, janitor.Image{
			ID: img.ID, NodeID: img.NodeID, Node: img.Node, RepoTags: img.RepoTags, RepoDigests: img.RepoDigests,
			Size: img.Size, Created: img.Created, Labels: img.Labels,
		})
	}
	e.readStacks(ctx, envID, &snap)
	snap.NodeTimes = e.readNodeTimes(ctx, envID, agg.Nodes)
	if e.clusters == nil {
		return snap, nil
	}
	swarm, err := e.clusters.IsSwarm(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap.Swarm = swarm
	if swarm {
		snap.ServicesRead = false
		e.readManager(ctx, envID, &snap)
	}
	return snap, nil
}

// readStacks fills the names and IDs of the TierPlan stacks of the environment. When they cannot be
// read StacksRead stays false, and the planner removes no compose container.
func (e *janitorEngine) readStacks(ctx context.Context, envID string, snap *janitor.Snapshot) {
	if e.facts == nil {
		return
	}
	names, ids, err := e.facts.Stacks(ctx, envID)
	if err != nil {
		e.log.Warn("the janitor could not read the stacks of an environment", "environment", envID, "error", err)
		return
	}
	snap.StackNames, snap.StackIDs, snap.StacksRead = names, ids, true
}

// readNodeTimes asks each node's engine what time it is, at most nodeTimeFanout nodes at once. A
// node whose engine does not say, or does not answer in time, has no entry: its containers are
// judged as before and the reason is logged (RESEARCH Pitfall 11, A9).
func (e *janitorEngine) readNodeTimes(ctx context.Context, envID string, nodes []envs.NodeRef) map[string]time.Time {
	var (
		mu  sync.Mutex
		out = make(map[string]time.Time, len(nodes))
	)
	var g errgroup.Group
	g.SetLimit(nodeTimeFanout)
	for _, n := range nodes {
		g.Go(func() error {
			ctx, cancel := context.WithTimeout(ctx, nodeTimeTimeout)
			defer cancel()
			var at time.Time
			err := e.agg.OnNode(ctx, envID, n.ID, func(c *dockerx.Client) error {
				var err error
				at, err = c.SystemTime(ctx)
				return err
			})
			switch {
			case err != nil:
				e.log.Warn("the janitor could not read the clock of a node", "environment", envID, "node", n.Name, "error", err)
			case at.IsZero():
				e.log.Info("the engine of a node does not report its time, so its clock is not checked", "environment", envID, "node", n.Name)
			default:
				mu.Lock()
				out[n.ID] = at
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// readManager fills the swarm's services and the tasks it wants running from one manager. When no
// manager answers, ManagerRead stays false and Services nil, so nothing is judged by whether a
// service exists.
func (e *janitorEngine) readManager(ctx context.Context, envID string, snap *janitor.Snapshot) {
	var (
		services map[string]janitor.Service
		tasks    []janitor.Task
		images   []string
	)
	err := e.clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		read, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		wanted, err := c.Tasks(ctx, dockerx.TaskFilter{DesiredState: "running"})
		if err != nil {
			return err
		}
		for _, ref := range read.ServiceRefs {
			if ref.Image != "" {
				images = append(images, ref.Image)
			}
		}
		services = make(map[string]janitor.Service, len(read.Services))
		for _, s := range read.Services {
			services[s.ID] = janitor.Service{Name: s.Name, Global: s.Mode == "global" || s.Mode == "global-job"}
		}
		tasks = make([]janitor.Task, 0, len(wanted))
		for _, t := range wanted {
			tasks = append(tasks, janitor.Task{ID: t.ID, ContainerID: t.ContainerID, ServiceID: t.ServiceID, NodeID: t.NodeID, DesiredState: t.DesiredState, Slot: t.Slot})
		}
		return nil
	})
	if err != nil {
		e.log.Warn("the janitor could not read the swarm's services", "environment", envID, "error", err)
		return
	}
	snap.ManagerRead, snap.Services, snap.Tasks = true, services, tasks
	snap.ServiceImages, snap.ServicesRead = images, true
}

// Inspect implements janitor.Engine: the end of each container on one node, with the size asked
// for. A container that cannot be inspected is left out.
func (e *janitorEngine) Inspect(ctx context.Context, envID, nodeID string, ids []string) (map[string]janitor.ContainerEnd, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectNodeTimeout)
	defer cancel()
	out := make(map[string]janitor.ContainerEnd, len(ids))
	err := e.agg.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		for _, id := range ids {
			if ctx.Err() != nil {
				return nil
			}
			end, err := c.ContainerEnd(ctx, id)
			if err != nil {
				continue
			}
			out[id] = janitor.ContainerEnd{
				StartedAt: end.StartedAt, FinishedAt: end.FinishedAt, ExitCode: end.ExitCode, Error: end.Error,
				OOMKilled: end.OOMKilled, Image: end.Image, ImageID: end.ImageID, SizeRw: end.SizeRw,
			}
		}
		return nil
	})
	return out, err
}

// removeContainerTimeout is how long inspecting and removing one container may take.
const removeContainerTimeout = time.Minute

// RemoveContainer implements janitor.Engine. On the container's node, through its agent, it
// inspects the container again (its state and labels) and removes it only if it is still exited and is not labelled
// tierplan.keep; the removal is neither forced nor takes the container's volumes (JAN-06). Nothing
// is retried: a removal whose answer was lost may have happened, and a second one would be wrong.
// The engine's refusal because the container is in use is ErrInUse, a container that is not there
// is ErrGone, a node that is not connected is ErrNodeOffline, and any other failure is a
// *janitor.CodedError.
func (e *janitorEngine) RemoveContainer(ctx context.Context, envID, nodeID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, removeContainerTimeout)
	defer cancel()
	err := e.agg.WriteOnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		d, _, err := c.ContainerDetail(ctx, id)
		if err != nil {
			return err
		}
		// The engine's own word for the state: only a container that ran and stopped is stale.
		if d.Status != "exited" || d.Labels[stacks.LabelKeep] == "true" {
			return janitor.ErrNoLongerStale
		}
		return c.ContainerRemove(ctx, id, dockerx.RemoveOptions{})
	})
	switch {
	case err == nil, errors.Is(err, janitor.ErrNoLongerStale):
		return err
	case errors.Is(err, envs.ErrNodeUnavailable):
		return fmt.Errorf("%w: %w", janitor.ErrNodeOffline, err)
	case dockerx.IsNotFound(err):
		return fmt.Errorf("%w: %w", janitor.ErrGone, err)
	case dockerx.IsConflict(err):
		return fmt.Errorf("%w: %w", janitor.ErrInUse, err)
	}
	// Anything else is a failure of the removal itself, given the word the containers API gives it:
	// the agent's policy refused, the host is read-only, the node did not answer, or it failed.
	code, _ := actionError(err)
	return &janitor.CodedError{Code: code, Err: err}
}
