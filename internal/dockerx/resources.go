// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
)

// ContainerRow is one container as the list view shows it. It carries no
// variables, no metadata and no mounts: those can hold secrets (T-01-77).
type ContainerRow struct {
	ID      string
	Name    string
	State   string
	Status  string
	Image   string
	Created time.Time
	Ports   []string
}

// ImageRow is one image. RepoTags is "<none>" for an untagged image.
type ImageRow struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  time.Time
}

// VolumeRow is one volume.
type VolumeRow struct {
	Name    string
	Driver  string
	Created time.Time
}

// NetworkRow is one network.
type NetworkRow struct {
	ID      string
	Name    string
	Driver  string
	Scope   string
	Subnets []string
	Created time.Time
}

// Counts are the dashboard tiles, derived from the lists of one Snapshot.
type Counts struct {
	ContainersRunning int
	ContainersStopped int
	Containers        int
	Images            int
	Volumes           int
	Networks          int
	// Nodes and Services are set on swarm managers only.
	Nodes    *int
	Services *int
}

// Snapshot is everything the dashboard and the four list views show, read at
// one moment.
type Snapshot struct {
	Containers []ContainerRow
	Images     []ImageRow
	Volumes    []VolumeRow
	Networks   []NetworkRow
	Counts     Counts
	FetchedAt  time.Time
}

// Snapshot reads the resource lists.
func (c *Client) Snapshot(_ context.Context, _ bool) (Snapshot, error) {
	return Snapshot{}, errors.New("dockerx: Snapshot is not implemented")
}
