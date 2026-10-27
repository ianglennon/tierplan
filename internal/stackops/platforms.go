// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
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

// platformMemoLimit is how many digests a PlatformMemo holds before it starts again.
const platformMemoLimit = 4096

// PlatformMemo remembers the platforms of the images Bayplan looked up, by the sha256 digest of the manifest. A digest
// names one manifest, so its platforms never change and nothing in the memo can go stale. A review fills it through
// lookupImage and the deploy that follows reads it, so that deploy needs no second registry call and takes no platform
// from the browser (T-08-98). It holds digests and os/arch pairs only (T-08-100). A nil *PlatformMemo remembers nothing
// and recalls nothing. It is safe for concurrent use.
type PlatformMemo struct {
	mu       sync.Mutex
	byDigest map[string][]swarm.Platform
}

// NewPlatformMemo returns an empty memo that holds at most 4096 digests and starts empty again when it is full; a miss
// only costs one lookup.
func NewPlatformMemo() *PlatformMemo { return &PlatformMemo{byDigest: map[string][]swarm.Platform{}} }

// remember keeps the platforms of a digest. A lookup that found none is not kept: a miss costs one lookup, and an
// answer of none is no fact worth holding.
func (m *PlatformMemo) remember(digest string, platforms []swarm.Platform) {
	if m == nil || !digestRe.MatchString(digest) || len(platforms) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, held := m.byDigest[digest]; !held && len(m.byDigest) >= platformMemoLimit {
		m.byDigest = map[string][]swarm.Platform{}
	}
	m.byDigest[digest] = slices.Clone(platforms)
}

// recall returns the platforms remembered for a digest.
func (m *PlatformMemo) recall(digest string) ([]swarm.Platform, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	platforms, ok := m.byDigest[digest]
	if !ok {
		return nil, false
	}
	return slices.Clone(platforms), true
}

// reviewedPlatforms gives each pin of a reviewed deploy that the review looked up (Action PinResolve, a reference that
// names a sha256 digest) the platforms of that digest: the ones the memo holds from the review, or else the ones one
// distribution check of that exact reference answers, which is then remembered. Nothing else is asked: a kept or
// as-written pin and a local image are served by carryPlatforms, and the caller's request never says a platform
// (T-08-98). A check that fails is classified as lookupImage classifies it; the first service that fails, in the order of
// items, is returned with the code, the service and the sentence of the failure. The pins map is changed in place.
func (d Deps) reviewedPlatforms(ctx context.Context, envID string, items []pinItem, pins map[string]Pin, read reader) (code, service, message string) {
	for _, it := range items {
		pin, ok := pins[it.Name]
		if !ok || pin.Action != stacks.PinResolve || len(pin.Platforms) > 0 {
			continue
		}
		digest := digestOf(pin.Ref)
		if !digestRe.MatchString(digest) {
			continue
		}
		platforms, held := d.Platforms.recall(digest)
		if !held {
			res := d.lookupPlatforms(ctx, envID, it.AsWritten, pin.Ref, read)
			switch {
			case res.infra != "":
				return res.infra, it.Name, ""
			case res.failure != "":
				return res.failure, it.Name, imageProblem(res, it).Message
			}
			platforms = res.platforms
			d.Platforms.remember(digest, platforms)
		}
		pin.Platforms = platforms
		pins[it.Name] = pin
	}
	return "", "", ""
}

// lookupPlatforms asks the registry, once, for the platforms of the reviewed reference ref (tag@digest) of the image
// written as text, through the engine read answers for, with the credential chosen for text.
func (d Deps) lookupPlatforms(ctx context.Context, envID, text, ref string, read reader) lookupResult {
	res := lookupResult{host: registryHost(text)}
	auth, err := d.authFor(ctx, envID, text)
	if err != nil {
		d.log().Error("could not resolve the registry credential of a stack image", "environment", envID, "image", text, "error", err)
		res.infra = CodeRegistryUnresolved
		return res
	}
	asked := ref
	if named, perr := reference.ParseNormalizedNamed(ref); perr == nil {
		asked = named.String()
	}
	var found dockerx.DistributionResult
	err = read(ctx, func(c *dockerx.Client) error {
		var rerr error
		found, rerr = c.DistributionInspect(ctx, asked, auth)
		return rerr
	})
	if err != nil {
		d.classifyLookup(ctx, envID, text, err, &res)
		return res
	}
	res.ref, res.platforms = ref, dockerx.SwarmPlatforms(found.Platforms)
	return res
}
