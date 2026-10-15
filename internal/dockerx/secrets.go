// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
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

// SecretSpec is a secret to create. Data is the value, which crosses Bayplan exactly once, on
// its way to the manager; it is never logged, audited or kept.
type SecretSpec struct {
	Name   string
	Labels map[string]string
	Data   []byte
}

// SecretList lists every secret of the swarm.
func (c *Client) SecretList(ctx context.Context) ([]SecretRow, error) {
	return nil, errors.New("not implemented")
}

// SecretInspect reads one secret by ID or name, with the raw document.
func (c *Client) SecretInspect(ctx context.Context, id string) (SecretRow, []byte, error) {
	return SecretRow{}, nil, errors.New("not implemented")
}

// SecretCreate creates one secret once and returns its ID.
func (c *Client) SecretCreate(ctx context.Context, spec SecretSpec) (string, error) {
	return "", errors.New("not implemented")
}

// SecretRemove removes one secret by ID, once.
func (c *Client) SecretRemove(ctx context.Context, id string) error {
	return errors.New("not implemented")
}
