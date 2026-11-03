// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package registry

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

// What a digest names, platform by platform (plan 12-03, IMG-04, D-02).

func TestPlatformKeyIsOSAndNodeSpelledArchitecture(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p    Platform
		want string
	}{
		{Platform{OS: "linux", Arch: "amd64"}, "linux/amd64"},
		{Platform{OS: "Linux", Arch: "ARM64", Variant: "v8"}, "linux/arm64"},
		{Platform{OS: "linux", Arch: "arm", Variant: "v7"}, "linux/arm/v7"},
		{Platform{OS: "linux", Arch: "arm", Variant: "v6"}, "linux/arm/v6"},
		{Platform{OS: "linux", Arch: "arm"}, "linux/arm/v7"},
	} {
		if got := PlatformKey(tc.p); got != tc.want {
			t.Errorf("PlatformKey(%+v) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

func TestIndexPlatforms(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	index, amd, arm := multiArch(fi, "acme/app", "1.0") // amd64, arm64 and an attestation entry
	single := fi.addImage("acme/solo", "1.0", imageSpec{"linux", "amd64", "", imgT0})
	c := tagServiceFor(f).clientFor(f.cred("c1", f.pass), f.host())
	ctx := context.Background()

	isIndex, ps, err := c.IndexPlatforms(ctx, "acme/app", index)
	if err != nil {
		t.Fatalf("IndexPlatforms(index): %v", err)
	}
	if !isIndex {
		t.Error("an index answered index = false")
	}
	var keys, digests []string
	for _, p := range ps {
		keys, digests = append(keys, PlatformKey(p)), append(digests, p.Digest)
	}
	if !slices.Equal(keys, []string{"linux/amd64", "linux/arm64"}) || !slices.Equal(digests, []string{amd, arm}) {
		t.Errorf("platforms = %v %v, want amd64 and arm64 with the attestation entry dropped", keys, digests)
	}

	isIndex, ps, err = c.IndexPlatforms(ctx, "acme/solo", single)
	if err != nil {
		t.Fatalf("IndexPlatforms(single): %v", err)
	}
	if isIndex || len(ps) != 0 {
		t.Errorf("a single manifest answered index = %v, platforms = %+v, want false and none", isIndex, ps)
	}

	// Each call was one GET by digest and nothing else: no HEAD, no config blob.
	if n := fi.count("GET", "manifests"); n != 2 {
		t.Errorf("GET manifests = %d, want 2", n)
	}
	if n := fi.count("GET", "blobs") + fi.count("HEAD", "blobs") + fi.count("HEAD", "manifests"); n != 0 {
		t.Errorf("blob and HEAD requests = %d, want 0", n)
	}

	if _, _, err := c.IndexPlatforms(ctx, "acme/app", "not-a-digest/../x"); err == nil {
		t.Error("a value that is not a digest was sent to the registry")
	}
	if _, _, err := c.IndexPlatforms(ctx, "acme/app", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err == nil {
		t.Error("a digest the registry does not have answered no error")
	}
}
