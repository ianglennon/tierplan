// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

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

// VolumeUsers names the containers on this node that mount the named volume, running or
// not: the engine's own list of every container, read now. A bind mount that happens to be
// spelled like the name is not a use of the volume.
func (c *Client) VolumeUsers(ctx context.Context, name string) ([]string, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	users := []string{}
	for _, row := range containerRows(cs.Items) {
		if slices.ContainsFunc(row.Mounts, func(m MountRow) bool { return m.Type == "volume" && m.Name == name }) {
			users = append(users, row.Name)
		}
	}
	return users, nil
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

// VolumeUsage is what the engine's disk usage report says about one volume.
type VolumeUsage struct {
	// Size is the bytes the volume takes, or -1 when the engine cannot say (a volume of a
	// driver other than local).
	Size int64
	// RefCount is how many containers reference the volume, or -1 when the engine does
	// not count them.
	RefCount int64
}

// VolumeSizes asks the engine for the disk usage of its volumes and answers it by volume
// name. The engine walks every volume's files to answer, which can take long on a host with
// large ones (and on an engine older than API 1.52 it computes the usage of images,
// containers and the build cache too, whatever is asked), so callers ask on request only,
// never on a refresh.
func (c *Client) VolumeSizes(ctx context.Context) (map[string]VolumeUsage, error) {
	res, err := c.cur().DiskUsage(ctx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, fmt.Errorf("volume disk usage on %s: %w", c.t.Describe(), err)
	}
	out := make(map[string]VolumeUsage, len(res.Volumes.Items))
	for _, v := range res.Volumes.Items {
		u := VolumeUsage{Size: -1, RefCount: -1}
		if v.UsageData != nil {
			u = VolumeUsage{Size: v.UsageData.Size, RefCount: v.UsageData.RefCount}
		}
		out[v.Name] = u
	}
	return out, nil
}

// VolumeDetail is one volume as its page shows it. Options can hold a share's credentials,
// so a caller sends them to a browser only through the redaction package.
type VolumeDetail struct {
	Name       string
	Driver     string
	Mountpoint string
	Scope      string
	// Created is zero when the engine does not say.
	Created time.Time
	// Options are the driver options the volume was created with, Labels its labels. Both are
	// never nil.
	Options map[string]string
	Labels  map[string]string
}

// VolumeDetail reads one volume by name, with the raw inspect document for the Inspect tab.
// A name the engine does not have is an error IsNotFound reports.
func (c *Client) VolumeDetail(ctx context.Context, name string) (VolumeDetail, []byte, error) {
	res, err := c.cur().VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err != nil {
		return VolumeDetail{}, nil, fmt.Errorf("inspect volume on %s: %w", c.t.Describe(), err)
	}
	v := res.Volume
	d := VolumeDetail{
		Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint, Scope: v.Scope,
		Options: map[string]string{}, Labels: map[string]string{},
	}
	maps.Copy(d.Options, v.Options)
	maps.Copy(d.Labels, v.Labels)
	if t, err := time.Parse(time.RFC3339Nano, v.CreatedAt); err == nil {
		d.Created = t
	}
	return d, res.Raw, nil
}
