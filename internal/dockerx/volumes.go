// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// DriverLocal is the volume driver a volume is created with when none is named.
const DriverLocal = "local"

// VolumeCreateSpec is a volume to create. Volumes are node-local: the Client it is
// sent through is the node's.
type VolumeCreateSpec struct {
	Name string
	// Driver is the volume driver; empty means local.
	Driver string
	// DriverOpts are the options passed to the driver. For the local driver they are
	// type, o and device. They can hold a share's password, so they are never logged
	// or audited as they are.
	DriverOpts map[string]string
	Labels     map[string]string
}

// VolumeCreate creates the volume once and never retries: a create whose answer was
// lost may have happened (P2 D-42). The engine answers a create of a name that exists
// with the existing volume and no error, so the caller checks VolumeExists first.
func (c *Client) VolumeCreate(ctx context.Context, spec VolumeCreateSpec) error {
	driver := spec.Driver
	if driver == "" {
		driver = DriverLocal
	}
	if _, err := c.cur().VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: spec.Name, Driver: driver, DriverOpts: spec.DriverOpts, Labels: spec.Labels,
	}); err != nil {
		return fmt.Errorf("create volume on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// VolumeExists reports whether the engine has a volume of that name.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	_, ok, err := c.VolumeDriver(ctx, name)
	return ok, err
}

// VolumeDriver reads the driver of the volume of that name, and whether the engine has it.
func (c *Client) VolumeDriver(ctx context.Context, name string) (driver string, ok bool, err error) {
	res, err := c.cur().VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		if IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect volume on %s: %w", c.t.Describe(), err)
	}
	return res.Volume.Driver, true, nil
}

// VolumeRemove removes the volume once and never retries (P2 D-42). Force is never
// set: it would remove a volume a container still mounts, and the agent refuses it
// too. A volume a container uses is refused by the engine with a conflict, which
// IsConflict reports.
func (c *Client) VolumeRemove(ctx context.Context, name string) error {
	if _, err := c.cur().VolumeRemove(ctx, name, client.VolumeRemoveOptions{}); err != nil {
		return fmt.Errorf("remove volume on %s: %w", c.t.Describe(), err)
	}
	return nil
}
