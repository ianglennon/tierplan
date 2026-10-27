// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The platforms of a service (TaskTemplate.Placement.Platforms) belong to its image: they are the os and
// architecture pairs the image was published for, and a swarm schedules a task only on a node that runs one of them
// (user decision 2026-10-27). docker stack deploy has the daemon resolve each image and stores them, so a stack made
// by it holds them and no compose file can say them. The converter never sees an image's platforms, so they are set
// where the image is pinned, as the docker client sets them after it converts; plan 08-31 adds the ones Bayplan
// looks up itself.

// withPin returns spec with the image of the pin and the platforms of that image. The spec given is not changed: the
// container spec and the placement are copied, and a placement is made when the spec has none. A pin that knows no
// platforms leaves the placement without any.
func withPin(spec swarm.ServiceSpec, p Pin) swarm.ServiceSpec {
	if spec.TaskTemplate.ContainerSpec != nil {
		cs := *spec.TaskTemplate.ContainerSpec
		cs.Image = p.Ref
		spec.TaskTemplate.ContainerSpec = &cs
	}
	pl := swarm.Placement{}
	if spec.TaskTemplate.Placement != nil {
		pl = *spec.TaskTemplate.Placement
	}
	pl.Platforms = slices.Clone(p.Platforms)
	spec.TaskTemplate.Placement = &pl
	return spec
}

// carryPlatforms fills the platforms of each pin that has none from the running service of the same name. It does so
// only when that service runs the image the pin names: both name the same sha256 digest, or neither names a digest
// and both are the same text. A pin whose image differs from the running one gets none, so a service on another
// image never borrows the platforms of the one it replaces (T-08-97). The pins map is changed in place.
func carryPlatforms(pins map[string]Pin, live map[string]dockerx.ServiceDetail) {
	for name, p := range pins {
		if len(p.Platforms) > 0 {
			continue
		}
		svc, ok := live[name]
		if !ok || !sameImageRef(p.Ref, svc.Image) {
			continue
		}
		if platforms := specPlatforms(svc.Spec); len(platforms) > 0 {
			p.Platforms = platforms
			pins[name] = p
		}
	}
}

// sameImageRef reports whether two image references name one image: the same sha256 digest, or no digest on either side
// and the same text.
func sameImageRef(a, b string) bool {
	da, db := digestOf(a), digestOf(b)
	if da != "" || db != "" {
		return da == db
	}
	return a != "" && strings.TrimSpace(a) == strings.TrimSpace(b)
}

// specPlatforms is the platforms of a stored service spec, nil when it holds none or does not read.
func specPlatforms(spec json.RawMessage) []swarm.Platform {
	var s struct {
		TaskTemplate struct {
			Placement *struct{ Platforms []swarm.Platform }
		}
	}
	if json.Unmarshal(spec, &s) != nil || s.TaskTemplate.Placement == nil {
		return nil
	}
	return slices.Clone(s.TaskTemplate.Placement.Platforms)
}
