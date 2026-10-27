// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

const (
	platformsDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	platformsDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// runningWith is a stored service running image with the platforms given.
func runningWith(t *testing.T, image string, platforms ...swarm.Platform) dockerx.ServiceDetail {
	t.Helper()
	raw, err := json.Marshal(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		ContainerSpec: &swarm.ContainerSpec{Image: image},
		Placement:     &swarm.Placement{Platforms: platforms},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return dockerx.ServiceDetail{Image: image, Spec: raw}
}

// TestCarryPlatformsKeepsThePlatformsOfTheSameImageOnly: the platforms belong to the image. A pin takes the running
// platforms while the service runs the image it names, and never from a service on another image (T-08-97).
func TestCarryPlatformsKeepsThePlatformsOfTheSameImageOnly(t *testing.T) {
	t.Parallel()
	amd, arm := swarm.Platform{OS: "linux", Architecture: "amd64"}, swarm.Platform{OS: "linux", Architecture: "arm64"}
	live := map[string]dockerx.ServiceDetail{
		"s_same":    runningWith(t, "nginx:1.27@"+platformsDigestA, amd, arm),
		"s_other":   runningWith(t, "nginx:1.28@"+platformsDigestB, amd, arm),
		"s_nodig":   runningWith(t, "local/app:1", amd),
		"s_nodig2":  runningWith(t, "local/app:2", amd),
		"s_mixed":   runningWith(t, "nginx:1.27", amd),
		"s_noplats": runningWith(t, "nginx:1.27@"+platformsDigestA),
	}
	pins := map[string]Pin{
		"s_same":    {Service: "s_same", Ref: "nginx:1.27@" + platformsDigestA},
		"s_other":   {Service: "s_other", Ref: "nginx:1.27@" + platformsDigestA},
		"s_nodig":   {Service: "s_nodig", Ref: "local/app:1"},
		"s_nodig2":  {Service: "s_nodig2", Ref: "local/app:1"},
		"s_mixed":   {Service: "s_mixed", Ref: "nginx:1.27@" + platformsDigestA},
		"s_noplats": {Service: "s_noplats", Ref: "nginx:1.27@" + platformsDigestA},
		"s_absent":  {Service: "s_absent", Ref: "nginx:1.27@" + platformsDigestA},
		"s_kept":    {Service: "s_kept", Ref: "nginx:1.27@" + platformsDigestA, Platforms: []swarm.Platform{arm}},
	}
	live["s_kept"] = runningWith(t, "nginx:1.27@"+platformsDigestA, amd)

	carryPlatforms(pins, live)

	for name, want := range map[string][]swarm.Platform{
		"s_same":    {amd, arm},
		"s_other":   nil,
		"s_nodig":   {amd},
		"s_nodig2":  nil,
		"s_mixed":   nil,
		"s_noplats": nil,
		"s_absent":  nil,
		"s_kept":    {arm},
	} {
		if got := pins[name].Platforms; !slices.Equal(got, want) {
			t.Errorf("%s: platforms = %v, want %v", name, got, want)
		}
	}
}

// TestWithPinSetsTheImageAndItsPlatformsOnACopy: the spec given is not changed, and a pin with no platforms leaves
// the placement without any.
func TestWithPinSetsTheImageAndItsPlatformsOnACopy(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	spec := swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		ContainerSpec: &swarm.ContainerSpec{Image: "nginx:1.27"},
		Placement:     &swarm.Placement{Constraints: []string{"node.role==worker"}, Platforms: []swarm.Platform{{OS: "plan9"}}},
	}}

	got := withPin(spec, Pin{Ref: "nginx:1.27@" + platformsDigestA, Platforms: []swarm.Platform{amd}})

	if got.TaskTemplate.ContainerSpec.Image != "nginx:1.27@"+platformsDigestA || !slices.Equal(got.TaskTemplate.Placement.Platforms, []swarm.Platform{amd}) {
		t.Errorf("pinned spec = %+v / %+v", got.TaskTemplate.ContainerSpec, got.TaskTemplate.Placement)
	}
	if !slices.Equal(got.TaskTemplate.Placement.Constraints, []string{"node.role==worker"}) {
		t.Errorf("constraints = %v, want them kept", got.TaskTemplate.Placement.Constraints)
	}
	if spec.TaskTemplate.ContainerSpec.Image != "nginx:1.27" || spec.TaskTemplate.Placement.Platforms[0].OS != "plan9" {
		t.Errorf("withPin changed the spec it was given: %+v / %+v", spec.TaskTemplate.ContainerSpec, spec.TaskTemplate.Placement)
	}

	bare := withPin(swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "x"}}}, Pin{Ref: "x@" + platformsDigestB})
	if bare.TaskTemplate.Placement == nil || len(bare.TaskTemplate.Placement.Platforms) != 0 {
		t.Errorf("a pin without platforms gave placement %+v, want an empty one", bare.TaskTemplate.Placement)
	}
}
