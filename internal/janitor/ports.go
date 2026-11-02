// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package janitor

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// The ports of the janitor. Nothing here is a Docker type: the server fills these from the
// engines and the planner judges them, so the rules are a table test and the package imports
// neither the engine client nor the HTTP layer.

// Node is one node the snapshot was read from.
type Node struct {
	ID, Name string
}

// Container is one container as a list reports it, and the node it is on.
type Container struct {
	ID, Name string
	NodeID   string
	Node     string
	// State is the engine's state: created, running, paused, restarting, removing, exited or dead.
	State   string
	ImageID string
	Created time.Time
	Labels  map[string]string
}

// Service is one swarm service the manager listed.
type Service struct {
	Name string
	// Global is true for a service with a task on every node, whose containers are grouped per
	// node and not per slot.
	Global bool
}

// Task is one swarm task the manager listed as wanted running.
type Task struct {
	// ID is the task's ID, ContainerID the container it runs, empty until it has one.
	ID, ContainerID string
	ServiceID       string
	NodeID          string
	DesiredState    string
	Slot            int
}

// Snapshot is what the janitor reads of an environment at one moment.
type Snapshot struct {
	// Swarm is true for a swarm environment; ManagerRead is true when a manager answered the
	// questions about services and tasks.
	Swarm, ManagerRead bool
	// Nodes are the nodes that reported.
	Nodes []Node
	// NotReporting names the nodes that were expected and did not report. None of their objects
	// is in the snapshot.
	NotReporting []string
	Containers   []Container
	// Services are the swarm's services by ID, nil when no manager answered.
	Services map[string]Service
	// Tasks are the tasks the swarm wants running. Without a manager there are none.
	Tasks  []Task
	ReadAt time.Time
}

// ContainerEnd is how a container ended: what a list does not carry and an inspect does.
type ContainerEnd struct {
	StartedAt, FinishedAt time.Time
	ExitCode              int
	Error                 string
	OOMKilled             bool
	Image, ImageID        string
	// SizeRw is the size in bytes of the writable layer, -1 when it is not known.
	SizeRw int64
}

// ContainerRef names a container to inspect: its ID and the node it is on.
type ContainerRef struct {
	ID, NodeID string
}

// Engine is how the planner's input is read. The server implements it over its aggregator, so a
// snapshot is read fresh from the engines and never from a cache.
type Engine interface {
	// Snapshot reads the environment now.
	Snapshot(ctx context.Context, envID string) (Snapshot, error)
	// Inspect reads the end of the containers ids on one node. A container that could not be
	// inspected is left out of the answer.
	Inspect(ctx context.Context, envID, nodeID string, ids []string) (map[string]ContainerEnd, error)
	// RemoveContainer removes one container through the agent of its node, once and never retried,
	// without force and without its volumes. It inspects the container on that node first and
	// refuses one that is no longer exited or is labelled tierplan.keep, so what the planner
	// judged a moment ago is judged again at the engine that will do the removing.
	RemoveContainer(ctx context.Context, envID, nodeID, id string) error
}

// What RemoveContainer answers when it does not remove the container. The runner reads them with
// errors.Is; any other error is a failure of the removal itself.
var (
	// ErrNoLongerStale: the container is not exited any more, or is labelled tierplan.keep.
	ErrNoLongerStale = errors.New("janitor: the container is no longer stale")
	// ErrInUse: the engine refused the removal because the container is in use.
	ErrInUse = errors.New("janitor: the container is in use")
	// ErrGone: the container no longer exists on its node.
	ErrGone = errors.New("janitor: the container is already gone")
	// ErrNodeOffline: the node is not connected, so nothing was sent.
	ErrNodeOffline = errors.New("janitor: the node is not connected")
)

// inspectNodes is how many nodes are inspected at once. A node is asked one container at a time.
const inspectNodes = 4

// InspectCandidates reads the end of every container in refs, node by node, at most inspectNodes
// nodes at once. The answer holds a container only when its inspect succeeded: one that failed
// (it was removed meanwhile, or its node stopped answering) is absent, and the planner calls it
// cant_tell and never removes it. It never returns an error, because a failure is exactly that
// answer.
func InspectCandidates(ctx context.Context, eng Engine, envID string, refs []ContainerRef) map[string]ContainerEnd {
	byNode := map[string][]string{}
	var order []string
	for _, r := range refs {
		if _, seen := byNode[r.NodeID]; !seen {
			order = append(order, r.NodeID)
		}
		byNode[r.NodeID] = append(byNode[r.NodeID], r.ID)
	}
	var (
		mu  sync.Mutex
		out = map[string]ContainerEnd{}
	)
	var g errgroup.Group
	g.SetLimit(inspectNodes)
	for _, node := range order {
		g.Go(func() error {
			ends, err := eng.Inspect(ctx, envID, node, byNode[node])
			if err != nil {
				return nil //nolint:nilerr // a node that cannot be inspected leaves its containers un-inspected, which the planner reads as cant_tell
			}
			mu.Lock()
			defer mu.Unlock()
			maps.Copy(out, ends)
			return nil
		})
	}
	_ = g.Wait()
	return out
}
