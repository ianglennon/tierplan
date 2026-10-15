// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"time"
)

// ConfigRow is one swarm config as the lists show it. The content is not a field of it.
type ConfigRow struct {
	ID, Name             string
	Labels               map[string]string
	CreatedAt, UpdatedAt time.Time
	// Templating is "golang" when the swarm evaluates the content as a Go template, else empty.
	Templating string
}

// ConfigDetail is one config with its content, read on the detail route only.
type ConfigDetail struct {
	ConfigRow
	Data []byte
}

// ConfigSpec is a config to create.
type ConfigSpec struct {
	Name       string
	Labels     map[string]string
	Data       []byte
	Templating string
}

var errConfigsNotImplemented = errors.New("not implemented")

// ConfigList is not implemented yet.
func (c *Client) ConfigList(ctx context.Context) ([]ConfigRow, error) {
	return nil, errConfigsNotImplemented
}

// ConfigInspect is not implemented yet.
func (c *Client) ConfigInspect(ctx context.Context, id string) (ConfigDetail, []byte, error) {
	return ConfigDetail{}, nil, errConfigsNotImplemented
}

// ConfigCreate is not implemented yet.
func (c *Client) ConfigCreate(ctx context.Context, spec ConfigSpec) (string, error) {
	return "", errConfigsNotImplemented
}

// ConfigRemove is not implemented yet.
func (c *Client) ConfigRemove(ctx context.Context, id string) error {
	return errConfigsNotImplemented
}
