// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ErrNodeNotFound means the manager knows no such node.
var ErrNodeNotFound = errors.New("dockerx: no such node")

// NodeDetail is one swarm node as an inspect shows it. The node's engine labels and
// spec labels are never nil.
type NodeDetail struct {
	ID string
	// Version is the object version a write must send back (swarm.Version.Index).
	Version uint64
	// Hostname is what the engine reports, Role and Availability are the spec's, State is
	// the manager's view of the node (ready, down, ...).
	Hostname, Role, Availability, State string
	// Address is the address the swarm reaches the node at.
	Address string
	// Reachability and ManagerAddr are those of a manager, empty for a worker.
	Reachability, ManagerAddr string
	EngineVersion, OS, Arch   string
	// Leader is true for the one manager that leads the swarm.
	Leader                bool
	NanoCPUs, MemoryBytes int64
	// Labels are the labels of the node's spec, EngineLabels those of its engine.
	Labels, EngineLabels map[string]string
	CreatedAt, UpdatedAt time.Time
}

// NodeInspect reads one node by ID and returns the typed detail and the raw document. A
// node the manager does not know is ErrNodeNotFound. A worker, and a manager that has
// lost quorum, answer with errors whose text carries the daemon's own message.
func (c *Client) NodeInspect(ctx context.Context, id string) (NodeDetail, []byte, error) {
	res, err := c.cur().NodeInspect(ctx, id, client.NodeInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return NodeDetail{}, nil, fmt.Errorf("inspect node on %s: %w", c.t.Describe(), ErrNodeNotFound)
		}
		return NodeDetail{}, nil, fmt.Errorf("inspect node on %s: %w", c.t.Describe(), err)
	}
	return nodeDetail(res.Node), res.Raw, nil
}

// nodeDetail turns an inspected node into its detail.
func nodeDetail(n swarm.Node) NodeDetail {
	d := NodeDetail{
		ID: n.ID, Version: n.Version.Index, Hostname: n.Description.Hostname,
		Role: string(n.Spec.Role), Availability: string(n.Spec.Availability), State: string(n.Status.State),
		Address: n.Status.Addr, EngineVersion: n.Description.Engine.EngineVersion,
		OS: n.Description.Platform.OS, Arch: n.Description.Platform.Architecture,
		NanoCPUs: n.Description.Resources.NanoCPUs, MemoryBytes: n.Description.Resources.MemoryBytes,
		Labels: nonNilLabels(n.Spec.Labels), EngineLabels: nonNilLabels(n.Description.Engine.Labels),
		CreatedAt: n.CreatedAt.UTC(), UpdatedAt: n.UpdatedAt.UTC(),
	}
	if m := n.ManagerStatus; m != nil {
		d.Leader, d.Reachability, d.ManagerAddr = m.Leader, string(m.Reachability), m.Addr
	}
	return d
}

// ErrNodeChanged means the node's version is no longer the one the caller read, so nothing was
// written. The caller reads the node again.
var ErrNodeChanged = errors.New("dockerx: the node changed since it was read")

// ErrLastManager is the daemon's refusal to demote the last manager of the swarm.
var ErrLastManager = errors.New("dockerx: attempting to demote the last manager of the swarm")

// lastManagerText is the daemon's words for that refusal (RESEARCH E14: HTTP 400).
const lastManagerText = "attempting to demote the last manager"

// NodeChange is one change to a node: the version the caller read, and the fields to change.
// An empty Availability or Role leaves that field alone. Labels replace the node's labels when
// they are not nil, or when SetLabels is true (a nil or empty map then clears them); with
// neither, labels are left alone.
type NodeChange struct {
	Version      uint64
	Availability string
	Role         string
	Labels       map[string]string
	SetLabels    bool
}

// NodeUpdate changes one node once. It reads the node, refuses a version that is not the one in
// ch (ErrNodeChanged) before anything is sent, then sends the node's whole spec with only the
// fields of ch changed and the version it just read. The swarm judges that version again, so a
// change in between is ErrNodeChanged too. A node the manager does not know is ErrNodeNotFound,
// and the daemon's refusal to demote the last manager is ErrLastManager. The write is never
// retried: whoever asked for it asks again (P2 D-42).
func (c *Client) NodeUpdate(ctx context.Context, id string, ch NodeChange) error {
	cli := c.cur()
	res, err := cli.NodeInspect(ctx, id, client.NodeInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return fmt.Errorf("inspect node on %s: %w", c.t.Describe(), ErrNodeNotFound)
		}
		return fmt.Errorf("inspect node on %s: %w", c.t.Describe(), err)
	}
	n := res.Node
	if n.Version.Index != ch.Version {
		return fmt.Errorf("update node on %s: %w", c.t.Describe(), ErrNodeChanged)
	}
	spec := n.Spec
	if ch.Availability != "" {
		spec.Availability = swarm.NodeAvailability(ch.Availability)
	}
	if ch.Role != "" {
		spec.Role = swarm.NodeRole(ch.Role)
	}
	if ch.SetLabels || ch.Labels != nil {
		spec.Labels = nonNilLabels(ch.Labels)
	}
	if _, err := cli.NodeUpdate(ctx, n.ID, client.NodeUpdateOptions{Version: n.Version, Spec: spec}); err != nil {
		switch {
		case strings.Contains(err.Error(), outOfSequence):
			return fmt.Errorf("update node on %s: %w", c.t.Describe(), ErrNodeChanged)
		case strings.Contains(err.Error(), lastManagerText):
			return fmt.Errorf("update node on %s: %w", c.t.Describe(), ErrLastManager)
		case cerrdefs.IsNotFound(err):
			return fmt.Errorf("update node on %s: %w", c.t.Describe(), ErrNodeNotFound)
		}
		return fmt.Errorf("update node on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// ErrNodeNotDown is the daemon's refusal to remove a node that is not down.
var ErrNodeNotDown = errors.New("dockerx: the node is not down and can't be removed")

// notDownText is the daemon's words for that refusal (RESEARCH E14: HTTP 400).
const notDownText = "is not down and can't be removed"

// NodeRemove removes the node id from the swarm once, and never forced: the call takes no options
// and sends no query, so the daemon itself refuses a node that is not down (ErrNodeNotDown). A node
// the manager does not know is ErrNodeNotFound. It is never retried: a remove whose answer was lost
// may have happened. The caller refuses a manager and a node that is not down before this is
// called; the daemon's refusal is the second line.
func (c *Client) NodeRemove(ctx context.Context, id string) error {
	if _, err := c.cur().NodeRemove(ctx, id, client.NodeRemoveOptions{}); err != nil {
		switch {
		case strings.Contains(err.Error(), notDownText):
			return fmt.Errorf("remove node on %s: %w", c.t.Describe(), ErrNodeNotDown)
		case cerrdefs.IsNotFound(err):
			return fmt.Errorf("remove node on %s: %w", c.t.Describe(), ErrNodeNotFound)
		}
		return fmt.Errorf("remove node on %s: %w", c.t.Describe(), err)
	}
	return nil
}
