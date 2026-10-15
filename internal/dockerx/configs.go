// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ConfigRow is one swarm config as the lists show it. The content is not a field of it: the
// manager sends every config's content with the list (RESEARCH E17), and dropping it here, at
// the edge, keeps it out of every cache, response and log the row could reach.
type ConfigRow struct {
	ID, Name             string
	Labels               map[string]string
	CreatedAt, UpdatedAt time.Time
	// Templating is "golang" when the swarm evaluates the content as a Go template, else empty.
	Templating string
}

// ConfigDetail is one config with its content. It is read on the detail route only.
type ConfigDetail struct {
	ConfigRow
	Data []byte
}

// ConfigSpec is a config to create. Templating is "golang" or empty; a config has no other
// driver.
type ConfigSpec struct {
	Name       string
	Labels     map[string]string
	Data       []byte
	Templating string
}

// configRow turns a swarm config into its row. Only the spec's name, labels and templating
// driver are read; its Data field is never touched.
func configRow(c swarm.Config) ConfigRow {
	row := ConfigRow{
		ID: c.ID, Name: c.Spec.Name, Labels: nonNilLabels(c.Spec.Labels),
		CreatedAt: c.CreatedAt.UTC(), UpdatedAt: c.UpdatedAt.UTC(),
	}
	if t := c.Spec.Templating; t != nil {
		row.Templating = t.Name
	}
	return row
}

// ConfigList lists every config of the swarm without its content, sorted by name then ID. It
// is a manager question; a worker and a manager without quorum answer with errors whose text
// carries the daemon's own message. The manager sends the content of every config, so a swarm
// of large configs can make the answer too large for the tunnel; that error is returned as it
// came, for the caller to tell the user.
func (c *Client) ConfigList(ctx context.Context) ([]ConfigRow, error) {
	res, err := c.cur().ConfigList(ctx, client.ConfigListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list swarm configs on %s: %w", c.t.Describe(), err)
	}
	rows := make([]ConfigRow, 0, len(res.Items))
	for i := range res.Items {
		rows = append(rows, configRow(res.Items[i]))
		res.Items[i].Spec.Data = nil
	}
	slices.SortFunc(rows, func(a, b ConfigRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// ConfigInspect reads one config by ID or name with its content and the raw document. A config
// the manager does not know is ErrObjectNotFound.
func (c *Client) ConfigInspect(ctx context.Context, id string) (ConfigDetail, []byte, error) {
	res, err := c.cur().ConfigInspect(ctx, id, client.ConfigInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return ConfigDetail{}, nil, fmt.Errorf("inspect config on %s: %w", c.t.Describe(), ErrObjectNotFound)
		}
		return ConfigDetail{}, nil, fmt.Errorf("inspect config on %s: %w", c.t.Describe(), err)
	}
	return ConfigDetail{ConfigRow: configRow(res.Config), Data: res.Config.Spec.Data}, res.Raw, nil
}

// ConfigCreate creates one config once and returns its ID. It sends the name, the labels, the
// content and, when asked for, the golang templating driver, and nothing else. A name the swarm
// already has is ErrNameTaken. The write is never retried: whoever asked for it asks again
// (P2 D-42).
func (c *Client) ConfigCreate(ctx context.Context, spec ConfigSpec) (string, error) {
	sw := swarm.ConfigSpec{
		Annotations: swarm.Annotations{Name: spec.Name, Labels: spec.Labels},
		Data:        spec.Data,
	}
	if spec.Templating != "" {
		sw.Templating = &swarm.Driver{Name: spec.Templating}
	}
	res, err := c.cur().ConfigCreate(ctx, client.ConfigCreateOptions{Spec: sw})
	if err != nil {
		if cerrdefs.IsConflict(err) || cerrdefs.IsAlreadyExists(err) || strings.Contains(err.Error(), alreadyExistsText) {
			return "", fmt.Errorf("create config on %s: %w", c.t.Describe(), ErrNameTaken)
		}
		return "", fmt.Errorf("create config on %s: %w", c.t.Describe(), err)
	}
	return res.ID, nil
}

// ConfigRemove removes one config by ID, once. A config the manager does not know is
// ErrObjectNotFound, and one a service's spec still references, whatever its tasks are doing,
// is ErrObjectInUse (the manager enforces that itself, RESEARCH E16).
func (c *Client) ConfigRemove(ctx context.Context, id string) error {
	if _, err := c.cur().ConfigRemove(ctx, id, client.ConfigRemoveOptions{}); err != nil {
		switch {
		case cerrdefs.IsNotFound(err):
			return fmt.Errorf("remove config on %s: %w", c.t.Describe(), ErrObjectNotFound)
		case strings.Contains(err.Error(), inUseText):
			return fmt.Errorf("remove config on %s: %w", c.t.Describe(), ErrObjectInUse)
		}
		return fmt.Errorf("remove config on %s: %w", c.t.Describe(), err)
	}
	return nil
}
