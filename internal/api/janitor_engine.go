// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/janitor"
)

// inspectNodeTimeout is how long the inspects of one node may take before the node's containers
// are left un-inspected, which the planner reads as cant_tell.
const inspectNodeTimeout = time.Minute

// janitorEngine is janitor.Engine over the server's aggregator: a snapshot is read fresh from
// every node, never from the cache, and the swarm's services and tasks come from one manager.
type janitorEngine struct {
	agg      *envs.Aggregator
	clusters *envs.Clusters
	log      *slog.Logger
}

var _ janitor.Engine = (*janitorEngine)(nil)

// Snapshot implements janitor.Engine.
func (e *janitorEngine) Snapshot(ctx context.Context, envID string) (janitor.Snapshot, error) {
	agg, _, err := e.agg.FreshSnapshot(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap := janitor.Snapshot{
		ReadAt: agg.FetchedAt, NotReporting: append([]string{}, agg.NotReporting...),
		Nodes: make([]janitor.Node, 0, len(agg.Nodes)), Containers: make([]janitor.Container, 0, len(agg.Containers)),
	}
	for _, n := range agg.Nodes {
		snap.Nodes = append(snap.Nodes, janitor.Node{ID: n.ID, Name: n.Name})
	}
	for _, c := range agg.Containers {
		snap.Containers = append(snap.Containers, janitor.Container{
			ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), NodeID: c.NodeID, Node: c.Node, State: c.State,
			ImageID: c.ImageID, Created: c.Created, Labels: c.Labels,
		})
	}
	if e.clusters == nil {
		return snap, nil
	}
	swarm, err := e.clusters.IsSwarm(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap.Swarm = swarm
	if swarm {
		e.readManager(ctx, envID, &snap)
	}
	return snap, nil
}

// readManager fills the swarm's services and the tasks it wants running from one manager. When no
// manager answers, ManagerRead stays false and Services nil, so nothing is judged by whether a
// service exists.
func (e *janitorEngine) readManager(ctx context.Context, envID string, snap *janitor.Snapshot) {
	var (
		services map[string]janitor.Service
		tasks    []janitor.Task
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
