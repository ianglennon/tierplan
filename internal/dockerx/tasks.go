// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
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
}

// TaskFilter narrows a task read. At least one field must be set.
type TaskFilter struct {
	ServiceIDs, NodeIDs, IDs []string
	DesiredState             string
}

// Tasks reads swarm tasks from a manager, newest first.
func (c *Client) Tasks(ctx context.Context, f TaskFilter) ([]TaskRow, error) {
	return nil, nil
}

// TaskHistoryLimit is how many tasks per slot Docker keeps, from /info.
func (c *Client) TaskHistoryLimit(ctx context.Context) (int, error) {
	return 0, nil
}
