// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"errors"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// HealthPublisher is where failover is announced: the health broker.
type HealthPublisher interface {
	Publish(ev HealthEvent) HealthEvent
	Notify(envID string)
}

// ClusterOptions configure Clusters. A zero field takes its default.
type ClusterOptions struct {
	Now       func() time.Time
	Publisher HealthPublisher
	Events    *Hub
	Grace     time.Duration
	Interval  time.Duration
	Timeout   time.Duration
	TTL       time.Duration
}

// ClusterState is what Clusters last learned about one environment.
type ClusterState struct {
	Read          dockerx.ClusterRead
	FetchedAt     time.Time
	LeaderNodeID  string
	LeaderAgentID string
	InUse         string
	NoQuorum      bool
}

// Clusters reads the cluster of swarm environments through a manager.
type Clusters struct{}

// NewClusters returns the cluster reader of svc.
func NewClusters(svc *Service, snaps *Snapshots, o ClusterOptions) *Clusters { return &Clusters{} }

var errNotImplemented = errors.New("envs: not implemented")

// ErrNoManager is the error of a cluster read of an environment that has no
// manager agent connected.
var ErrNoManager = errors.New("envs: no manager agent is connected")

// ClusterRead runs a read on a manager of the environment.
func (c *Clusters) ClusterRead(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	return errNotImplemented
}

// State is the cluster state, refreshed when it is older than the TTL.
func (c *Clusters) State(ctx context.Context, envID string) (ClusterState, error) {
	return ClusterState{}, errNotImplemented
}

// Cached is the cluster state as last read, with no I/O.
func (c *Clusters) Cached(envID string) (ClusterState, bool) { return ClusterState{}, false }

// Refresh reads the cluster now.
func (c *Clusters) Refresh(ctx context.Context, envID string) error { return errNotImplemented }

// RefreshAll refreshes every environment.
func (c *Clusters) RefreshAll(ctx context.Context) {}

// Run refreshes every environment on an interval until ctx ends.
func (c *Clusters) Run(ctx context.Context) {}

// View is the cluster view of an environment's rollup input.
func (c *Clusters) View(in RollupInput) *ClusterView { return nil }

// Manager is the monitor key of the manager a read of the cluster goes to.
func (c *Clusters) Manager(in RollupInput) (string, bool) { return "", false }

// LeaderAgent is the ID of the agent on the leading manager, or empty.
func (c *Clusters) LeaderAgent(envID string) string { return "" }

// Forget drops what is known of a removed environment.
func (c *Clusters) Forget(envID string) {}
