// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package dockerx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// ErrNameTaken means the manager already has a secret or config of that name.
var ErrNameTaken = errors.New("dockerx: an object of that name already exists")

// ErrObjectInUse means a service's spec still references the secret or config, so the manager
// refused to remove it. It is shared with configs.
var ErrObjectInUse = errors.New("dockerx: the object is in use by a service")

// ErrObjectNotFound means the manager knows no such secret or config. It is shared with configs.
var ErrObjectNotFound = errors.New("dockerx: no such object")

// SecretRow is one swarm secret as the lists show it. A secret's value is not a field of it,
// because the manager never returns one.
type SecretRow struct {
	ID, Name             string
	Labels               map[string]string
	CreatedAt, UpdatedAt time.Time
}

// SecretSpec is a secret to create. Data is the value, which crosses Coaming exactly once, on
// its way to the manager; it is never logged, audited or kept.
type SecretSpec struct {
	Name   string
	Labels map[string]string
	Data   []byte
}

// secretRow turns a swarm secret into its row. Only the spec's name and labels are read; the
// spec's Data field is never touched, though the manager leaves it empty anyway.
func secretRow(s swarm.Secret) SecretRow {
	return SecretRow{
		ID: s.ID, Name: s.Spec.Name, Labels: nonNilLabels(s.Spec.Labels),
		CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
	}
}

// SecretList lists every secret of the swarm, sorted by name then ID. It is a manager
// question; a worker and a manager without quorum answer with errors whose text carries the
// daemon's own message.
func (c *Client) SecretList(ctx context.Context) ([]SecretRow, error) {
	res, err := c.cur().SecretList(ctx, client.SecretListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list swarm secrets on %s: %w", c.t.Describe(), err)
	}
	rows := make([]SecretRow, 0, len(res.Items))
	for _, s := range res.Items {
		rows = append(rows, secretRow(s))
	}
	slices.SortFunc(rows, func(a, b SecretRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return rows, nil
}

// SecretInspect reads one secret by ID or name, with the raw document, which has a name and
// labels and no value. A secret the manager does not know is ErrObjectNotFound.
func (c *Client) SecretInspect(ctx context.Context, id string) (SecretRow, []byte, error) {
	res, err := c.cur().SecretInspect(ctx, id, client.SecretInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return SecretRow{}, nil, fmt.Errorf("inspect secret on %s: %w", c.t.Describe(), ErrObjectNotFound)
		}
		return SecretRow{}, nil, fmt.Errorf("inspect secret on %s: %w", c.t.Describe(), err)
	}
	return secretRow(res.Secret), res.Raw, nil
}

// SecretCreate creates one secret once and returns its ID. It sends the name, the labels and
// the value and nothing else: no driver and no templating can be expressed. A name the swarm
// already has is ErrNameTaken. The write is never retried: whoever asked for it asks again
// (P2 D-42). The value is not kept and no error carries it.
func (c *Client) SecretCreate(ctx context.Context, spec SecretSpec) (string, error) {
	res, err := c.cur().SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: spec.Name, Labels: spec.Labels},
		Data:        spec.Data,
	}})
	if err != nil {
		if cerrdefs.IsConflict(err) || cerrdefs.IsAlreadyExists(err) || strings.Contains(err.Error(), alreadyExistsText) {
			return "", fmt.Errorf("create secret on %s: %w", c.t.Describe(), ErrNameTaken)
		}
		return "", fmt.Errorf("create secret on %s: %w", c.t.Describe(), err)
	}
	return res.ID, nil
}

// SecretRemove removes one secret by ID, once. A secret the manager does not know is
// ErrObjectNotFound, and one a service's spec still references, whatever its tasks are doing,
// is ErrObjectInUse (the manager enforces that itself, RESEARCH E16).
func (c *Client) SecretRemove(ctx context.Context, id string) error {
	if _, err := c.cur().SecretRemove(ctx, id, client.SecretRemoveOptions{}); err != nil {
		switch {
		case cerrdefs.IsNotFound(err):
			return fmt.Errorf("remove secret on %s: %w", c.t.Describe(), ErrObjectNotFound)
		case strings.Contains(err.Error(), inUseText):
			return fmt.Errorf("remove secret on %s: %w", c.t.Describe(), ErrObjectInUse)
		}
		return fmt.Errorf("remove secret on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// alreadyExistsText and inUseText are the manager's words for a name clash and for an object
// a service still references (RESEARCH E16); the status that carries them is not certain, so
// they are matched too.
const (
	alreadyExistsText = "already exists"
	inUseText         = "is in use by the following service"
)
