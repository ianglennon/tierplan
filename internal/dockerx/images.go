// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Codes PullFailure answers. A code is a stable word the API maps to its own copy; the
// engine's own text is never sent on.
const (
	PullCodeDenied              = "pull_denied"
	PullCodeUnauthorized        = "pull_unauthorized"
	PullCodeNotFound            = "image_not_found"
	PullCodeInvalidReference    = "invalid_reference"
	PullCodeRateLimited         = "rate_limited"
	PullCodeRegistryUnreachable = "registry_unreachable"
	PullCodeRegistryTLS         = "registry_tls"
	PullCodeRegistryHTTP        = "registry_http"
	// PullCodeRegistryHTTPFallback is the engine's answer "Client sent an HTTP request to an
	// HTTPS server". The engine retried over plain HTTP, because the registry is in
	// insecure-registries or its name resolves to a loopback address, and the registry spoke TLS.
	// The plain-HTTP answer hides the HTTPS one, so a missing tag, a refused sign-in and a missing
	// repository all read the same; the real cause is not known.
	PullCodeRegistryHTTPFallback = "registry_http_fallback"
	PullCodeCancelled            = "cancelled"
	PullCodeFailed               = "pull_failed"
)

// PullOptions are what a pull is sent with.
type PullOptions struct {
	// EncodedAuth is the base64url X-Registry-Auth value for the registry the image comes
	// from; empty pulls anonymously. It is a credential: it is sent to the engine and
	// goes nowhere else, and no error built here includes it.
	EncodedAuth string
	// Platform is os/arch[/variant]; empty takes the engine's own.
	Platform string
}

// PullEvent is one line of the engine's progress for a pull.
type PullEvent struct {
	// Layer is the layer the line is about, empty for a line about the image as a whole.
	Layer  string
	Status string
	// Current and Total count Bytes of the layer when Bytes is true. Other units (the
	// seconds an extraction reports) are not carried: they are not a size.
	Current, Total int64
	Bytes          bool
}

// PullResult is how a pull ended.
type PullResult struct {
	// Digest is the digest the reference resolved to, empty when the engine did not say.
	Digest string
	// UpToDate is true when the engine already had that image.
	UpToDate bool
}

// pullError carries the engine's answer to a pull. Its text names the reference and the
// node as every error of this package does, and PullFailure classifies the answer alone,
// so a reference that happens to contain a word like "denied" is not mistaken for one.
type pullError struct {
	ref, where string
	err        error
}

func (e *pullError) Error() string { return fmt.Sprintf("pull %s on %s: %v", e.ref, e.where, e.err) }
func (e *pullError) Unwrap() error { return e.err }

// ImagePull pulls ref on this node and reports the engine's progress to on (which may be
// nil) as it streams. It returns when the engine has finished, an error ends the stream,
// or ctx ends. It never retries: a pull that failed is retried by the person who started
// it. ref is a full reference as registry.ParseRef writes it.
func (c *Client) ImagePull(ctx context.Context, ref string, o PullOptions, on func(PullEvent)) (PullResult, error) {
	opts := client.ImagePullOptions{RegistryAuth: o.EncodedAuth}
	if o.Platform != "" {
		p, err := parsePlatform(o.Platform)
		if err != nil {
			return PullResult{}, &pullError{ref: ref, where: c.t.Describe(), err: cerrdefs.ErrInvalidArgument.WithMessage(err.Error())}
		}
		opts.Platforms = []ocispec.Platform{p}
	}
	resp, err := c.cur().ImagePull(ctx, ref, opts)
	if err != nil {
		return PullResult{}, &pullError{ref: ref, where: c.t.Describe(), err: err}
	}
	var res PullResult
	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return res, &pullError{ref: ref, where: c.t.Describe(), err: err}
		}
		if msg.Error != nil {
			return res, &pullError{ref: ref, where: c.t.Describe(), err: errors.New(msg.Error.Message)}
		}
		if d, ok := strings.CutPrefix(msg.Status, "Digest: "); ok {
			res.Digest = strings.TrimSpace(d)
			continue
		}
		if strings.Contains(msg.Status, "Image is up to date") {
			res.UpToDate = true
			continue
		}
		if strings.HasPrefix(msg.Status, "Status: ") || on == nil {
			continue
		}
		ev := PullEvent{Layer: msg.ID, Status: msg.Status}
		if strings.HasPrefix(msg.Status, "Pulling from ") {
			// The line about the image: its ID is the tag.
			ev.Layer = ""
		}
		if p := msg.Progress; p != nil && !p.HideCounts && (p.Units == "" || p.Units == "bytes") && (p.Total > 0 || p.Current > 0) {
			ev.Current, ev.Total, ev.Bytes = p.Current, p.Total, true
		}
		on(ev)
	}
	if err := ctx.Err(); err != nil {
		return res, &pullError{ref: ref, where: c.t.Describe(), err: err}
	}
	return res, nil
}

// DistributionResult is what the engine learned from a registry about a reference.
type DistributionResult struct {
	// Digest is the digest of the manifest, or of the index of a multi-platform image.
	Digest string
	// Platforms are os/arch[/variant] for each platform the image is built for. The
	// unknown/unknown entries BuildKit attestations appear as are dropped.
	Platforms []string
}

// DistributionInspect asks the engine to read the manifest of ref from its registry, with
// the credential in encodedAuth (empty is anonymous), and so says whether this node can
// pull it. The engine answers with a HEAD of the manifest, which Docker Hub does not count
// as a pull. Like ImagePull it never retries, and an error is classified by PullFailure.
// encodedAuth is a credential: it goes to the engine and nowhere else.
func (c *Client) DistributionInspect(ctx context.Context, ref, encodedAuth string) (DistributionResult, error) {
	res, err := c.cur().DistributionInspect(ctx, ref, client.DistributionInspectOptions{EncodedRegistryAuth: encodedAuth})
	if err != nil {
		return DistributionResult{}, &pullError{ref: ref, where: c.t.Describe(), err: err}
	}
	out := DistributionResult{Digest: res.Descriptor.Digest.String()}
	for _, p := range res.Platforms {
		if p.OS == "" || p.Architecture == "" || p.OS == "unknown" || p.Architecture == "unknown" {
			continue
		}
		s := p.OS + "/" + p.Architecture
		if p.Variant != "" {
			s += "/" + p.Variant
		}
		if !slices.Contains(out.Platforms, s) {
			out.Platforms = append(out.Platforms, s)
		}
	}
	return out, nil
}

// parsePlatform reads os/arch[/variant].
func parsePlatform(s string) (ocispec.Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return ocispec.Platform{}, fmt.Errorf("platform %q is not os/arch[/variant]", s)
	}
	p := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

// PullFailure classifies the error of ImagePull as one of the PullCode words: by the
// status the engine answered first, then by what its message says. Anything else is
// PullCodeFailed.
func PullFailure(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return PullCodeCancelled
	case cerrdefs.IsUnauthorized(err):
		return PullCodeUnauthorized
	case cerrdefs.IsPermissionDenied(err):
		return PullCodeDenied
	case cerrdefs.IsNotFound(err):
		return PullCodeNotFound
	case cerrdefs.IsInvalidArgument(err):
		return PullCodeInvalidReference
	}
	text := err.Error()
	var pe *pullError
	if errors.As(err, &pe) {
		text = pe.err.Error()
	}
	text = strings.ToLower(text)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(text, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("denied"):
		return PullCodeDenied
	case has("unauthorized", "authentication required"):
		return PullCodeUnauthorized
	case has("toomanyrequests", "too many requests"):
		return PullCodeRateLimited
	case has("no such host", "connection refused", "i/o timeout"):
		return PullCodeRegistryUnreachable
	case has("x509:", "certificate"):
		return PullCodeRegistryTLS
	case has("server gave http response to https client"):
		return PullCodeRegistryHTTP
	case has("client sent an http request to an https server"):
		return PullCodeRegistryHTTPFallback
	}
	return PullCodeFailed
}

// ImageDetail is one image as its page shows it. The labels and the names are what the
// engine reports; the image's configuration, which can hold secrets, is only in the raw
// document ImageDetail returns beside it, and a caller sends that to a browser only
// through the redaction package.
type ImageDetail struct {
	// ID is the image ID, sha256: and 64 hexadecimal characters.
	ID string
	// RepoTags are the names the image has on this node, RepoDigests the repo@sha256 names.
	// Both are never nil.
	RepoTags, RepoDigests []string
	Size                  int64
	// Created is zero when the engine does not say.
	Created                   time.Time
	OS, Architecture, Variant string
	// Labels are never nil.
	Labels map[string]string
}

// ImageDetail reads one image by ID with the engine's own raw inspect document, which is
// the Inspect tab's. TierPlan asks for no manifests and no platform: the image the engine
// holds for the ID is the one answered. An image the engine does not have is an error
// IsNotFound reports.
func (c *Client) ImageDetail(ctx context.Context, id string) (ImageDetail, []byte, error) {
	var raw bytes.Buffer
	res, err := c.cur().ImageInspect(ctx, id, client.ImageInspectWithRawResponse(&raw))
	if err != nil {
		return ImageDetail{}, nil, fmt.Errorf("inspect image on %s: %w", c.t.Describe(), err)
	}
	d := ImageDetail{
		ID: res.ID, RepoTags: slices.Clone(res.RepoTags), RepoDigests: slices.Clone(res.RepoDigests),
		Size: res.Size, OS: res.Os, Architecture: res.Architecture, Variant: res.Variant,
		Labels: map[string]string{},
	}
	if d.RepoTags == nil {
		d.RepoTags = []string{}
	}
	if d.RepoDigests == nil {
		d.RepoDigests = []string{}
	}
	if res.Config != nil {
		maps.Copy(d.Labels, res.Config.Labels)
	}
	if t, err := time.Parse(time.RFC3339Nano, res.Created); err == nil && t.Year() > 1 {
		d.Created = t
	}
	return d, raw.Bytes(), nil
}

// HistoryRow is one layer of an image's history, as the engine reports it. CreatedBy is the
// command that made the layer, which a build can have a secret in; it is shown as the
// engine says it.
type HistoryRow struct {
	// ID is the layer's image ID, or "<missing>" for a layer of a pulled image; empty when
	// the engine gives none.
	ID, CreatedBy, Comment string
	Size                   int64
	// Created is zero when the engine does not say.
	Created time.Time
}

// ImageHistory reads the layers of one image by ID, in the order the engine answers: the
// newest layer first. An image the engine does not have is an error IsNotFound reports.
func (c *Client) ImageHistory(ctx context.Context, id string) ([]HistoryRow, error) {
	res, err := c.cur().ImageHistory(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("image history on %s: %w", c.t.Describe(), err)
	}
	out := make([]HistoryRow, 0, len(res.Items))
	for _, h := range res.Items {
		row := HistoryRow{ID: h.ID, CreatedBy: h.CreatedBy, Comment: h.Comment, Size: h.Size}
		if h.Created > 0 {
			row.Created = time.Unix(h.Created, 0).UTC()
		}
		out = append(out, row)
	}
	return out, nil
}

// ImageUntag removes one name (repository:tag or repository@digest) of an image on this
// node. The engine keeps the image while another name of it remains, and deletes it when
// that was the last one, so removed says which happened. It never sends force: an image
// that a container still uses is refused with a conflict IsConflict reports, and a name
// the node does not have is an error IsNotFound reports.
func (c *Client) ImageUntag(ctx context.Context, name string) (removed bool, err error) {
	res, err := c.cur().ImageRemove(ctx, name, client.ImageRemoveOptions{PruneChildren: true})
	if err != nil {
		return false, fmt.Errorf("untag image on %s: %w", c.t.Describe(), err)
	}
	for _, it := range res.Items {
		if it.Deleted != "" {
			removed = true
		}
	}
	return removed, nil
}

// ImageRemove removes the image id and every one of its names on this node, without force.
// An engine refuses to delete by ID an image that has several names (it "must be forced"),
// so each name in names is untagged first and the image is deleted by ID only if it is
// still there afterwards. A name the engine no longer has is skipped: someone else removed
// it. An image that a container uses is refused with a conflict IsConflict reports, and an
// image that is not there at all is an error IsNotFound reports.
func (c *Client) ImageRemove(ctx context.Context, id string, names []string) error {
	// An image a container uses is refused before any name is taken off it, so a refusal
	// never leaves it half untagged.
	users, err := c.ImageUsers(ctx, id)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("remove image on %s: %w", c.t.Describe(),
			cerrdefs.ErrConflict.WithMessage("image is being used by container "+users[0]))
	}
	untagged := false
	for _, name := range names {
		_, err := c.ImageUntag(ctx, name)
		switch {
		case err == nil:
			untagged = true
		case !IsNotFound(err):
			return err
		}
	}
	if _, err := c.cur().ImageInspect(ctx, id); err != nil {
		if IsNotFound(err) && untagged {
			// The untag of its last name deleted it.
			return nil
		}
		return fmt.Errorf("inspect image on %s: %w", c.t.Describe(), err)
	}
	if _, err := c.cur().ImageRemove(ctx, id, client.ImageRemoveOptions{PruneChildren: true}); err != nil {
		return fmt.Errorf("remove image on %s: %w", c.t.Describe(), err)
	}
	return nil
}

// ImageUsers names the containers on this node, running or not, that were created from the
// image id: the engine's own list of every container, read now.
func (c *Client) ImageUsers(ctx context.Context, id string) ([]string, error) {
	cs, err := c.cur().ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers on %s: %w", c.t.Describe(), err)
	}
	users := []string{}
	for _, row := range containerRows(cs.Items) {
		if row.ImageID == id {
			users = append(users, row.Name)
		}
	}
	return users, nil
}
