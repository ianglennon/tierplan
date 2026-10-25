// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"maps"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func TestStripOwnership(t *testing.T) {
	t.Parallel()
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "web_app", Labels: map[string]string{
			stacks.LabelStackID: "stack-1", stacks.LabelStackImage: "nginx", stacks.LabelSwarmNamespace: "web", "team": "a",
		}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:  "nginx",
			Labels: map[string]string{stacks.LabelStackID: "stack-1", "keep": "yes"},
		}},
	}
	before := maps.Clone(spec.Labels)

	got := stacks.StripOwnership(spec)

	if want := map[string]string{stacks.LabelSwarmNamespace: "web", "team": "a"}; !maps.Equal(got.Labels, want) {
		t.Errorf("service labels = %v, want %v", got.Labels, want)
	}
	if want := map[string]string{"keep": "yes"}; !maps.Equal(got.TaskTemplate.ContainerSpec.Labels, want) {
		t.Errorf("container labels = %v, want %v", got.TaskTemplate.ContainerSpec.Labels, want)
	}
	if !maps.Equal(spec.Labels, before) || spec.TaskTemplate.ContainerSpec.Labels[stacks.LabelStackID] != "stack-1" {
		t.Error("StripOwnership changed the spec it was given")
	}
	if got := stacks.StripOwnership(swarm.ServiceSpec{}); got.Labels != nil || got.TaskTemplate.ContainerSpec != nil {
		t.Errorf("an empty spec = %+v, want it unchanged", got)
	}
}
