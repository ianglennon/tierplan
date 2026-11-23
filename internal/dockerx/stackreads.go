// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ServicesWithLabel lists the swarm services that carry the label key=value, as details.
// The filter is built with the engine client's Filters type, so key and value are encoded for
// the query and never joined into it by hand. It is read-only and needs a manager.
func (c *Client) ServicesWithLabel(ctx context.Context, key, value string) ([]ServiceDetail, error) {
	ss, err := c.cur().ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("list swarm services on %s: %w", c.t.Describe(), err)
	}
	out := make([]ServiceDetail, 0, len(ss.Items))
	for _, s := range ss.Items {
		// A label filter matches on the key and value, but a daemon that ignores it would list
		// every service, so the label is checked again here.
		if s.Spec.Labels[key] != value {
			continue
		}
		d, err := serviceDetail(s)
		if err != nil {
			return nil, fmt.Errorf("read service on %s: %w", c.t.Describe(), err)
		}
		out = append(out, d)
	}
	return out, nil
}

// NetworksWithLabel lists the networks that carry the label key=value, sorted by name then ID,
// built the same way as ServicesWithLabel. It is read-only.
func (c *Client) NetworksWithLabel(ctx context.Context, key, value string) ([]NetworkRow, error) {
	ns, err := c.cur().NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("list networks on %s: %w", c.t.Describe(), err)
	}
	rows := networkRows(ns.Items)
	out := rows[:0]
	for _, r := range rows {
		if r.Labels[key] == value {
			out = append(out, r)
		}
	}
	return out, nil
}

// SecretsWithLabel lists the swarm secrets that carry the label key=value, sorted by name then ID.
// The list is asked for whole and the label is matched on each row here, because an agent's secrets
// list takes no filter (its route takes no query key, so a filtered list is refused on a real swarm
// and a fake engine cannot tell). It is read-only and needs a manager. A value is not a field of a
// row, so none is read.
func (c *Client) SecretsWithLabel(ctx context.Context, key, value string) ([]SecretRow, error) {
	res, err := c.cur().SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list swarm secrets on %s: %w", c.t.Describe(), err)
	}
	rows := make([]SecretRow, 0, len(res.Items))
	for _, s := range res.Items {
		if row := secretRow(s); row.Labels[key] == value {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b SecretRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// ConfigsWithLabel lists the swarm configs that carry the label key=value, without their content,
// sorted by name then ID, built the same way as SecretsWithLabel: asked for whole, matched here. It
// is read-only and needs a manager.
func (c *Client) ConfigsWithLabel(ctx context.Context, key, value string) ([]ConfigRow, error) {
	res, err := c.cur().ConfigList(ctx, client.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list swarm configs on %s: %w", c.t.Describe(), err)
	}
	rows := make([]ConfigRow, 0, len(res.Items))
	for i := range res.Items {
		if row := configRow(res.Items[i]); row.Labels[key] == value {
			rows = append(rows, row)
		}
		res.Items[i].Spec.Data = nil
	}
	slices.SortFunc(rows, func(a, b ConfigRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// ContainersWithLabel lists the containers, stopped ones too, that carry the label key=value,
// sorted by name then ID. The filter is built with the engine client's Filters type, so key and
// value are encoded for the query and never joined into it by hand; the label is checked again
// on each row. It is read-only and works on any engine, a swarm node's or a standalone host's.
func (c *Client) ContainersWithLabel(ctx context.Context, key, value string) ([]ContainerRow, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	rows := containerRows(cs.Items)
	out := rows[:0]
	for _, r := range rows {
		if r.Labels[key] == value {
			out = append(out, r)
		}
	}
	return out, nil
}

// VolumesWithLabel lists the volumes that carry the label key=value, sorted by name, built the
// same way as ContainersWithLabel. It is read-only.
func (c *Client) VolumesWithLabel(ctx context.Context, key, value string) ([]VolumeRow, error) {
	vs, err := c.cur().VolumeList(ctx, client.VolumeListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
	if err != nil {
		return nil, fmt.Errorf("list volumes on %s: %w", c.t.Describe(), err)
	}
	out := make([]VolumeRow, 0, len(vs.Items))
	for _, v := range vs.Items {
		if v.Labels[key] != value {
			continue
		}
		out = append(out, VolumeRow{
			Name: v.Name, Driver: v.Driver, Created: parseTime(v.CreatedAt),
			Labels: maps.Clone(v.Labels), Anonymous: IsAnonymousVolume(v.Name, v.Labels),
		})
	}
	slices.SortFunc(out, func(a, b VolumeRow) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// RunningContainers lists the containers that are running now, sorted by name then ID. It is
// read-only and works on any engine, a swarm node's or a standalone host's. The ingress
// preflight reads it to see which host ports the node already uses.
func (c *Client) RunningContainers(ctx context.Context) ([]ContainerRow, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list running containers on %s: %w", c.t.Describe(), err)
	}
	return containerRows(cs.Items), nil
}

// ContainersOnNetwork names the containers, stopped ones too, that are attached to the network of that name, sorted. An
// attached container is what keeps an engine from removing the network. It is read-only and works on any engine.
func (c *Client) ContainersOnNetwork(ctx context.Context, network string) ([]string, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	names := []string{}
	for _, row := range containerRows(cs.Items) {
		if slices.ContainsFunc(row.Networks, func(n ContainerNet) bool { return n.Name == network }) {
			names = append(names, row.Name)
		}
	}
	return names, nil
}

// PublishedPort is one host TCP port a container binds, as ContainersPublishing reads it.
type PublishedPort struct {
	ID, Name, State string
	Port            uint32
	Labels          map[string]string
}

// ContainersPublishing lists every container, whatever its state, that binds one of the host TCP ports, one
// row per container and port, sorted by name then port. A running container is read from the ports of the
// list; every other one is inspected and its HostConfig.PortBindings read, because an engine lists a created
// or exited container with no ports at all, and one with a restart policy takes its port back when the
// engine starts (D-12, D-31). A container that cannot be inspected is an error, never a skipped row: a
// preflight that missed a holder would say a port is free when it is not.
//
// Containers on the host network and processes outside Docker bind ports this read cannot see (Pitfall 11).
// It is read-only.
func (c *Client) ContainersPublishing(ctx context.Context, tcpPorts ...uint32) ([]PublishedPort, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	wanted := func(n uint32) bool { return slices.Contains(tcpPorts, n) }
	var out []PublishedPort
	for _, it := range cs.Items {
		row := PublishedPort{ID: it.ID, Name: containerName(it.ID, it.Names), State: string(it.State), Labels: maps.Clone(it.Labels)}
		seen := map[uint32]bool{}
		add := func(n uint32) {
			if wanted(n) && !seen[n] {
				seen[n] = true
				r := row
				r.Port = n
				out = append(out, r)
			}
		}
		if it.State == "running" {
			for _, p := range it.Ports {
				if p.Type == "tcp" && p.PublicPort != 0 {
					add(uint32(p.PublicPort))
				}
			}
			continue
		}
		res, err := c.cur().ContainerInspect(ctx, it.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect container %s on %s: %w", row.Name, c.t.Describe(), err)
		}
		if res.Container.HostConfig == nil {
			continue
		}
		for port, binds := range res.Container.HostConfig.PortBindings {
			if port.Proto() != network.TCP {
				continue
			}
			for _, b := range binds {
				// An empty host port is a binding the engine picks a port for, not a host port held; "8000-8010"
				// is a range, and holds every port in it.
				lo, hi, ok := hostPortRange(b.HostPort)
				if !ok {
					continue
				}
				for _, want := range tcpPorts {
					if want >= lo && want <= hi {
						add(want)
					}
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b PublishedPort) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Port, b.Port), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// hostPortRange reads a binding's HostPort, a number or a range "lo-hi"; ok is false for an empty or unreadable
// one.
func hostPortRange(s string) (lo, hi uint32, ok bool) {
	a, b, isRange := strings.Cut(s, "-")
	l, err := strconv.ParseUint(a, 10, 16)
	if err != nil || l == 0 {
		return 0, 0, false
	}
	if !isRange {
		return uint32(l), uint32(l), true
	}
	h, err := strconv.ParseUint(b, 10, 16)
	if err != nil || h < l {
		return 0, 0, false
	}
	return uint32(l), uint32(h), true
}
