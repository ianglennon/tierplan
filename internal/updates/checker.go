// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ianglennon/tierplan/internal/registry"
)

// Heads is what the Checker asks a registry. It is a port so a test answers for the registry and the
// per-platform judgement and the fleet dedupe can be added behind it.
type Heads interface {
	// Digest returns the digest ref's tag points at, read with a manifest HEAD under the credential the
	// environment resolves for it, and the ID of that credential ("" for an anonymous call). The
	// credential ID is returned with an error too whenever it is known, so a failure is stored under the
	// credential that met it.
	Digest(ctx context.Context, envID string, ref registry.Ref) (digest, credID string, err error)
	// Index reads what a digest names with a GET by digest, which a registry may count as a pull:
	// whether it is a multi-platform index and the manifest digest of each platform it lists (platform
	// key linux/amd64 to digest). The Checker asks only when a tag's digest changed.
	Index(ctx context.Context, envID string, ref registry.Ref, digest string) (index bool, platforms map[string]string, err error)
}

// registryHeads is Heads over the registry client, dialing through the same egress policy as every other
// registry call (T-12-02).
type registryHeads struct {
	Resolver *registry.Resolver
	Clients  *registry.ClientCache
}

// NewHeads returns the Heads that asks real registries with the stored credentials of resolver.
func NewHeads(resolver *registry.Resolver, clients *registry.ClientCache) Heads {
	return registryHeads{Resolver: resolver, Clients: clients}
}

// client chooses the registry client for ref under the credential the environment resolves for it, and
// that credential's ID ("" for an anonymous call). The ID is returned with an error whenever it is known.
func (h registryHeads) client(ctx context.Context, envID string, ref registry.Ref) (*registry.Client, string, error) {
	cred, err := h.Resolver.ForBrowse(ctx, envID, nil, ref)
	if err != nil {
		id, _ := h.Resolver.CredentialFor(ctx, envID, ref) // the identity alone; opens no secret
		return nil, id, err
	}
	if cred == nil {
		return h.Clients.Anonymous(ref.Domain), "", nil
	}
	return h.Clients.ForCredential(cred.ID, cred.UpdatedAt, cred.HostOptions), cred.ID, nil
}

// Digest implements Heads. A tag that is not a tag as the reference grammar writes one is refused as
// not found before any request: a name that is not must never be put in a registry URL.
func (h registryHeads) Digest(ctx context.Context, envID string, ref registry.Ref) (string, string, error) {
	if !registry.ValidTag(ref.Tag) {
		return "", "", errNoSuchTag
	}
	c, credID, err := h.client(ctx, envID, ref)
	if err != nil {
		return "", credID, err
	}
	d, err := c.ManifestDigest(ctx, ref.Path, ref.Tag)
	return d, credID, err
}

// Index implements Heads: one read of the manifest the digest names, by digest. It is the only request
// of the check that is a pull, so the Checker makes it only when the tag's digest changed.
func (h registryHeads) Index(ctx context.Context, envID string, ref registry.Ref, digest string) (bool, map[string]string, error) {
	c, _, err := h.client(ctx, envID, ref)
	if err != nil {
		return false, nil, err
	}
	index, ps, err := c.IndexPlatforms(ctx, ref.Path, digest)
	if err != nil || !index {
		return false, nil, err
	}
	platforms := make(map[string]string, len(ps))
	for _, p := range ps {
		platforms[registry.PlatformKey(p)] = p.Digest
	}
	return true, platforms, nil
}

// errNoSuchTag is the error of a tag no request was made for. It is stored as not found.
var errNoSuchTag = errors.New("updates: not a tag")

// reasonFor words an error of a registry call as the Reason a row stores: from the error's type, never
// from the registry's text.
func reasonFor(err error) Reason {
	switch {
	case errors.Is(err, errNoSuchTag):
		return ReasonNotFound
	case errors.Is(err, registry.ErrNoDigest):
		return ReasonUnsupported
	}
	return ReasonOf(registry.Classify(err))
}

// Checker asks the registries about the images of an environment and keeps what they answer.
type Checker struct {
	Heads Heads
	Store Store
	// Runs, when set, is given the platforms of an index the check is about to replace, which is the
	// index the workloads that have not been updated yet still run.
	Runs *RunIndex
	Now  func() time.Time
	Log  *slog.Logger
}

// CheckResult counts what one check of an environment found.
type CheckResult struct {
	// Checked is how many distinct tags were asked about, Updates how many workloads run another
	// digest than the one their tag points at now, Failed how many tags could not be read.
	Checked, Updates, Failed int
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// CheckEnv asks the registries for the digest of the tag of each workload, once for each distinct
// registry, repository and tag, and stores what each answered. A tag that cannot be read is stored as a
// failure with its Reason and keeps the earlier answer, so the page never reads a failed check as up to
// date. A cancelled context stops the check and stores nothing for the tag it cut short.
func (c *Checker) CheckEnv(ctx context.Context, envID string, ws []Workload) (CheckResult, error) {
	var res CheckResult
	type tagKey struct{ host, repo, tag string }
	answered := map[tagKey]*Check{}
	for _, w := range ws {
		if !w.Checkable() {
			continue
		}
		tk := tagKey{w.Ref.Domain, w.Ref.Path, w.Ref.Tag}
		chk, seen := answered[tk]
		if !seen {
			res.Checked++
			var err error
			if chk, err = c.checkOne(ctx, envID, w.Ref); err != nil {
				return res, err
			}
			answered[tk] = chk
			if chk.Error != "" {
				res.Failed++
			}
		}
		if chk.Error == "" && chk.Digest != "" && w.RunningDigest != "" && chk.Digest != w.RunningDigest {
			res.Updates++
		}
	}
	return res, nil
}

// checkOne asks about one tag and stores the answer. The returned check is the one stored.
func (c *Checker) checkOne(ctx context.Context, envID string, ref registry.Ref) (*Check, error) {
	at := c.now()
	key := Key{Host: ref.Domain, Repo: ref.Path, Tag: ref.Tag}
	var (
		digest string
		err    error
	)
	if !registry.ValidTag(ref.Tag) {
		err = errNoSuchTag
	} else {
		digest, key.CredID, err = c.Heads.Digest(ctx, envID, ref)
	}
	if err == nil && digest == "" {
		err = registry.ErrNoDigest
	}
	var prev *Check
	var index bool
	var platforms map[string]string
	if err == nil {
		if prev, err = c.stored(ctx, key); err != nil {
			return nil, err
		}
		index, platforms, err = c.platformsOf(ctx, envID, ref, digest, prev)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason := reasonFor(err)
		c.log().Info("an image check failed", "host", key.Host, "repo", key.Repo, "tag", key.Tag, "reason", string(reason))
		if serr := c.Store.SaveFailure(ctx, key, at, reason); serr != nil {
			return nil, fmt.Errorf("store the failed check: %w", serr)
		}
		return &Check{Key: key, CheckedAt: at, Error: reason}, nil
	}
	chk := Check{Key: key, Digest: digest, Index: index, Platforms: platforms, CheckedAt: at, SuccessAt: at}
	if serr := c.Store.SaveSuccess(ctx, chk); serr != nil {
		return nil, fmt.Errorf("store the check: %w", serr)
	}
	if prev != nil && prev.Index && prev.Digest != digest {
		// The index the tag leaves is the one workloads that are not updated yet still run.
		c.Runs.Put(ref, prev.Digest, prev.Platforms)
	}
	return &chk, nil
}

// stored returns the check stored for key, nil when there is none.
func (c *Checker) stored(ctx context.Context, key Key) (*Check, error) {
	got, err := c.Store.Checks(ctx, []Key{key})
	if err != nil {
		return nil, fmt.Errorf("read the stored check: %w", err)
	}
	if p, ok := got[key]; ok {
		return &p, nil
	}
	return nil, nil //nolint:nilnil // no stored check is an answer
}

// platformsOf says what digest names. The HEAD that found it costs no pull, so the read of the index
// (one GET by digest) is made only when the digest is not the one already stored, or the stored index
// has no platforms; otherwise the stored answer stands (D-02, RESEARCH Pitfall 2). An index that lists
// no runnable platform is read as one image: there is nothing to compare platform by platform.
func (c *Checker) platformsOf(ctx context.Context, envID string, ref registry.Ref, digest string, prev *Check) (bool, map[string]string, error) {
	if prev != nil && prev.Digest == digest && !prev.SuccessAt.IsZero() && (!prev.Index || len(prev.Platforms) > 0) {
		return prev.Index, prev.Platforms, nil
	}
	index, platforms, err := c.Heads.Index(ctx, envID, ref, digest)
	if err != nil {
		return false, nil, err
	}
	if !index || len(platforms) == 0 {
		return false, nil, nil
	}
	return true, platforms, nil
}
