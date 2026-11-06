// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registry

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/regclient/regclient/types/manifest"
)

// digestForm is the shape of a digest as the reference grammar writes one: an algorithm, a colon and
// hex. A digest is put in a registry URL, so a value of another shape is never sent.
var digestForm = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-fA-F0-9]{32,}$`)

// PlatformKey is the name of a platform in the maps of an update check: the OS and the architecture as
// the nodes of an environment spell it, linux/amd64 or linux/arm/v7. A platform's own Variant matters
// only for 32-bit ARM, where it is read as v7 when the index names none.
func PlatformKey(p Platform) string {
	return strings.ToLower(p.OS) + "/" + ociArchKey(p)
}

// IndexPlatforms reads the manifest a digest names with one GET by digest, which is the only request
// it makes: no config blob is read (Inspect, which costs three, is for the tag browser). For an index
// it answers true with the runnable platforms the index lists, BuildKit attestations and unknown
// entries left out. For one image it answers false and no platforms.
func (c *Client) IndexPlatforms(ctx context.Context, repo, digest string) (bool, []Platform, error) {
	if !digestForm.MatchString(digest) {
		return false, nil, fmt.Errorf("registry: %q is not a digest", digest)
	}
	type answer struct {
		index bool
		ps    []Platform
	}
	a, err := withFallback(ctx, c, func(c *Client, ctx context.Context) (answer, error) {
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		m, err := c.rc.ManifestGet(ctx, c.repoRef(repo).SetDigest(digest))
		if err != nil {
			return answer{}, fmt.Errorf("registry: read the manifest %s/%s@%s: %w", c.host, repo, digest, err)
		}
		if !m.IsList() {
			return answer{}, nil
		}
		idx, ok := m.(manifest.Indexer)
		if !ok {
			return answer{}, fmt.Errorf("registry: the index %s/%s@%s cannot be listed", c.host, repo, digest)
		}
		descs, err := idx.GetManifestList()
		if err != nil {
			return answer{}, fmt.Errorf("registry: list the index %s/%s@%s: %w", c.host, repo, digest, err)
		}
		return answer{index: true, ps: platformsOf(descs)}, nil
	})
	return a.index, a.ps, err
}
