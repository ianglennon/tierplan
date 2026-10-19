// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var _ = registerFakeHandler((*Fake).serveImageCreate)
var _ = registerFakeHandler((*Fake).serveDistribution)
var _ = registerFakeHandler((*Fake).serveImageInspect)
var _ = registerFakeHandler((*Fake).serveImageHistory)

// imageInspectPath and imageHistoryPath match /images/{name}/json and /images/{name}/history.
// The list, /images/json, has no name in front of its /json and is not matched.
var (
	imageInspectPath = regexp.MustCompile(`^/images/(.+)/json$`)
	imageHistoryPath = regexp.MustCompile(`^/images/(.+)/history$`)
)

// resolveImage finds an image by its ID, by the ID without sha256:, or by one of its tags.
func (c FakeConfig) resolveImage(name string) (FakeImage, bool) {
	for _, fi := range c.Images {
		if fi.ID == name || strings.TrimPrefix(fi.ID, "sha256:") == name {
			return fi, true
		}
		for _, rt := range append(slices.Clone(fi.RepoTags), fi.RepoDigests...) {
			if normaliseImage(rt) == normaliseImage(name) || byDigest(rt) == byDigest(name) {
				return fi, true
			}
		}
	}
	return FakeImage{}, false
}

// byDigest is a reference written name:tag@digest read the way the engine reads it: the digest says
// which image, and the tag is only how the person wrote it, so it is dropped. A reference without a digest
// is returned as it is (never equal to another: it has no "@").
func byDigest(ref string) string {
	repo, digest, ok := strings.Cut(normaliseImage(ref), "@")
	if !ok {
		return ref
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo + "@" + digest
}

// serveImageInspect answers GET /images/{name}/json: the image as the daemon documents it,
// with its names, platform, labels and its config environment, or the 404 a real daemon gives.
func (f *Fake) serveImageInspect(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := imageInspectPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	fi, ok := cfg.resolveImage(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such image: " + m[1]})
		return true
	}
	os, arch := fi.OS, fi.Architecture
	if os == "" {
		os = "linux"
	}
	if arch == "" {
		arch = "amd64"
	}
	config := map[string]any{"Env": orEmpty(fi.Env), "Labels": fi.Labels}
	if len(fi.Cmd) > 0 {
		config["Cmd"] = fi.Cmd
	}
	if len(fi.Entrypoint) > 0 {
		config["Entrypoint"] = fi.Entrypoint
	}
	doc := map[string]any{
		"Id": fi.ID, "RepoTags": orEmpty(fi.RepoTags), "RepoDigests": orEmpty(fi.RepoDigests),
		"Os": os, "Architecture": arch, "Size": fi.Size,
		"Config": config,
		"RootFS": map[string]any{"Type": "layers", "Layers": []string{fakeDigest("layer:" + fi.ID)}},
	}
	if fi.Variant != "" {
		doc["Variant"] = fi.Variant
	}
	if fi.Created > 0 {
		doc["Created"] = time.Unix(fi.Created, 0).UTC().Format(time.RFC3339Nano)
	}
	if len(fi.Env) > 0 {
		doc["ContainerConfig"] = map[string]any{"Env": fi.Env}
	}
	writeJSON(w, http.StatusOK, doc)
	return true
}

// serveImageHistory answers GET /images/{name}/history. HistoryDeny answers as an agent too
// old for the route does.
func (f *Fake) serveImageHistory(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodGet {
		return false
	}
	m := imageHistoryPath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	if cfg.HistoryDeny || cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	fi, ok := cfg.resolveImage(m[1])
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such image: " + m[1]})
		return true
	}
	layers := fi.History
	if len(layers) == 0 {
		layers = []FakeLayer{{ID: fi.ID, CreatedBy: "/bin/sh -c #(nop) CMD [\"sh\"]", Size: fi.Size, Created: fi.Created}}
	}
	out := make([]map[string]any, 0, len(layers))
	for _, l := range layers {
		out = append(out, map[string]any{
			"Id": l.ID, "CreatedBy": l.CreatedBy, "Comment": l.Comment, "Size": l.Size, "Created": l.Created, "Tags": []string{},
		})
	}
	writeJSON(w, http.StatusOK, out)
	return true
}

// orEmpty is s, or an empty list for nil, so the document carries [] and not null.
func orEmpty[S ~[]E, E any](s S) S {
	if s == nil {
		return S{}
	}
	return s
}

// serveDistribution answers GET /distribution/{name}/json, the check of whether the daemon
// can read a manifest. It records the X-Registry-Auth header, then answers as the
// configuration says: PolicyDeny as an agent too old to know the route does, PullStatus as
// a registry's refusal, otherwise a descriptor whose digest is the same for the same
// reference and the platforms of a multi-platform image with the unknown/unknown entries
// BuildKit attestations appear as.
func (f *Fake) serveDistribution(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	name, ok := strings.CutPrefix(p, "/distribution/")
	if !ok || r.Method != http.MethodGet {
		return false
	}
	name, ok = strings.CutSuffix(name, "/json")
	if !ok {
		return false
	}
	f.mu.Lock()
	f.distAuth = append(f.distAuth, r.Header.Get("X-Registry-Auth"))
	f.distRefs = append(f.distRefs, name)
	f.mu.Unlock()
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	if cfg.PullStatus.Code != 0 {
		writeJSON(w, cfg.PullStatus.Code, map[string]string{"message": cfg.PullStatus.Message})
		return true
	}
	f.mu.Lock()
	failure, failing := f.distFail[name]
	dgst, moved := f.distDigest[name]
	f.mu.Unlock()
	if failing {
		writeJSON(w, failure.Code, map[string]string{"message": failure.Message})
		return true
	}
	if !moved {
		dgst = fakeDigest(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Descriptor": map[string]any{
			"mediaType": "application/vnd.oci.image.index.v1+json",
			"digest":    dgst,
			"size":      1024,
		},
		"Platforms": []map[string]any{
			{"architecture": "amd64", "os": "linux"},
			{"architecture": "unknown", "os": "unknown"},
			{"architecture": "arm64", "os": "linux", "variant": "v8"},
			{"architecture": "unknown", "os": "unknown"},
		},
	})
	return true
}

// SetDistributionDigest makes the distribution route answer digest for the full reference
// (for example "docker.io/library/nginx:1.27") from now on, as a registry does when a tag is
// pushed again.
func (f *Fake) SetDistributionDigest(fullRef, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.distDigest == nil {
		f.distDigest = map[string]string{}
	}
	f.distDigest[fullRef] = digest
}

// SetDistributionFailure makes the distribution route answer the full reference (for example
// "registry.example:5000/app:1") with the status and message from now on, as one registry that
// is down, refuses a credential or does not know the image does, while every other reference is
// answered as before. The check is still recorded in DistributionRefs.
func (f *Fake) SetDistributionFailure(fullRef string, status int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.distFail == nil {
		f.distFail = map[string]FakePullStatus{}
	}
	f.distFail[fullRef] = FakePullStatus{Code: status, Message: message}
}

// DistributionAuth returns the X-Registry-Auth header of every distribution check the
// daemon served, in order; an anonymous check sends none and is recorded as "".
func (f *Fake) DistributionAuth() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.distAuth)
}

// DistributionRefs returns the reference of every distribution check the daemon served.
func (f *Fake) DistributionRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.distRefs)
}

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
	if cfg.PullHold != nil {
		select {
		case <-cfg.PullHold:
		case <-r.Context().Done():
			return true
		}
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

// withImages returns c with the images pulled through the daemon listed after its own, the
// images removed through it dropped and the names taken off an image gone. The slices are
// copied, so the Fake's own configuration is not touched.
func (c FakeConfig) withImages(pulled []FakeImage, gone map[string]bool, untagged map[string]map[string]bool) FakeConfig {
	if len(pulled) == 0 && len(gone) == 0 && len(untagged) == 0 {
		return c
	}
	all := append(slices.Clone(c.Images), pulled...)
	out := make([]FakeImage, 0, len(all))
	for _, fi := range all {
		if gone[fi.ID] {
			continue
		}
		if off := untagged[fi.ID]; len(off) > 0 {
			keep := func(n string) bool { return !off[normaliseImage(n)] }
			fi.RepoTags = slices.DeleteFunc(slices.Clone(fi.RepoTags), func(n string) bool { return !keep(n) })
			fi.RepoDigests = slices.DeleteFunc(slices.Clone(fi.RepoDigests), func(n string) bool { return !keep(n) })
		}
		out = append(out, fi)
	}
	c.Images = out
	return c
}

var _ = registerFakeHandler((*Fake).serveImageRemove)

// imageDeletePath matches /images/{name}, a name that has no further segment such as /json.
var imageDeletePath = regexp.MustCompile(`^/images/(.+)$`)

// serveImageRemove answers DELETE /images/{name} as a daemon does. By a name (a tag or a
// repo@digest) it untags that name and keeps the image while another name remains; the last
// name deletes the image unless a container was created from it (409, cannot be forced). By
// the image ID it deletes the image, but an image with several names is a 409 "must be
// forced", and one a container uses a 409 "being used by". An unknown name is 404. The write
// is recorded with its query whatever the answer, so a test can see that force was never sent.
func (f *Fake) serveImageRemove(w http.ResponseWriter, r *http.Request, p string, cfg FakeConfig) bool {
	if r.Method != http.MethodDelete {
		return false
	}
	m := imageDeletePath.FindStringSubmatch(p)
	if m == nil {
		return false
	}
	f.recordWrite(r)
	defer f.beginWrite(cfg.WriteDelay)()
	if cfg.DropWrites && dropConnection(w) {
		return true
	}
	if cfg.PolicyDeny {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "agent_policy_denied: " + r.Method + " " + r.URL.Path})
		return true
	}
	name := m[1]
	fi, ok := cfg.resolveImage(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such image: " + name})
		return true
	}
	force := r.URL.Query().Get("force") != ""
	byID := fi.ID == name || strings.TrimPrefix(fi.ID, "sha256:") == name
	names := append(slices.Clone(fi.RepoTags), fi.RepoDigests...)
	users := f.imageUsers(cfg, fi.ID)
	short := strings.TrimPrefix(fi.ID, "sha256:")[:min(12, len(strings.TrimPrefix(fi.ID, "sha256:")))]
	if !force {
		switch {
		case byID && len(names) > 1:
			writeJSON(w, http.StatusConflict, map[string]string{"message": "unable to delete " + short + " (must be forced) - image is referenced in multiple repositories"})
			return true
		case len(users) > 0 && (byID || len(names) <= 1):
			writeJSON(w, http.StatusConflict, map[string]string{"message": "conflict: unable to delete " + short + " (cannot be forced) - image is being used by stopped container " + users[0]})
			return true
		}
	}
	var resp []map[string]string
	f.mu.Lock()
	if f.imgUntagged == nil {
		f.imgUntagged = map[string]map[string]bool{}
	}
	if f.imgGone == nil {
		f.imgGone = map[string]bool{}
	}
	drop := names
	if !byID && len(names) > 1 {
		drop = []string{name}
	}
	for _, n := range drop {
		if f.imgUntagged[fi.ID] == nil {
			f.imgUntagged[fi.ID] = map[string]bool{}
		}
		f.imgUntagged[fi.ID][normaliseImage(n)] = true
		resp = append(resp, map[string]string{"Untagged": n})
	}
	if byID || len(names) <= 1 {
		f.imgGone[fi.ID] = true
		resp = append(resp, map[string]string{"Deleted": fi.ID})
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
	return true
}

// imageUsers are the IDs of the containers created from an image, or one stand-in for a
// container that started in between when the image is in BusyImages.
func (f *Fake) imageUsers(cfg FakeConfig, id string) []string {
	var out []string
	for _, c := range cfg.Containers {
		if c.ImageID == id {
			out = append(out, c.ID)
		}
	}
	if slices.Contains(cfg.BusyImages, id) {
		out = append(out, "started-meanwhile")
	}
	return out
}
