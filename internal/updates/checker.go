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
	// Index reads what the tag's digest names: whether it is a multi-platform index and the manifest
	// digest of each platform it lists.
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

// Digest implements Heads. A tag that is not a tag as the reference grammar writes one is refused as
// not found before any request: a name that is not must never be put in a registry URL.
func (h registryHeads) Digest(ctx context.Context, envID string, ref registry.Ref) (string, string, error) {
	if !registry.ValidTag(ref.Tag) {
		return "", "", errNoSuchTag
	}
	cred, err := h.Resolver.ForBrowse(ctx, envID, nil, ref)
	if err != nil {
		id, _ := h.Resolver.CredentialFor(ctx, envID, ref) // the identity alone; opens no secret
		return "", id, err
	}
	var c *registry.Client
	credID := ""
	if cred == nil {
		c = h.Clients.Anonymous(ref.Domain)
	} else {
		credID = cred.ID
		c = h.Clients.ForCredential(cred.ID, cred.UpdatedAt, cred.HostOptions)
	}
	d, err := c.ManifestDigest(ctx, ref.Path, ref.Tag)
	return d, credID, err
}

// Index implements Heads. It answers the single-manifest view for now: the per-platform judgement
// replaces it.
func (h registryHeads) Index(context.Context, string, registry.Ref, string) (bool, map[string]string, error) {
	return false, nil, nil
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
	Now   func() time.Time
	Log   *slog.Logger
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
	chk := Check{Key: key, Digest: digest, CheckedAt: at, SuccessAt: at}
	if serr := c.Store.SaveSuccess(ctx, chk); serr != nil {
		return nil, fmt.Errorf("store the check: %w", serr)
	}
	return &chk, nil
}
