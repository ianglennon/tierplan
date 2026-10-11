// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
)

var _ = registerFakeHandler((*Fake).serveImageCreate)

// serveImageCreate answers POST /images/create, the pull. It records the write and the
// X-Registry-Auth header, then answers as the configuration says: PolicyDeny and
// DropWrites as for every write, PullStatus before any stream, otherwise the stream a real
// engine sends for a known reference: "Pulling from", per layer "Pulling fs layer",
// "Downloading" with bytes, "Download complete", "Extracting" in seconds, "Pull complete",
// then "Digest:" and a "Status:" line. A reference already in FakeConfig.Images streams
// only "Image is up to date". The image joins the list the daemon answers once pulled.
func (f *Fake) serveImageCreate(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodPost || p != "/images/create" {
		return false
	}
	f.recordWrite(r)
	f.mu.Lock()
	f.pullAuth = append(f.pullAuth, r.Header.Get("X-Registry-Auth"))
	f.mu.Unlock()
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.PullStatus.Code != 0 {
		writeJSON(w, cfg.PullStatus.Code, map[string]string{"message": cfg.PullStatus.Message})
		return true
	}
	q := r.URL.Query()
	from, tag := q.Get("fromImage"), q.Get("tag")
	name := normaliseImage(from)
	sep := ":"
	if strings.HasPrefix(tag, "sha256:") {
		sep = "@"
	}
	full := name + sep + tag
	digest := fakeDigest(from + sep + tag)
	present := slices.ContainsFunc(cfg.Images, func(fi FakeImage) bool {
		return slices.ContainsFunc(fi.RepoTags, func(rt string) bool { return normaliseImage(rt) == normaliseImage(full) })
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	flush := func() {
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
	send := func(m map[string]any) { _ = enc.Encode(m); flush() }
	send(map[string]any{"status": "Pulling from " + strings.TrimPrefix(from, "docker.io/"), "id": tag})
	if present {
		send(map[string]any{"status": "Digest: " + digest})
		send(map[string]any{"status": "Status: Image is up to date for " + full})
		return true
	}
	layers := []string{"4b3ffd8ccb52", "9f5b1d72a8c1"}
	for _, l := range layers {
		send(map[string]any{"status": "Pulling fs layer", "id": l, "progressDetail": map[string]any{}})
	}
	for _, l := range layers {
		send(map[string]any{"status": "Downloading", "id": l, "progressDetail": map[string]any{"current": 512, "total": 1024}, "progress": "[=====>]"})
		send(map[string]any{"status": "Downloading", "id": l, "progressDetail": map[string]any{"current": 1024, "total": 1024}, "progress": "[=========>]"})
		send(map[string]any{"status": "Download complete", "id": l, "progressDetail": map[string]any{"hidecounts": true}})
		send(map[string]any{"status": "Extracting", "id": l, "progressDetail": map[string]any{"current": 1, "total": 1, "units": "s"}})
		send(map[string]any{"status": "Pull complete", "id": l, "progressDetail": map[string]any{}})
	}
	if cfg.PullStreamError != "" {
		send(map[string]any{"errorDetail": map[string]any{"message": cfg.PullStreamError}, "error": cfg.PullStreamError})
		return true
	}
	send(map[string]any{"status": "Digest: " + digest})
	send(map[string]any{"status": "Status: Downloaded newer image for " + full})
	f.mu.Lock()
	f.pulled = append(f.pulled, FakeImage{
		ID: fakeDigest("image:" + full), RepoTags: []string{full},
		Size: 2048, Created: time.Now().Unix(),
	})
	f.mu.Unlock()
	return true
}

// normaliseImage drops the docker.io/ and library/ a reference is written with, so
// "docker.io/library/busybox:1.36.1" and "busybox:1.36.1" compare equal.
func normaliseImage(s string) string {
	s = strings.TrimPrefix(s, "docker.io/")
	return strings.TrimPrefix(s, "library/")
}

// fakeDigest is a digest that is the same for the same reference, so a test can compare.
func fakeDigest(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FakeDigestOf is the digest the fake answers a pull of the full reference with, for
// example "docker.io/library/busybox:1.36.1".
func FakeDigestOf(fullRef string) string { return fakeDigest(fullRef) }

// PullAuth returns the X-Registry-Auth header of every pull the daemon served, in order;
// an anonymous pull sends none and is recorded as the empty string.
func (f *Fake) PullAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pullAuth)
}

// withImages returns c with the images pulled through the daemon listed after its own.
func (c FakeConfig) withImages(pulled []FakeImage) FakeConfig {
	if len(pulled) == 0 {
		return c
	}
	c.Images = append(slices.Clone(c.Images), pulled...)
	return c
}
