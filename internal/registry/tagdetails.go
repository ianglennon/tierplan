// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/regclient/regclient"
	"github.com/regclient/regclient/types/descriptor"
	"github.com/regclient/regclient/types/manifest"
	"github.com/regclient/regclient/types/ref"
)

const (
	// CodeDateUnavailable is a tag whose digest and platforms were read but whose created date
	// could not be: the config blob was missing, too big or unreadable.
	CodeDateUnavailable = "date_unavailable"
	// CodeTagNotFound is a tag the registry does not have, which happens when it was moved or
	// deleted after the list was read.
	CodeTagNotFound = "tag_not_found"

	// headTTL is how long the digest a tag pointed at is remembered.
	headTTL = 5 * time.Minute
	// failedDetailTTL is how long a detail whose date could not be read is remembered, so a
	// scroll back to the row tries again soon without hammering the registry.
	failedDetailTTL = time.Minute
	// detailCacheEntries is how many digests' platform lists and dates are kept, and how many
	// tag digests (T-05-75).
	detailCacheEntries = 5000
	// detailsParallel is how many tags of one request are worked on at once. The client allows
	// reqConcurrent requests to its registry at a time however many are waiting.
	detailsParallel = 8
	// configBytesMax caps an image config blob the server reads for a created date (T-05-75).
	configBytesMax = 4 << 20
	// attestationAnnotation marks an index entry that is a BuildKit attestation, not an image.
	attestationAnnotation = "vnd.docker.reference.type"
	attestationValue      = "attestation-manifest"
)

// Platform is one image of a multi-platform tag: where it runs and which manifest it is.
type Platform struct {
	OS, Arch, Variant string
	// Digest is the digest of this platform's manifest.
	Digest string
	// Size is the compressed size of the image's layers in bytes, or 0 when it is not known:
	// a registry other than Docker Hub only says it for the platform whose manifest was read.
	Size int64
}

// TagDetail is what the server learned about one tag.
type TagDetail struct {
	Tag string
	// Digest is the digest the tag points at: the index digest for a multi-platform tag.
	Digest string
	// Created is when the image was built, from its config; nil when it is not known.
	Created   *time.Time
	Platforms []Platform
	// Code is "" or one of CodeDateUnavailable, CodeTagNotFound and the codes of a failed
	// listing (needs_login, login_refused, registry_unreachable, registry_tls, rate_limited).
	Code string
}

type headKey struct {
	cred, host, repo, tag string
}

type detailKey struct {
	cred, host, repo, digest string
}

// detailEntry is what a digest holds. It is shared between calls and never changed.
type detailEntry struct {
	platforms []Platform
	created   *time.Time
	code      string
}

// Details reads the digest, platforms and created date of the given tags (at most as many as
// the caller bounds) of ref's repository with cred (nil is anonymous). preferArch is the CPU
// architecture the created date is read for when the tag has no linux/amd64 image (D-20).
// Docker Hub is read through its tags API, which costs no pulls; any other registry through a
// manifest HEAD and, per digest, the manifest and the config blob of one platform. The answer
// is in the order of tags; a tag that failed carries a Code. The caller must have been
// authorised, and cred must be one the caller may use, before this is called: the caches are
// keyed by credential, not by caller.
func (s *TagService) Details(ctx context.Context, cred *ResolvedCredential, r Ref, tags []string, preferArch string) []TagDetail {
	// A tag is the last segment of a registry URL, sent with the credential, so a name that is
	// not a tag (a path, a query, an escape) is never put in one: it reads as a tag the registry
	// does not have (WR-A02).
	out := make([]TagDetail, len(tags))
	var valid []string
	var at []int
	for i, tag := range tags {
		if ValidTag(tag) {
			valid, at = append(valid, tag), append(at, i)
		} else {
			out[i] = TagDetail{Tag: tag, Code: CodeTagNotFound}
		}
	}
	var got []TagDetail
	if strings.EqualFold(r.Domain, hubDomain) {
		got = s.hubDetails(ctx, cred, r, valid)
	} else {
		got = s.registryDetails(ctx, cred, r, valid, preferArch)
	}
	for j, d := range got {
		out[at[j]] = d
	}
	return out
}

// registryDetails is Details for a registry other than Docker Hub, for tags already checked.
func (s *TagService) registryDetails(ctx context.Context, cred *ResolvedCredential, r Ref, tags []string, preferArch string) []TagDetail {
	out := make([]TagDetail, len(tags))
	c := s.clientFor(cred, r.Domain)
	id := credIdentity(cred)
	var g errgroup.Group
	g.SetLimit(detailsParallel)
	for i, tag := range tags {
		g.Go(func() error {
			out[i] = s.oneDetail(ctx, c, id, cred != nil, r, tag, preferArch)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// oneDetail reads one tag, from the caches where they hold it.
func (s *TagService) oneDetail(ctx context.Context, c *Client, id string, signedIn bool, r Ref, tag, preferArch string) TagDetail {
	d := TagDetail{Tag: tag}
	hk := headKey{cred: id, host: r.Domain, repo: r.Path, tag: tag}
	digest, ok := s.heads.Get(hk)
	if !ok {
		var err error
		if digest, err = c.ManifestDigest(ctx, r.Path, tag); err != nil {
			d.Code = detailCode(err, signedIn)
			return d
		}
		s.heads.Put(hk, digest, headTTL)
	}
	d.Digest = digest
	dk := detailKey{cred: id, host: r.Domain, repo: r.Path, digest: digest}
	e, ok := s.details.Get(dk)
	if !ok {
		// Tags that share a digest are read once while the first read is under way.
		v, err, _ := s.flight.Do(fmt.Sprintf("%q", []string{dk.cred, dk.host, dk.repo, dk.digest}), func() (any, error) {
			return c.Inspect(ctx, r.Path, digest, preferArch)
		})
		if err != nil {
			d.Code = detailCode(err, signedIn)
			return d
		}
		in := v.(Inspected) //nolint:errcheck,forcetypeassert // Inspect is the only thing the group runs
		e = detailEntry{platforms: in.Platforms, created: in.Created}
		ttl := time.Duration(0)
		if in.DateFailed {
			e.code, ttl = CodeDateUnavailable, failedDetailTTL
		}
		s.details.Put(dk, e, ttl)
	}
	d.Created, d.Platforms, d.Code = e.created, e.platforms, e.code
	return d
}

// detailCode words a failed read of one tag: a tag the registry does not have is its own
// code, everything else reads as it does for a listing.
func detailCode(err error, signedIn bool) string {
	if classify(err) == CodeRepositoryNotFound {
		return CodeTagNotFound
	}
	return tagCode(err, signedIn)
}

// clientFor returns the client a call with cred makes its requests with.
func (s *TagService) clientFor(cred *ResolvedCredential, host string) *Client {
	if cred == nil {
		return s.clients.Anonymous(host)
	}
	return s.clients.ForCredential(cred.ID, cred.UpdatedAt, cred.HostOptions)
}

// choosePlatform picks the platform whose config the created date is read from (D-20): the
// linux/amd64 image, else the linux image for the CPU architecture of the environment's nodes,
// else the first. It returns -1 when there is none.
func choosePlatform(ps []Platform, preferArch string) int {
	for i, p := range ps {
		if p.OS == "linux" && p.Arch == "amd64" {
			return i
		}
	}
	if preferArch != "" {
		for i, p := range ps {
			if p.OS == "linux" && (p.Arch == preferArch || ociArchKey(p) == preferArch) {
				return i
			}
		}
	}
	if len(ps) > 0 {
		return 0
	}
	return -1
}

// ociArchKey is the architecture of a platform as the nodes spell it (dockerx.OCIArch): the
// architecture alone, except for 32-bit ARM, which is arm/v7 or arm/v6. A node's "arm/v7" never
// equals a platform's Arch "arm" (WR-A05); a platform with no variant is read as v7, as the API's
// platform comparison does.
func ociArchKey(p Platform) string {
	arch := strings.ToLower(p.Arch)
	if arch != "arm" {
		return arch
	}
	v := strings.ToLower(p.Variant)
	if v == "" {
		v = "v7"
	}
	return "arm/" + v
}

// isRealPlatform is false for the entries of an index that are not a runnable image: the
// unknown/unknown ones and BuildKit attestations (RESEARCH Pattern 6).
func isRealPlatform(os, arch string) bool {
	return os != "" && arch != "" && os != "unknown" && arch != "unknown"
}

// platformsOf lists the runnable images of an index's entries.
func platformsOf(descs []descriptor.Descriptor) []Platform {
	var out []Platform
	for _, d := range descs {
		if d.Platform == nil || !isRealPlatform(d.Platform.OS, d.Platform.Architecture) {
			continue
		}
		if d.Annotations[attestationAnnotation] == attestationValue {
			continue
		}
		out = append(out, Platform{OS: d.Platform.OS, Arch: d.Platform.Architecture, Variant: d.Platform.Variant, Digest: d.Digest.String()})
	}
	return out
}

// Inspected is what one digest holds.
type Inspected struct {
	Platforms []Platform
	Created   *time.Time
	// DateFailed is true when the platforms are known but the date could not be read.
	DateFailed bool
}

// imageConfig is the part of an image config the browser reads.
type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant"`
	Created      string `json:"created"`
}

// repoRef is a reference to a repository of this client's registry.
func (c *Client) repoRef(repo string) ref.Ref {
	return ref.Ref{Scheme: "reg", Registry: c.host, Repository: repo}
}

// ManifestDigest asks the registry for the digest a tag points at with a manifest HEAD, which
// the pull limits of Docker Hub do not count. For a multi-platform tag it is the index digest.
func (c *Client) ManifestDigest(ctx context.Context, repo, tag string) (string, error) {
	return withFallback(ctx, c, func(c *Client, ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		m, err := c.rc.ManifestHead(ctx, c.repoRef(repo).SetTag(tag), regclient.WithManifestRequireDigest())
		if err != nil {
			return "", fmt.Errorf("registry: head %s/%s:%s: %w", c.host, repo, tag, err)
		}
		d := m.GetDescriptor().Digest.String()
		if d == "" {
			return "", fmt.Errorf("registry: head %s/%s:%s: no digest in the answer", c.host, repo, tag)
		}
		return d, nil
	})
}

// Inspect reads the manifest a digest names. For an index it lists the real platforms and
// reads the created date from the config of the platform choosePlatform picks; for one image
// it is that image's platform and date. A date that cannot be read leaves the platforms and
// sets DateFailed.
func (c *Client) Inspect(ctx context.Context, repo, digest, preferArch string) (Inspected, error) {
	return withFallback(ctx, c, func(c *Client, ctx context.Context) (Inspected, error) {
		return c.inspectOnce(ctx, repo, digest, preferArch)
	})
}

func (c *Client) inspectOnce(ctx context.Context, repo, digest, preferArch string) (Inspected, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()
	r := c.repoRef(repo).SetDigest(digest)
	m, err := c.rc.ManifestGet(ctx, r)
	if err != nil {
		return Inspected{}, fmt.Errorf("registry: read the manifest %s/%s@%s: %w", c.host, repo, digest, err)
	}
	if !m.IsList() {
		return c.inspectImage(ctx, r, m, nil), nil
	}
	idx, ok := m.(manifest.Indexer)
	if !ok {
		return Inspected{}, fmt.Errorf("registry: the index %s/%s@%s cannot be listed", c.host, repo, digest)
	}
	descs, err := idx.GetManifestList()
	if err != nil {
		return Inspected{}, fmt.Errorf("registry: list the index %s/%s@%s: %w", c.host, repo, digest, err)
	}
	out := Inspected{Platforms: platformsOf(descs)}
	i := choosePlatform(out.Platforms, preferArch)
	if i < 0 {
		out.DateFailed = true
		return out, nil
	}
	pr := r.SetDigest(out.Platforms[i].Digest)
	pm, err := c.rc.ManifestGet(ctx, pr)
	if err != nil {
		out.DateFailed = true
		return out, nil //nolint:nilerr // the platforms are known; only the date is not
	}
	one := c.inspectImage(ctx, pr, pm, &out.Platforms[i])
	out.Created, out.DateFailed = one.Created, one.DateFailed
	return out, nil
}

// inspectImage reads one image manifest's config. into, when set, is the platform entry of an
// index to fill with the size; otherwise the image's own platform is made from its config.
func (c *Client) inspectImage(ctx context.Context, r ref.Ref, m manifest.Manifest, into *Platform) Inspected {
	var out Inspected
	img, ok := m.(manifest.Imager)
	if !ok {
		out.DateFailed = true
		return out
	}
	size, sizeErr := img.GetSize()
	if into != nil && sizeErr == nil {
		into.Size = size
	}
	cfgDesc, err := img.GetConfig()
	if err != nil {
		out.DateFailed = true
		return out
	}
	cfg, err := c.readConfig(ctx, r, cfgDesc)
	if err != nil {
		out.DateFailed = true
		return out
	}
	if t, err := time.Parse(time.RFC3339Nano, cfg.Created); err == nil && !t.IsZero() && t.Year() > 1 {
		out.Created = &t
	} else {
		out.DateFailed = true
	}
	if into == nil && isRealPlatform(cfg.OS, cfg.Architecture) {
		p := Platform{OS: cfg.OS, Arch: cfg.Architecture, Variant: cfg.Variant, Digest: m.GetDescriptor().Digest.String()}
		if sizeErr == nil {
			p.Size = size
		}
		out.Platforms = []Platform{p}
	}
	return out
}

// readConfig reads and decodes an image config blob, refusing one larger than configBytesMax.
func (c *Client) readConfig(ctx context.Context, r ref.Ref, d descriptor.Descriptor) (imageConfig, error) {
	if d.Size > configBytesMax {
		return imageConfig{}, errors.New("registry: the image config is too large")
	}
	b, err := c.rc.BlobGet(ctx, r, d)
	if err != nil {
		return imageConfig{}, fmt.Errorf("registry: read the image config: %w", err)
	}
	defer func() { _ = b.Close() }()
	raw, err := io.ReadAll(io.LimitReader(b, configBytesMax+1))
	if err != nil {
		return imageConfig{}, fmt.Errorf("registry: read the image config: %w", err)
	}
	if len(raw) > configBytesMax {
		return imageConfig{}, errors.New("registry: the image config is too large")
	}
	var cfg imageConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return imageConfig{}, fmt.Errorf("registry: decode the image config: %w", err)
	}
	return cfg, nil
}
