// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
)

// ErrNodeNotFound means the manager knows no such node.
var ErrNodeNotFound = errors.New("dockerx: no such node")

// NodeDetail is one swarm node as an inspect shows it.
type NodeDetail struct {
	ID string
	// Version is the object version a write must send back (swarm.Version.Index).
	Version                                                                 uint64
	Hostname, Role, Availability, State, Address, Reachability, ManagerAddr string
	EngineVersion, OS, Arch                                                 string
	Leader                                                                  bool
	NanoCPUs, MemoryBytes                                                   int64
	Labels, EngineLabels                                                    map[string]string
	CreatedAt, UpdatedAt                                                    time.Time
}

// NodeInspect is not written yet.
func (c *Client) NodeInspect(ctx context.Context, id string) (NodeDetail, []byte, error) {
	return NodeDetail{}, nil, errors.New("not implemented")
}
