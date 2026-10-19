// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"slices"

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

// SecretsWithLabel lists the swarm secrets that carry the label key=value, sorted by name then ID,
// built the same way as ServicesWithLabel: the filter is one encoded term, and the label is
// checked again on each row. It is read-only and needs a manager. A value is not a field of a
// row, so none is read.
func (c *Client) SecretsWithLabel(ctx context.Context, key, value string) ([]SecretRow, error) {
	res, err := c.cur().SecretList(ctx, client.SecretListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
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
// sorted by name then ID, built the same way as SecretsWithLabel. It is read-only and needs a
// manager.
func (c *Client) ConfigsWithLabel(ctx context.Context, key, value string) ([]ConfigRow, error) {
	res, err := c.cur().ConfigList(ctx, client.ConfigListOptions{
		Filters: make(client.Filters).Add("label", key+"="+value),
	})
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
