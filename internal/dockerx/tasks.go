// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ErrUnfilteredTasks means a task read named no service, node, task or desired state.
// A swarm lists every task of every service for such a request, which on a large
// cluster is thousands of rows nobody asked for.
var ErrUnfilteredTasks = errors.New("dockerx: a task read needs a service, node, task or desired-state filter")

// DefaultTaskHistoryLimit is how many tasks per slot Docker keeps when the cluster never
// set TaskHistoryRetentionLimit.
const DefaultTaskHistoryLimit = 5

// TaskRow is one swarm task.
type TaskRow struct {
	ID, ServiceID, NodeID string
	// Slot is the replica number of a replicated service's task, 0 for a global one.
	Slot int
	// State is the state the swarm last saw the task in, DesiredState the state it wants.
	State, DesiredState string
	// Err and Message are the swarm's own free text about the task.
	Err, Message string
	// ContainerID is the task's container, empty until it has one.
	ContainerID string
	// ExitCode is the exit code of the task's container, nil when the task has no
	// container status.
	ExitCode *int
	// CreatedAt and UpdatedAt are the task's own times, StateSince when State was reached.
	CreatedAt, UpdatedAt, StateSince time.Time
	// SpecHash identifies the task's spec, Runtime aside. A task is on the service's
	// current version when it equals ServiceDetail.TemplateHash (RESEARCH E10).
	SpecHash string
	// MemoryLimit is the memory limit in bytes of the task's spec (Resources.Limits), 0
	// when the spec sets none. A reservation is not a limit.
	MemoryLimit int64
}

// TaskFilter narrows a task read. At least one field must be set.
type TaskFilter struct {
	ServiceIDs, NodeIDs, IDs []string
	DesiredState             string
	// Labels are "key=value" pairs the swarm matches against the labels of the task's service as they were when the
	// task was made. A task outlives its service for a while, so this is the one filter that still finds the tasks of
	// a service that was removed (a service filter is answered "not found" once the service is gone).
	Labels []string
}

// Tasks reads swarm tasks from a manager, newest first (by creation, then ID). A filter
// that names no service, node, task or desired state is ErrUnfilteredTasks and nothing is
// sent: the swarm would answer it with every task it has. A blank value counts as none.
func (c *Client) Tasks(ctx context.Context, f TaskFilter) ([]TaskRow, error) {
	filters := make(client.Filters)
	named := false
	for key, values := range map[string][]string{"service": f.ServiceIDs, "node": f.NodeIDs, "id": f.IDs, "desired-state": {f.DesiredState}, "label": f.Labels} {
		var clean []string
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				clean = append(clean, v)
			}
		}
		if len(clean) > 0 {
			filters.Add(key, clean...)
			named = true
		}
	}
	if !named {
		return nil, ErrUnfilteredTasks
	}
	res, err := c.cur().TaskList(ctx, client.TaskListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list swarm tasks on %s: %w", c.t.Describe(), err)
	}
	rows := make([]TaskRow, 0, len(res.Items))
	for _, t := range res.Items {
		row := TaskRow{
			ID: t.ID, ServiceID: t.ServiceID, NodeID: t.NodeID, Slot: t.Slot,
			State: string(t.Status.State), DesiredState: string(t.DesiredState),
			Err: t.Status.Err, Message: t.Status.Message,
			CreatedAt: t.CreatedAt.UTC(), UpdatedAt: t.UpdatedAt.UTC(), StateSince: t.Status.Timestamp.UTC(),
			SpecHash: specHash(t.Spec),
		}
		if r := t.Spec.Resources; r != nil && r.Limits != nil {
			row.MemoryLimit = r.Limits.MemoryBytes
		}
		if cs := t.Status.ContainerStatus; cs != nil {
			row.ContainerID = cs.ContainerID
			code := cs.ExitCode
			row.ExitCode = &code
		}
		rows = append(rows, row)
	}
	slices.SortStableFunc(rows, func(a, b TaskRow) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	return rows, nil
}

// TaskHistoryLimit is how many tasks per slot Docker keeps
// (Swarm.Cluster.Spec.Orchestration.TaskHistoryRetentionLimit of the engine's info), or
// DefaultTaskHistoryLimit when the engine does not say: an engine outside a swarm, a
// worker, which does not know the cluster's spec, or a cluster that never set it.
func (c *Client) TaskHistoryLimit(ctx context.Context) (int, error) {
	in, err := c.cur().Info(ctx, client.InfoOptions{})
	if err != nil {
		return 0, fmt.Errorf("read the engine info from %s: %w", c.t.Describe(), err)
	}
	if cl := in.Info.Swarm.Cluster; cl != nil {
		if lim := cl.Spec.Orchestration.TaskHistoryRetentionLimit; lim != nil && *lim >= 0 {
			return int(*lim), nil
		}
	}
	return DefaultTaskHistoryLimit, nil
}

// specHash is the SHA-256, in hex, of a task spec encoded as JSON with Runtime cleared: a
// task's spec equals its service's TaskTemplate except for Runtime (RESEARCH E10), so equal
// hashes mean the task runs the service's current template. The encoding sorts map keys,
// so two equal specs hash alike whatever order their maps were built in.
func specHash(spec swarm.TaskSpec) string {
	spec.Runtime = ""
	raw, err := json.Marshal(spec)
	if err != nil {
		// A TaskSpec holds only types the encoder accepts; an empty hash matches nothing.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
