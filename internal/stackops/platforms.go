// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The platforms of a service (TaskTemplate.Placement.Platforms) belong to its image: they are the os and
// architecture pairs the image was published for, and a swarm schedules a task only on a node that runs one of them
// (user decision 2026-10-27). docker stack deploy has the daemon resolve each image and stores them, so a stack made
// by it holds them and no compose file can say them. The converter never sees an image's platforms, so they are set
// where the image is pinned, as the docker client sets them after it converts; plan 08-31 adds the ones TierPlan
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

// recordedPlatforms is the platforms the stack's last good revision recorded for the service of a pin, when that record
// names the same sha256 digest as the pin. A record made for another image is never applied (T-08-111).
func recordedPlatforms(p Pin, name string, stored map[string]stacks.LivePin) []swarm.Platform {
	rec, ok := stored[name]
	if !ok || len(rec.Platforms) == 0 {
		return nil
	}
	digest := digestOf(p.Ref)
	if !digestRe.MatchString(digest) || digestOf(rec.Image) != digest {
		return nil
	}
	return slices.Clone(rec.Platforms)
}

// pinPlatforms gives each pin its platforms by one precedence, which the deploy review and every deploy that looks images
// up apply alike, so the review shows the placement the deploy sends (WR-02): what the stack's last good revision recorded
// for the pin's sha256 digest comes first, even over what a registry lookup just said for it, so a redeploy puts back
// platforms someone took off outside TierPlan and the review shows it; a pin that has none yet then takes those of the
// running service of the same image (carryPlatforms). Nothing is asked of a registry here (P7 D-18). The pins map is
// changed in place.
func pinPlatforms(pins map[string]Pin, stored map[string]stacks.LivePin, live map[string]dockerx.ServiceDetail) {
	for name, p := range pins {
		if recorded := recordedPlatforms(p, name, stored); len(recorded) > 0 {
			p.Platforms = recorded
			pins[name] = p
		}
	}
	carryPlatforms(pins, live)
}

// storedPlatforms fills the platforms of each pin that has none from the record of the stack's last good revision
// (stored, by service name), by the same rule as pinPlatforms but never replacing platforms a pin already has: a
// rollback's pins carry the platforms their own revision stored, and those stand. The pins map is changed in place.
func storedPlatforms(pins map[string]Pin, stored map[string]stacks.LivePin) {
	for name, p := range pins {
		if len(p.Platforms) > 0 {
			continue
		}
		if recorded := recordedPlatforms(p, name, stored); len(recorded) > 0 {
			p.Platforms = recorded
			pins[name] = p
		}
	}
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

// PlatformMemo remembers the platforms of the images TierPlan looked up, by the repository the image was looked up in and
// the sha256 digest of its manifest, within the environment that looked it up. A digest names one manifest, but only a
// registry that tells the truth keeps it so, and the memo is one for the whole server, so an answer is held only for the
// repository it came from: a lookup of one repository never answers for another that names the same digest (CR-01). Each
// environment has its own registry credentials and mirrors, so one environment's answer is never recalled for another
// (T-08-114). A second answer that differs from the one held
// for a key drops the entry, so the digest is looked up again and no registry can overwrite what another one said. A
// review fills the memo through lookupImage and the deploy that follows reads it, so that deploy needs no second registry
// call and takes no platform from the browser (T-08-98). It holds repositories, digests and os/arch pairs only (T-08-100).
// A nil *PlatformMemo remembers nothing and recalls nothing. It is safe for concurrent use.
type PlatformMemo struct {
	mu    sync.Mutex
	byKey map[string][]swarm.Platform
}

// NewPlatformMemo returns an empty memo that holds at most 4096 answers and starts empty again when it is full; a miss
// only costs one lookup.
func NewPlatformMemo() *PlatformMemo { return &PlatformMemo{byKey: map[string][]swarm.Platform{}} }

// memoKey is the key of the answer for the image written as text with the manifest digest in the environment envID: the
// environment, the normalised repository (no tag, no digest, the registry host always spelled out) and the digest. It is
// empty when envID is empty, digest is not a sha256 digest or text does not parse as an image reference, and nothing is
// kept or recalled for an empty key.
func memoKey(envID, text, digest string) string {
	if envID == "" || !digestRe.MatchString(digest) {
		return ""
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(text))
	if err != nil {
		return ""
	}
	return envID + "\x00" + reference.TrimNamed(named).String() + "@" + digest
}

// remember keeps the platforms of a digest as looked up in the repository of text from the environment envID. A lookup that found none is kept as
// that answer too, so a deploy that follows the review does not ask the registry again for a digest that names no
// platform. An answer that differs from the one held for the same repository and digest drops the entry instead of
// replacing it.
func (m *PlatformMemo) remember(envID, text, digest string, platforms []swarm.Platform) {
	key := memoKey(envID, text, digest)
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if held, ok := m.byKey[key]; ok {
		if !slices.Equal(held, platforms) {
			delete(m.byKey, key)
		}
		return
	}
	if len(m.byKey) >= platformMemoLimit {
		m.byKey = map[string][]swarm.Platform{}
	}
	m.byKey[key] = append([]swarm.Platform{}, platforms...)
}

// Remember records that the digest of the image written as text, looked up from the environment envID, carries
// platforms, so a deploy that follows an image update's check needs no daemon call of its own for it (a revision that
// predates the Phase 8 platform record has none to carry over). An empty list records nothing: a digest that names no
// platform is a single-manifest image, and the deploy keeps its own one lookup for it. It is remember for a caller
// outside the package, with the same keying, the same refusal of a key that is not usable and the same disagreement
// rule; a nil memo remembers nothing.
func (m *PlatformMemo) Remember(envID, text, digest string, platforms []swarm.Platform) {
	if len(platforms) == 0 {
		return
	}
	m.remember(envID, text, digest, platforms)
}

// recall returns the platforms remembered for a digest in the repository of text, as looked up from the environment envID.
func (m *PlatformMemo) recall(envID, text, digest string) ([]swarm.Platform, bool) {
	key := memoKey(envID, text, digest)
	if m == nil || key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	platforms, ok := m.byKey[key]
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
		platforms, held := d.Platforms.recall(envID, it.AsWritten, digest)
		if !held {
			res := d.lookupPlatforms(ctx, envID, it.AsWritten, pin.Ref, read)
			switch {
			case res.infra != "":
				return res.infra, it.Name, ""
			case res.failure != "":
				return res.failure, it.Name, imageProblem(res, it).Message
			}
			platforms = res.platforms
			d.Platforms.remember(envID, it.AsWritten, digest, platforms)
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
