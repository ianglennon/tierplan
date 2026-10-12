// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

const busybox = "docker.io/library/busybox:1.36.1"

func TestImagePullStreamsLayersAndAnswersTheDigest(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	var events []dockerx.PullEvent
	res, err := c.ImagePull(context.Background(), busybox, dockerx.PullOptions{}, func(e dockerx.PullEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("ImagePull: %v", err)
	}
	if res.Digest != dockertest.FakeDigestOf(busybox) || res.UpToDate {
		t.Errorf("result = %+v, want the digest and not up to date", res)
	}
	layers := map[string]bool{}
	var sawBytes, sawExtractBytes, sawHeader bool
	for _, e := range events {
		if e.Layer != "" {
			layers[e.Layer] = true
		}
		if e.Status == "Pulling from library/busybox" && e.Layer == "" {
			sawHeader = true
		}
		if e.Status == "Downloading" && e.Bytes && e.Total == 1024 {
			sawBytes = true
		}
		if e.Status == "Extracting" && e.Bytes {
			sawExtractBytes = true
		}
		if strings.HasPrefix(e.Status, "Digest") || strings.HasPrefix(e.Status, "Status") {
			t.Errorf("a summary line was reported as progress: %+v", e)
		}
	}
	if len(layers) != 2 || !sawBytes || !sawHeader {
		t.Errorf("layers=%v bytes=%v header=%v events=%+v", layers, sawBytes, sawHeader, events)
	}
	if sawExtractBytes {
		t.Error("an extraction counted in seconds was reported as bytes")
	}
	if got := fake.Writes(); len(got) != 1 || !strings.HasPrefix(got[0], "POST /images/create?") || !strings.Contains(got[0], "tag=1.36.1") {
		t.Errorf("writes = %v, want one create for the tag", got)
	}
}

func TestImagePullOfAPresentImageIsUpToDate(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Images: []dockertest.FakeImage{{ID: "sha256:aaaa", RepoTags: []string{"busybox:1.36.1"}}}})
	c := newClient(t, fake)
	res, err := c.ImagePull(context.Background(), busybox, dockerx.PullOptions{}, nil)
	if err != nil || !res.UpToDate || res.Digest == "" {
		t.Fatalf("ImagePull = %+v, %v, want up to date with a digest", res, err)
	}
	// A pulled image is present afterwards.
	other := "docker.io/library/alpine:3.20"
	if res, err := c.ImagePull(context.Background(), other, dockerx.PullOptions{}, nil); err != nil || res.UpToDate {
		t.Fatalf("first pull = %+v, %v", res, err)
	}
	if res, err := c.ImagePull(context.Background(), other, dockerx.PullOptions{}, nil); err != nil || !res.UpToDate {
		t.Fatalf("second pull = %+v, %v, want up to date", res, err)
	}
}

func TestImagePullSendsTheEncodedAuthAndOnlyThat(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	if _, err := c.ImagePull(context.Background(), busybox, dockerx.PullOptions{EncodedAuth: "e30="}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImagePull(context.Background(), "docker.io/library/alpine:3", dockerx.PullOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.PullAuth(); !slices.Equal(got, []string{"e30=", ""}) {
		t.Errorf("auth headers = %q", got)
	}
}

func TestImagePullFailuresAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    dockertest.FakeConfig
		want   string
		inText string
	}{
		{"403 before the stream", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 403, Message: "denied: requested access to the resource is denied"}}, dockerx.PullCodeDenied, ""},
		{"401", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 401, Message: "unauthorized: authentication required"}}, dockerx.PullCodeUnauthorized, ""},
		{"404", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 404, Message: "manifest unknown"}}, dockerx.PullCodeNotFound, ""},
		{"500 rate limit", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "toomanyrequests: You have reached your pull rate limit"}}, dockerx.PullCodeRateLimited, ""},
		{"500 no such host", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "Get \"https://reg.example/v2/\": dial tcp: lookup reg.example: no such host"}}, dockerx.PullCodeRegistryUnreachable, ""},
		{"500 tls", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "x509: certificate signed by unknown authority"}}, dockerx.PullCodeRegistryTLS, ""},
		{"500 http", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "http: server gave HTTP response to HTTPS client"}}, dockerx.PullCodeRegistryHTTP, ""},
		{"500 other", dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "boom"}}, dockerx.PullCodeFailed, ""},
		{"in-stream error", dockertest.FakeConfig{PullStreamError: "layer exploded"}, dockerx.PullCodeFailed, "layer exploded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, dockertest.NewFake(t, tc.cfg))
			_, err := c.ImagePull(context.Background(), busybox, dockerx.PullOptions{}, nil)
			if err == nil {
				t.Fatal("ImagePull succeeded")
			}
			if got := dockerx.PullFailure(err); got != tc.want {
				t.Errorf("PullFailure(%v) = %q, want %q", err, got, tc.want)
			}
			if tc.inText != "" && !strings.Contains(err.Error(), tc.inText) {
				t.Errorf("error %q lacks the engine message %q", err, tc.inText)
			}
		})
	}
}

func TestPullFailureDoesNotReadTheReferenceAsAnAnswer(t *testing.T) {
	c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "boom"}}))
	_, err := c.ImagePull(context.Background(), "docker.io/denied/app:1", dockerx.PullOptions{}, nil)
	if got := dockerx.PullFailure(err); got != dockerx.PullCodeFailed {
		t.Errorf("PullFailure = %q, want pull_failed: the word is in the reference, not the answer", got)
	}
	if got := dockerx.PullFailure(context.Canceled); got != dockerx.PullCodeCancelled {
		t.Errorf("PullFailure(Canceled) = %q", got)
	}
	if got := dockerx.PullFailure(errors.New("plain")); got != dockerx.PullCodeFailed {
		t.Errorf("PullFailure(plain) = %q", got)
	}
}

func TestImagePullErrorNamesTheRefAndNodeButNeverTheAuth(t *testing.T) {
	c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{PullStatus: dockertest.FakePullStatus{Code: 500, Message: "boom"}}))
	_, err := c.ImagePull(context.Background(), busybox, dockerx.PullOptions{EncodedAuth: "c2VjcmV0LXRva2Vu"}, nil)
	if err == nil || !strings.Contains(err.Error(), busybox) || strings.Contains(err.Error(), "c2VjcmV0LXRva2Vu") {
		t.Errorf("error = %v", err)
	}
}

func TestDistributionInspectAnswersTheDigestAndOnlyRealPlatforms(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{})
	c := newClient(t, fake)
	const ref = "localhost:5000/private/app:1"
	res, err := c.DistributionInspect(context.Background(), ref, "e30=")
	if err != nil {
		t.Fatalf("DistributionInspect: %v", err)
	}
	if !strings.HasPrefix(res.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", res.Digest)
	}
	if want := []string{"linux/amd64", "linux/arm64/v8"}; !slices.Equal(res.Platforms, want) {
		t.Errorf("platforms = %q, want %q with no unknown/unknown", res.Platforms, want)
	}
	if got := fake.DistributionAuth(); !slices.Equal(got, []string{"e30="}) {
		t.Errorf("auth headers = %q, want the one that was sent", got)
	}
	if got := fake.DistributionRefs(); !slices.Equal(got, []string{ref}) {
		t.Errorf("refs = %q, want %q", got, ref)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("a distribution check wrote %v, want nothing", w)
	}
}

func TestDistributionInspectFailuresAreClassifiedAsAPullIs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status dockertest.FakePullStatus
		want   string
	}{
		{"403", dockertest.FakePullStatus{Code: 403, Message: "Head \"https://ghcr.io/v2/acme/app/manifests/x\": denied"}, dockerx.PullCodeDenied},
		{"401", dockertest.FakePullStatus{Code: 401, Message: "unauthorized: authentication required"}, dockerx.PullCodeUnauthorized},
		{"404", dockertest.FakePullStatus{Code: 404, Message: "manifest unknown"}, dockerx.PullCodeNotFound},
		{"500 no such host", dockertest.FakePullStatus{Code: 500, Message: "lookup reg.example: no such host"}, dockerx.PullCodeRegistryUnreachable},
		{"500 certificate", dockertest.FakePullStatus{Code: 500, Message: "x509: certificate signed by unknown authority"}, dockerx.PullCodeRegistryTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{PullStatus: tc.status}))
			_, err := c.DistributionInspect(context.Background(), "ghcr.io/acme/app:1", "c2VjcmV0LXRva2Vu")
			if err == nil {
				t.Fatal("DistributionInspect succeeded")
			}
			if got := dockerx.PullFailure(err); got != tc.want {
				t.Errorf("PullFailure(%v) = %q, want %q", err, got, tc.want)
			}
			if strings.Contains(err.Error(), "c2VjcmV0LXRva2Vu") {
				t.Errorf("error %q carries the credential", err)
			}
		})
	}
}

// One image's detail (plan 05-14).

const (
	imgA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestImageDetailReadsTheImageWithItsNamesPlatformLabelsAndTheRawDocument(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Images: []dockertest.FakeImage{
		{ID: imgA, RepoTags: []string{"nginx:1.27", "nginx:latest"}, RepoDigests: []string{"nginx@sha256:" + strings.Repeat("c", 64)},
			Size: 4096, Created: 1788000000, OS: "linux", Architecture: "arm64", Variant: "v8",
			Labels: map[string]string{"maintainer": "x"}, Env: []string{"A=1"}},
		{ID: imgB},
	}})
	c := newClient(t, fake)
	d, raw, err := c.ImageDetail(context.Background(), imgA)
	if err != nil {
		t.Fatalf("ImageDetail: %v", err)
	}
	if d.ID != imgA || !slices.Equal(d.RepoTags, []string{"nginx:1.27", "nginx:latest"}) ||
		!slices.Equal(d.RepoDigests, []string{"nginx@sha256:" + strings.Repeat("c", 64)}) ||
		d.Size != 4096 || d.Created.Unix() != 1788000000 || d.OS != "linux" || d.Architecture != "arm64" || d.Variant != "v8" ||
		d.Labels["maintainer"] != "x" {
		t.Errorf("detail = %+v", d)
	}
	if !strings.Contains(string(raw), `"Architecture"`) || !strings.Contains(string(raw), `"A=1"`) {
		t.Errorf("raw = %s, want the engine's own document", raw)
	}
	bare, _, err := c.ImageDetail(context.Background(), imgB)
	if err != nil || bare.RepoTags == nil || bare.RepoDigests == nil || bare.Labels == nil || !bare.Created.IsZero() {
		t.Errorf("an image with none: %+v, %v; want empty lists and maps (never nil) and no created time", bare, err)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("a read wrote: %v", w)
	}
}

func TestImageDetailAnImageTheEngineLacksIsNotFound(t *testing.T) {
	c := newClient(t, dockertest.NewFake(t, dockertest.FakeConfig{Images: []dockertest.FakeImage{{ID: imgA}}}))
	_, _, err := c.ImageDetail(context.Background(), imgB)
	if !dockerx.IsNotFound(err) {
		t.Fatalf("ImageDetail(unknown) = %v, want a not-found error", err)
	}
}
