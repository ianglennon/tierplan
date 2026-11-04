// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
)

// Pre-flight warnings (plan 12-16, D-05, RESEARCH Pitfall 10). They are what the compact confirm shows beside the digest
// change: the things an update can do that a person would want to know first. They are worked out on the server from the
// service's own spec and never from the browser.

// The codes of a Warning, in TierPlan's words.
const (
	// WarnDowntime: the only replica of a service is stopped before its replacement starts, so it is down for a moment.
	WarnDowntime = "downtime"
	// WarnContainerDowntime: a container is stopped and recreated, so it is down while that happens (plan 12-20).
	WarnContainerDowntime = "container_downtime"
	// WarnNoHealthcheck: neither the service nor its image defines a health check, so a new version that starts and then
	// fails cannot be told from one that works.
	WarnNoHealthcheck = "no_healthcheck"
	// WarnHostPorts: a port is published in host mode, so the new task cannot start on a node while the old one holds it.
	WarnHostPorts = "host_ports"
	// WarnVolumes: the service mounts a volume, which the new version will see in the state the old one left it.
	WarnVolumes = "volumes"
	// WarnPreviousFailure: an update of this workload to this digest failed before (plan 12-24).
	WarnPreviousFailure = "previous_failure"
)

// Warning is one thing to know before an update runs: which workload and a code.
type Warning struct {
	Workload, Code string
}

// ServiceWarnings are the warnings of updating the service name with that spec. imageHasHealthcheck says whether the image
// the service runs defines a health check: nil when the image could not be inspected, which reads as none.
//
//   - downtime: a replicated service with one replica whose update order is stop-first (the swarm's default).
//   - no_healthcheck: the spec defines none (or switches the image's off) and the image defines none, or could not be read.
//   - host_ports: a port published in host mode.
//   - volumes: a mount of type volume.
//
// The warnings come in that order.
func ServiceWarnings(name string, spec swarm.ServiceSpec, imageHasHealthcheck *bool) []Warning {
	var out []Warning
	add := func(code string) { out = append(out, Warning{Workload: name, Code: code}) }
	if r := spec.Mode.Replicated; r != nil && r.Replicas != nil && *r.Replicas == 1 &&
		(spec.UpdateConfig == nil || spec.UpdateConfig.Order != swarm.UpdateOrderStartFirst) {
		add(WarnDowntime)
	}
	if !specHasHealthcheck(spec, imageHasHealthcheck) {
		add(WarnNoHealthcheck)
	}
	if spec.EndpointSpec != nil {
		for _, p := range spec.EndpointSpec.Ports {
			if p.PublishMode == swarm.PortConfigPublishModeHost {
				add(WarnHostPorts)
				break
			}
		}
	}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, m := range cs.Mounts {
			if m.Type == mount.TypeVolume {
				add(WarnVolumes)
				break
			}
		}
	}
	return out
}

// specHasHealthcheck reports whether a service will be health-checked: its spec defines a check that runs, or says nothing
// and the image defines one. A spec that switches the check off (NONE) is not checked whatever the image says.
func specHasHealthcheck(spec swarm.ServiceSpec, image *bool) bool {
	cs := spec.TaskTemplate.ContainerSpec
	if cs != nil && cs.Healthcheck != nil && len(cs.Healthcheck.Test) > 0 {
		return cs.Healthcheck.Test[0] != "NONE"
	}
	return image != nil && *image
}

// ContainerWarnings are the warnings of updating the plain container name, from its inspect document. imageHasHealthcheck says
// whether the image the container runs defines a health check: nil when the image could not be inspected, which reads as none.
//
//   - container_downtime: always, since the container is stopped and recreated.
//   - no_healthcheck: the container defines no health check (or switches the image's off) and its image defines none.
//   - volumes: it mounts a volume, which the new version will see in the state the old one left it.
//
// The warnings come in that order.
func ContainerWarnings(name string, insp container.InspectResponse, imageHasHealthcheck *bool) []Warning {
	out := []Warning{{Workload: name, Code: WarnContainerDowntime}}
	if !containerHasHealthcheck(insp, imageHasHealthcheck) {
		out = append(out, Warning{Workload: name, Code: WarnNoHealthcheck})
	}
	for _, m := range insp.Mounts {
		if m.Type == mount.TypeVolume {
			out = append(out, Warning{Workload: name, Code: WarnVolumes})
			break
		}
	}
	return out
}

// containerHasHealthcheck reports whether a container will be health-checked: its config defines a check that runs, or says
// nothing and the image defines one. A config that switches the check off (NONE) is not checked whatever the image says.
func containerHasHealthcheck(insp container.InspectResponse, image *bool) bool {
	if insp.Config != nil && insp.Config.Healthcheck != nil && len(insp.Config.Healthcheck.Test) > 0 {
		return insp.Config.Healthcheck.Test[0] != "NONE"
	}
	return image != nil && *image
}
