// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/updates"
)

func replicated(n uint64) swarm.ServiceMode {
	return swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &n}}
}

func warnSpec(mode swarm.ServiceMode, order swarm.UpdateOrder) swarm.ServiceSpec {
	s := swarm.ServiceSpec{
		Mode:         mode,
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "registry.test/team/web:1"}},
	}
	if order != "" {
		s.UpdateConfig = &swarm.UpdateConfig{Order: order}
	}
	return s
}

func codes(ws []updates.Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

// What a person is told before an update (plan 12-16, D-05, RESEARCH Pitfall 10): each warning is worked out from the service's
// own spec and the image it runs, and only where it is true.
func TestServiceWarningsMatrix(t *testing.T) {
	t.Parallel()
	hc := &swarm.ContainerSpec{Image: "x", Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}}}
	withHC := func(s swarm.ServiceSpec) swarm.ServiceSpec { s.TaskTemplate.ContainerSpec = hc; return s }
	withNone := func(s swarm.ServiceSpec) swarm.ServiceSpec {
		s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "x", Healthcheck: &container.HealthConfig{Test: []string{"NONE"}}}
		return s
	}
	withHostPort := func(s swarm.ServiceSpec) swarm.ServiceSpec {
		s.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{
			{PublishedPort: 80, TargetPort: 80, PublishMode: swarm.PortConfigPublishModeIngress},
			{PublishedPort: 81, TargetPort: 81, PublishMode: swarm.PortConfigPublishModeHost},
			{PublishedPort: 82, TargetPort: 82, PublishMode: swarm.PortConfigPublishModeHost},
		}}
		return s
	}
	withMounts := func(types ...mount.Type) func(swarm.ServiceSpec) swarm.ServiceSpec {
		return func(s swarm.ServiceSpec) swarm.ServiceSpec {
			cs := *s.TaskTemplate.ContainerSpec
			for _, ty := range types {
				cs.Mounts = append(cs.Mounts, mount.Mount{Type: ty, Source: "s", Target: "/t"})
			}
			s.TaskTemplate.ContainerSpec = &cs
			return s
		}
	}
	id := func(s swarm.ServiceSpec) swarm.ServiceSpec { return s }

	tests := []struct {
		name  string
		spec  swarm.ServiceSpec
		image *bool
		want  []string
	}{
		{"three replicas stop-first have no downtime", withHC(warnSpec(replicated(3), swarm.UpdateOrderStopFirst)), nil, nil},
		{"one replica stop-first is down briefly", withHC(warnSpec(replicated(1), swarm.UpdateOrderStopFirst)), nil, []string{"downtime"}},
		{"one replica with no update config is stop-first", withHC(warnSpec(replicated(1), "")), nil, []string{"downtime"}},
		{"one replica start-first has no downtime", withHC(warnSpec(replicated(1), swarm.UpdateOrderStartFirst)), nil, nil},
		{"a global service has no downtime", withHC(warnSpec(swarm.ServiceMode{Global: &swarm.GlobalService{}}, "")), nil, nil},
		{"a health check in the spec", withHC(warnSpec(replicated(3), "")), nil, nil},
		{"a health check in the image", warnSpec(replicated(3), ""), ptr(true), nil},
		{"no health check anywhere", warnSpec(replicated(3), ""), ptr(false), []string{"no_healthcheck"}},
		{"an image that could not be inspected is read as none", warnSpec(replicated(3), ""), nil, []string{"no_healthcheck"}},
		{"a spec that switches the check off beats the image", withNone(warnSpec(replicated(3), "")), ptr(true), []string{"no_healthcheck"}},
		{"a host-mode port is one warning however many there are", withHC(withHostPort(warnSpec(replicated(3), ""))), nil, []string{"host_ports"}},
		{"a bind mount only has no volumes warning", withMounts(mount.TypeBind)(withHC(warnSpec(replicated(3), ""))), nil, nil},
		{"a volume mount is one warning", withMounts(mount.TypeVolume, mount.TypeBind, mount.TypeVolume)(withHC(warnSpec(replicated(3), ""))), nil, []string{"volumes"}},
		{"all of them, in order", withMounts(mount.TypeVolume)(withHostPort(id(warnSpec(replicated(1), "")))), nil,
			[]string{"downtime", "no_healthcheck", "host_ports", "volumes"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := updates.ServiceWarnings("web", tc.spec, tc.image)
			for _, w := range got {
				if w.Workload != "web" {
					t.Errorf("warning %+v names workload %q, want web", w, w.Workload)
				}
			}
			if g := codes(got); !slices.Equal(g, tc.want) {
				t.Errorf("warnings = %v, want %v", g, tc.want)
			}
		})
	}
}
