// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// untagged is how an image with no repository tag is listed.
const untagged = "<none>"

// ContainerRow is one container as the list view shows it. It carries no
// variables, no metadata and no mounts: those can hold secrets (T-01-77).
type ContainerRow struct {
	ID      string
	Name    string
	State   string
	Status  string
	Image   string
	Created time.Time
	// Ports reads like "0.0.0.0:8080->80/tcp", or "80/tcp" when not published.
	Ports []string
}

// ImageRow is one image. RepoTags is ["<none>"] for an untagged image.
type ImageRow struct {
	ID       string
	RepoTags []string
	Size     int64
	Created  time.Time
}

// VolumeRow is one volume. Created is the zero time when the engine did not say.
type VolumeRow struct {
	Name    string
	Driver  string
	Created time.Time
}

// NetworkRow is one network. Subnets is empty, not nil, for a network with none.
type NetworkRow struct {
	ID      string
	Name    string
	Driver  string
	Scope   string
	Subnets []string
	Created time.Time
}

// Counts are the dashboard tiles. Every one is derived from the lists of the
// same Snapshot, so a tile always equals the length of the list it links to
// (Pitfall 22).
type Counts struct {
	// ContainersRunning is the containers in state "running"; every other
	// state, including "removing", is stopped (Pitfall 21).
	ContainersRunning int
	ContainersStopped int
	Containers        int
	Images            int
	Volumes           int
	Networks          int
	// Nodes and Services are set on swarm managers only: a worker cannot list
	// them (Pattern 5).
	Nodes    *int
	Services *int
}

// Snapshot is everything the dashboard and the four list views show, read at
// one moment. No slice is ever nil.
type Snapshot struct {
	Containers []ContainerRow
	Images     []ImageRow
	Volumes    []VolumeRow
	Networks   []NetworkRow
	Counts     Counts
	FetchedAt  time.Time
}

// Snapshot reads the containers (stopped ones too), images, volumes and
// networks of the engine, and on a swarm manager also its nodes and services.
// Rows are sorted so equal keys keep a stable order: containers by name then
// ID, images newest first then ID, volumes by name, networks by name then ID.
//
// swarmManager comes from the probe. It must be false for a worker and for a
// standalone engine, which would refuse the swarm calls.
func (c *Client) Snapshot(ctx context.Context, swarmManager bool) (Snapshot, error) {
	cli := c.cur()

	var snap Snapshot

	cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	snap.Containers = containerRows(cs.Items)

	is, err := cli.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("list images on %s: %w", c.t.Describe(), err)
	}
	snap.Images = imageRows(is)

	vs, err := cli.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("list volumes on %s: %w", c.t.Describe(), err)
	}
	snap.Volumes = make([]VolumeRow, 0, len(vs.Items))
	for _, v := range vs.Items {
		snap.Volumes = append(snap.Volumes, VolumeRow{Name: v.Name, Driver: v.Driver, Created: parseTime(v.CreatedAt)})
	}
	slices.SortFunc(snap.Volumes, func(a, b VolumeRow) int { return cmp.Compare(a.Name, b.Name) })

	ns, err := cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return Snapshot{}, fmt.Errorf("list networks on %s: %w", c.t.Describe(), err)
	}
	snap.Networks = make([]NetworkRow, 0, len(ns.Items))
	for _, n := range ns.Items {
		row := NetworkRow{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Subnets: []string{}, Created: n.Created.UTC()}
		for _, cfg := range n.IPAM.Config {
			if cfg.Subnet.IsValid() {
				row.Subnets = append(row.Subnets, cfg.Subnet.String())
			}
		}
		snap.Networks = append(snap.Networks, row)
	}
	slices.SortFunc(snap.Networks, func(a, b NetworkRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})

	snap.Counts = Counts{
		Containers: len(snap.Containers),
		Images:     len(snap.Images),
		Volumes:    len(snap.Volumes),
		Networks:   len(snap.Networks),
	}
	for _, row := range snap.Containers {
		if row.State == string(container.StateRunning) {
			snap.Counts.ContainersRunning++
		}
	}
	snap.Counts.ContainersStopped = snap.Counts.Containers - snap.Counts.ContainersRunning

	if swarmManager {
		nodes, err := cli.NodeList(ctx, client.NodeListOptions{})
		if err != nil {
			if IsQuorumLoss(err) {
				// The node-local lists above are still good. The cluster counts are
				// left out, and the caller says why from the cluster read (D-43).
				snap.FetchedAt = time.Now().UTC()
				return snap, nil
			}
			return Snapshot{}, fmt.Errorf("list swarm nodes on %s: %w", c.t.Describe(), err)
		}
		services, err := cli.ServiceList(ctx, client.ServiceListOptions{})
		if err != nil {
			if IsQuorumLoss(err) {
				snap.FetchedAt = time.Now().UTC()
				return snap, nil
			}
			return Snapshot{}, fmt.Errorf("list swarm services on %s: %w", c.t.Describe(), err)
		}
		nodeCount, serviceCount := len(nodes.Items), len(services.Items)
		snap.Counts.Nodes, snap.Counts.Services = &nodeCount, &serviceCount
	}

	snap.FetchedAt = time.Now().UTC()
	return snap, nil
}

// containerRows maps the engine's container summaries to rows. Only the
// listed fields are read.
func containerRows(items []container.Summary) []ContainerRow {
	rows := make([]ContainerRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, ContainerRow{
			ID:      it.ID,
			Name:    containerName(it.ID, it.Names),
			State:   string(it.State),
			Status:  it.Status,
			Image:   it.Image,
			Created: time.Unix(it.Created, 0).UTC(),
			Ports:   portStrings(it.Ports),
		})
	}
	slices.SortFunc(rows, func(a, b ContainerRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return rows
}

// containerName is the name the container was given, without the leading
// slash. A container linked to others also lists names like "/other/alias";
// the one with no further slash is its own. With no usable name the short ID
// stands in.
func containerName(id string, names []string) string {
	for _, n := range names {
		if n = strings.TrimPrefix(n, "/"); n != "" && !strings.Contains(n, "/") {
			return n
		}
	}
	if len(names) > 0 {
		if n := strings.TrimPrefix(names[0], "/"); n != "" {
			return n
		}
	}
	return id[:min(len(id), 12)]
}

// portStrings formats published and exposed ports the way `docker ps` does.
// The engine lists them in no particular order, so they are sorted.
func portStrings(ports []container.PortSummary) []string {
	sorted := slices.Clone(ports)
	slices.SortFunc(sorted, func(a, b container.PortSummary) int {
		return cmp.Or(
			cmp.Compare(a.PrivatePort, b.PrivatePort),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.PublicPort, b.PublicPort),
			cmp.Compare(a.IP.String(), b.IP.String()),
		)
	})
	out := make([]string, 0, len(sorted))
	for _, p := range sorted {
		s := fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		if p.PublicPort != 0 {
			host := "0.0.0.0"
			if p.IP != (netip.Addr{}) {
				host = p.IP.String()
				if p.IP.Is6() {
					host = "[" + host + "]"
				}
			}
			s = fmt.Sprintf("%s:%d->%s", host, p.PublicPort, s)
		}
		out = append(out, s)
	}
	return out
}

// imageRows maps the engine's image list to rows.
func imageRows(res client.ImageListResult) []ImageRow {
	rows := make([]ImageRow, 0, len(res.Items))
	for _, it := range res.Items {
		tags := slices.DeleteFunc(slices.Clone(it.RepoTags), func(t string) bool { return t == "" || t == untagged+":"+untagged })
		if len(tags) == 0 {
			tags = []string{untagged}
		}
		rows = append(rows, ImageRow{ID: it.ID, RepoTags: tags, Size: it.Size, Created: time.Unix(it.Created, 0).UTC()})
	}
	slices.SortFunc(rows, func(a, b ImageRow) int {
		return cmp.Or(cmp.Compare(b.Created.Unix(), a.Created.Unix()), cmp.Compare(a.ID, b.ID))
	})
	return rows
}

// parseTime reads an RFC 3339 timestamp, with or without fractional seconds.
// It returns the zero time for an empty or unreadable value.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
