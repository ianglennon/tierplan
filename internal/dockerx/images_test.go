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
