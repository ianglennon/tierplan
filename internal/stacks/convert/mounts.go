// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package convert

import (
	"fmt"
	"maps"
	"os"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/mount"

	"github.com/ianglennon/tierplan/internal/stacks"
)

// serviceMounts converts a service's volumes to swarm mounts, in the order the file lists them.
//
// The conversion is ported from cli/compose/convert/volume.go in docker/cli (Apache-2.0),
// onto the compose-go v2 types, with three differences that are TierPlan's:
//
//   - a bind source must be an absolute path (ErrRelativeBind): a stack's compose file is not on
//     disk, so a relative source would only mean a path on the server (D-06);
//   - a named pipe mount is refused: a Windows pipe has no meaning on the Linux nodes a swarm
//     stack runs on here;
//   - the volume's ownership labels include tierplan.stack.id, and the stack's own labels win over
//     any the file sets under the same key.
//
// Options are written only when the file sets something for them, so a plain bind is a mount
// with no BindOptions, as docker stack deploy writes it.
func serviceMounts(svc composetypes.ServiceConfig, p *composetypes.Project, o Options) ([]mount.Mount, error) {
	if len(svc.Volumes) == 0 {
		return nil, nil
	}
	out := make([]mount.Mount, 0, len(svc.Volumes))
	for _, v := range svc.Volumes {
		m, err := convertMount(svc.Name, v, p, o)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func convertMount(service string, v composetypes.ServiceVolumeConfig, p *composetypes.Project, o Options) (mount.Mount, error) {
	m := mount.Mount{
		Type:        mount.Type(v.Type),
		Target:      v.Target,
		ReadOnly:    v.ReadOnly,
		Source:      v.Source,
		Consistency: mount.Consistency(v.Consistency),
	}
	switch v.Type {
	case composetypes.VolumeTypeVolume, "":
		m.Type = mount.TypeVolume
		return namedVolumeMount(service, v, m, p, o)
	case composetypes.VolumeTypeBind:
		return bindMount(service, v, m)
	case composetypes.VolumeTypeTmpfs:
		if v.Source != "" {
			return mount.Mount{}, fmt.Errorf("convert: service %s: a tmpfs mount at %s has a source", service, v.Target)
		}
		if v.Tmpfs != nil && (v.Tmpfs.Size != 0 || v.Tmpfs.Mode != 0) {
			m.TmpfsOptions = &mount.TmpfsOptions{SizeBytes: int64(v.Tmpfs.Size), Mode: os.FileMode(v.Tmpfs.Mode)}
		}
		return m, nil
	case composetypes.VolumeTypeImage:
		if v.Source == "" {
			return mount.Mount{}, fmt.Errorf("convert: service %s: an image mount at %s has no source", service, v.Target)
		}
		if v.Image != nil && v.Image.SubPath != "" {
			m.ImageOptions = &mount.ImageOptions{Subpath: v.Image.SubPath}
		}
		return m, nil
	}
	return mount.Mount{}, fmt.Errorf("convert: service %s: a %s mount at %s is not supported on a swarm (volume, bind, tmpfs and image are)", service, v.Type, v.Target)
}

// namedVolumeMount converts a volume mount. An anonymous volume (no source) is only a target; a
// named one must be declared, and takes the stack's labels and the volume's driver unless it is
// external.
func namedVolumeMount(service string, v composetypes.ServiceVolumeConfig, m mount.Mount, p *composetypes.Project, o Options) (mount.Mount, error) {
	if v.Source == "" {
		return m, nil
	}
	decl, declared := p.Volumes[v.Source]
	if !declared {
		return mount.Mount{}, fmt.Errorf("convert: service %s mounts the volume %q, which the file does not declare", service, v.Source)
	}
	m.Source = o.StackName + "_" + v.Source
	if decl.Name != "" {
		m.Source = decl.Name
	}
	m.VolumeOptions = &mount.VolumeOptions{}
	if v.Volume != nil {
		m.VolumeOptions.NoCopy = v.Volume.NoCopy
		m.VolumeOptions.Subpath = v.Volume.Subpath
	}
	if bool(decl.External) {
		return m, nil
	}
	labels := map[string]string{}
	if v.Volume != nil {
		maps.Copy(labels, v.Volume.Labels)
	}
	maps.Copy(labels, decl.Labels)
	labels[stacks.LabelSwarmNamespace] = o.StackName
	labels[stacks.LabelStackID] = o.StackID
	m.VolumeOptions.Labels = labels
	if decl.Driver != "" || len(decl.DriverOpts) > 0 {
		m.VolumeOptions.DriverConfig = &mount.Driver{Name: decl.Driver, Options: maps.Clone(decl.DriverOpts)}
	}
	return m, nil
}

// bindMount converts a bind mount, which must name an absolute path on the swarm node.
func bindMount(service string, v composetypes.ServiceVolumeConfig, m mount.Mount) (mount.Mount, error) {
	if v.Source == "" {
		return mount.Mount{}, fmt.Errorf("convert: service %s: a bind mount at %s has no source", service, v.Target)
	}
	if !strings.HasPrefix(v.Source, "/") {
		return mount.Mount{}, fmt.Errorf("%w: service %s mounts %q at %s", ErrRelativeBind, service, v.Source, v.Target)
	}
	if v.Bind != nil && v.Bind.Propagation != "" {
		m.BindOptions = &mount.BindOptions{Propagation: mount.Propagation(v.Bind.Propagation)}
	}
	return m, nil
}
