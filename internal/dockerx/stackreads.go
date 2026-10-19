// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"fmt"

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
