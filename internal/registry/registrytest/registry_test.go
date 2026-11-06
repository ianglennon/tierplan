// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registrytest_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/registry/registrytest"
)

func clientOf(r *registrytest.Registry, user, pass string) *registry.Client {
	return registry.NewClient(registry.HostOptions{Host: r.Host(), Username: user, Password: pass}, r.EgressPolicy(), "test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func ctxOf(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func TestHeadAndGetAreCountedApart(t *testing.T) {
	t.Parallel()
	r := registrytest.New(t, registrytest.Options{})
	amd := registrytest.Image("amd")
	arm := registrytest.Image("arm")
	idx := registrytest.Index(
		registrytest.PlatformManifest{OS: "linux", Arch: "amd64", Image: amd},
		registrytest.PlatformManifest{OS: "linux", Arch: "arm64", Image: arm},
	)
	r.SetTag("acme/app", "1", idx)
	c := clientOf(r, "", "")

	got, err := c.ManifestDigest(ctxOf(t), "acme/app", "1")
	if err != nil || got != idx.Digest() {
		t.Fatalf("ManifestDigest = %q, %v; want the index digest %q", got, err, idx.Digest())
	}
	if r.Heads() != 1 || r.Gets() != 0 {
		t.Errorf("after a digest read: %d HEAD and %d GET, want 1 and 0", r.Heads(), r.Gets())
	}
	index, ps, err := c.IndexPlatforms(ctxOf(t), "acme/app", idx.Digest())
	if err != nil || !index || len(ps) != 2 {
		t.Fatalf("IndexPlatforms = %v, %d platforms, %v; want an index of 2", index, len(ps), err)
	}
	if r.Heads() != 1 || r.Gets() != 1 {
		t.Errorf("after an index read: %d HEAD and %d GET, want 1 and 1", r.Heads(), r.Gets())
	}

	// A moved tag answers the new digest; the image it listed is still served by digest.
	moved := registrytest.Image("moved")
	r.SetTag("acme/app", "1", moved)
	if got, err := c.ManifestDigest(ctxOf(t), "acme/app", "1"); err != nil || got != moved.Digest() {
		t.Errorf("after the tag moved: %q, %v; want %q", got, err, moved.Digest())
	}
	if index, _, err := c.IndexPlatforms(ctxOf(t), "acme/app", moved.Digest()); err != nil || index {
		t.Errorf("one image read as index=%v, %v; want a single image", index, err)
	}
}

func TestUnknownTagIsNotFound(t *testing.T) {
	t.Parallel()
	r := registrytest.New(t, registrytest.Options{})
	_, err := clientOf(r, "", "").ManifestDigest(ctxOf(t), "acme/none", "1")
	if got := registry.Classify(err); got != registry.CodeRepositoryNotFound {
		t.Errorf("an unknown tag classified %q (%v), want repository_not_found", got, err)
	}
}

func TestSignInIsAskedFor(t *testing.T) {
	t.Parallel()
	r := registrytest.New(t, registrytest.Options{User: "bot", Password: "s3cret-token-value"})
	r.SetTag("acme/app", "1", registrytest.Image("a"))

	if _, err := clientOf(r, "bot", "wrong").ManifestDigest(ctxOf(t), "acme/app", "1"); registry.Classify(err) != registry.CodeSignInRefused {
		t.Errorf("a wrong sign-in classified %q (%v), want sign_in_refused", registry.Classify(err), err)
	}
	if _, err := clientOf(r, "bot", "s3cret-token-value").ManifestDigest(ctxOf(t), "acme/app", "1"); err != nil {
		t.Errorf("the right sign-in: %v", err)
	}
}

func TestRateLimitSwitch(t *testing.T) {
	t.Parallel()
	r := registrytest.New(t, registrytest.Options{})
	r.SetTag("acme/app", "1", registrytest.Image("a"))
	c := clientOf(r, "", "")
	if _, err := c.ManifestDigest(ctxOf(t), "acme/app", "1"); err != nil {
		t.Fatalf("before the limit: %v", err)
	}
	r.SetRateLimitAfter(0)
	_, err := c.ManifestDigest(ctxOf(t), "acme/app", "1")
	if got := registry.Classify(err); got != registry.CodeRateLimited {
		t.Errorf("after the limit: classified %q (%v), want rate_limited", got, err)
	}
	r.SetRateLimitAfter(-1)
	if _, err := c.ManifestDigest(ctxOf(t), "acme/app", "1"); err != nil {
		t.Errorf("after the limit was lifted: %v", err)
	}
}

func TestMissingDigestHeaderFallsBackToAGet(t *testing.T) {
	t.Parallel()
	r := registrytest.New(t, registrytest.Options{NoDigestHeader: true})
	m := registrytest.Image("a")
	r.SetTag("acme/app", "1", m)
	got, err := clientOf(r, "", "").ManifestDigest(ctxOf(t), "acme/app", "1")
	if err != nil || got != m.Digest() {
		t.Fatalf("ManifestDigest = %q, %v; want %q", got, err, m.Digest())
	}
	if r.Heads() != 1 || r.Gets() != 1 {
		t.Errorf("a HEAD with no digest header cost %d HEAD and %d GET, want 1 and 1 (the client reads the body)", r.Heads(), r.Gets())
	}
}
