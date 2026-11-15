// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"maps"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/stacks"
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

// TestStripOwnershipStripsTheLabelsOfVolumeMounts: the stack's ID the converter puts on a named volume's mount is
// TierPlan's too, and drift never reports it (D-11). Every other label and mount field stays, and the spec given is
// not changed.
func TestStripOwnershipStripsTheLabelsOfVolumeMounts(t *testing.T) {
	t.Parallel()
	spec := swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
		Image: "api",
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: "shop_data", Target: "/data", ReadOnly: true, VolumeOptions: &mount.VolumeOptions{
				NoCopy: true,
				Labels: map[string]string{stacks.LabelStackID: "stack-1", "com.docker.stack.namespace": "shop"},
			}},
			{Type: mount.TypeBind, Source: "/srv", Target: "/srv"},
			{Type: mount.TypeVolume, Source: "plain", Target: "/plain", VolumeOptions: &mount.VolumeOptions{}},
		},
	}}}

	got := stacks.StripOwnership(spec)

	m := got.TaskTemplate.ContainerSpec.Mounts
	if len(m) != 3 {
		t.Fatalf("mounts = %+v, want 3", m)
	}
	if want := map[string]string{"com.docker.stack.namespace": "shop"}; !maps.Equal(m[0].VolumeOptions.Labels, want) {
		t.Errorf("mount labels = %v, want %v", m[0].VolumeOptions.Labels, want)
	}
	if !m[0].ReadOnly || !m[0].VolumeOptions.NoCopy || m[0].Source != "shop_data" || m[0].Target != "/data" {
		t.Errorf("a field of the mount other than its labels changed: %+v", m[0])
	}
	if m[1].VolumeOptions != nil || m[1].Source != "/srv" {
		t.Errorf("the bind mount = %+v, want it unchanged", m[1])
	}
	if m[2].VolumeOptions == nil || len(m[2].VolumeOptions.Labels) != 0 {
		t.Errorf("a mount with no labels = %+v, want it unchanged", m[2])
	}
	orig := spec.TaskTemplate.ContainerSpec.Mounts[0].VolumeOptions.Labels
	if orig[stacks.LabelStackID] != "stack-1" || len(orig) != 2 {
		t.Errorf("StripOwnership changed the labels of the mount it was given: %v", orig)
	}
	if &spec.TaskTemplate.ContainerSpec.Mounts[0] == &got.TaskTemplate.ContainerSpec.Mounts[0] || spec.TaskTemplate.ContainerSpec.Mounts[0].VolumeOptions == got.TaskTemplate.ContainerSpec.Mounts[0].VolumeOptions {
		t.Error("the result shares its mounts with the spec it was given")
	}
}

// TestStripOwnershipKeepsKeepLabel: tierplan.keep is the user's label (JAN-07), so drift keeps it on both sides of the
// comparison: removing it from a running service while the stored definition has it shows as a difference, and the
// labels of TierPlan's own are still dropped.
func TestStripOwnershipKeepsKeepLabel(t *testing.T) {
	t.Parallel()
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "web_app", Labels: map[string]string{
			stacks.LabelStackID: "stack-1", stacks.LabelKeep: "true",
		}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:  "nginx",
			Labels: map[string]string{stacks.LabelStackID: "stack-1", stacks.LabelKeep: "true"},
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data", VolumeOptions: &mount.VolumeOptions{
				Labels: map[string]string{stacks.LabelStackID: "stack-1", stacks.LabelKeep: "true"},
			}}},
		}},
	}
	removed := stacks.StripOwnership(spec)
	delete(removed.Labels, stacks.LabelKeep)

	got := stacks.StripOwnership(spec)

	want := map[string]string{stacks.LabelKeep: "true"}
	if !maps.Equal(got.Labels, want) {
		t.Errorf("service labels = %v, want %v", got.Labels, want)
	}
	if cs := got.TaskTemplate.ContainerSpec; !maps.Equal(cs.Labels, want) || !maps.Equal(cs.Mounts[0].VolumeOptions.Labels, want) {
		t.Errorf("container labels = %v, mount labels = %v, want %v on both", cs.Labels, cs.Mounts[0].VolumeOptions.Labels, want)
	}
	if maps.Equal(got.Labels, removed.Labels) {
		t.Errorf("a service that lost tierplan.keep reads the same as one that has it: drift would never show the removal")
	}
}

// TestStripOwnershipKeepsIngressLabels: tierplan.ingress.* labels are the user's (LBL-05, D-22), so drift keeps them on
// both sides of the comparison: a live service that carries the same ingress labels as its file reads the same, and one
// that lost a label reads differently, like any user label. TierPlan's own labels are still dropped.
func TestStripOwnershipKeepsIngressLabels(t *testing.T) {
	t.Parallel()
	const host, port = "tierplan.ingress.host", "tierplan.ingress.port"
	labels := func() map[string]string {
		return map[string]string{stacks.LabelStackID: "stack-1", host: "app.example.org", port: "8080", "team": "a"}
	}
	spec := swarm.ServiceSpec{
		Annotations:  swarm.Annotations{Name: "web_app", Labels: labels()},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "nginx", Labels: labels()}},
	}
	want := map[string]string{host: "app.example.org", port: "8080", "team": "a"}

	got := stacks.StripOwnership(spec)
	if !maps.Equal(got.Labels, want) || !maps.Equal(got.TaskTemplate.ContainerSpec.Labels, want) {
		t.Errorf("service labels = %v, container labels = %v, want %v on both", got.Labels, got.TaskTemplate.ContainerSpec.Labels, want)
	}

	// The file's side of the comparison carries the same ingress labels and no bookkeeping, so the live
	// service reads the same as the file and there is no drift to report.
	file := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web_app", Labels: maps.Clone(want)}}
	if !maps.Equal(got.Labels, stacks.StripOwnership(file).Labels) {
		t.Errorf("live labels = %v, file labels = %v, want no drift", got.Labels, stacks.StripOwnership(file).Labels)
	}

	// A live service that lost one ingress label differs from the file, so the removal shows as drift.
	lost := spec
	lost.Labels = labels()
	delete(lost.Labels, port)
	if maps.Equal(stacks.StripOwnership(lost).Labels, got.Labels) {
		t.Errorf("a service that lost %s reads the same as one that has it: drift would never show the removal", port)
	}
}
