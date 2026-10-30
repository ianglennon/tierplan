// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stacks

import (
	"maps"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

// ownershipLabelPrefix starts the key of each label that is Coaming's own bookkeeping (LabelStackID and the rest).
const ownershipLabelPrefix = "coaming."

// StripOwnership returns a copy of spec without any coaming.* label, on the service, on its containers and on each of
// its volume mounts. Drift is judged on both sides through it: an adopted stack's services carry none of Coaming's
// labels until its first deploy, and the labels say who owns a service, not how it is set up (D-21, D-24). The ID the
// converter puts on a named volume's mount is one of them: a stale one is replaced by the first deploy, and drift
// never reports it (D-11). The spec given is not changed.
func StripOwnership(spec swarm.ServiceSpec) swarm.ServiceSpec {
	spec.Labels = withoutOwnership(spec.Labels)
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		stripped := *cs
		stripped.Labels = withoutOwnership(cs.Labels)
		if cs.Mounts != nil {
			stripped.Mounts = make([]mount.Mount, len(cs.Mounts))
			for i, m := range cs.Mounts {
				if m.VolumeOptions != nil {
					vo := *m.VolumeOptions
					vo.Labels = withoutOwnership(vo.Labels)
					m.VolumeOptions = &vo
				}
				stripped.Mounts[i] = m
			}
		}
		spec.TaskTemplate.ContainerSpec = &stripped
	}
	return spec
}

// withoutOwnership is labels less the keys that start with coaming., as a new map; nil stays nil.
func withoutOwnership(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := maps.Clone(labels)
	for k := range out {
		if strings.HasPrefix(k, ownershipLabelPrefix) {
			delete(out, k)
		}
	}
	return out
}
