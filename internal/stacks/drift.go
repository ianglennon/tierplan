// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks

import (
	"maps"
	"strings"

	"github.com/moby/moby/api/types/swarm"
)

// ownershipLabelPrefix starts the key of each label that is Bayplan's own bookkeeping (LabelStackID and the rest).
const ownershipLabelPrefix = "bayplan."

// StripOwnership returns a copy of spec without any bayplan.* label, on the service and on its containers. Drift is
// judged on both sides through it: an adopted stack's services carry none of Bayplan's labels until its first
// deploy, and the labels say who owns a service, not how it is set up (D-21, D-24). The spec given is not changed.
func StripOwnership(spec swarm.ServiceSpec) swarm.ServiceSpec {
	spec.Labels = withoutOwnership(spec.Labels)
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		stripped := *cs
		stripped.Labels = withoutOwnership(cs.Labels)
		spec.TaskTemplate.ContainerSpec = &stripped
	}
	return spec
}

// withoutOwnership is labels less the keys that start with bayplan., as a new map; nil stays nil.
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
