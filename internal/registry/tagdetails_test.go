// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The details of the tags on screen (plan 05-19, IMG-03, D-20, D-23, D-24a). The registry is
// a real TLS server in the test that serves manifests and config blobs.

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type fakeManifest struct {
	body      []byte
	mediaType string
}

// fakeImages serves manifests and blobs for a fakeRegistry and counts what it is asked.
type fakeImages struct {
	f *fakeRegistry

	mu        sync.Mutex
	manifests map[string]fakeManifest // repo|digest
	tags      map[string]string       // repo|tag -> digest
	blobs     map[string][]byte       // digest -> body
	requests  []string                // "METHOD kind ref"
	failBlobs bool
	delay     time.Duration
	// refuse answers every manifest and blob request that carries the issued token with a 401
	// challenge, the way Docker Hub answers a repository it hides.
	refuse bool

	inflight, maxInflight int
}

func newFakeImages(f *fakeRegistry) *fakeImages {
	fi := &fakeImages{f: f, manifests: map[string]fakeManifest{}, tags: map[string]string{}, blobs: map[string][]byte{}}
	f.extra = fi.handle
	return fi
}

const (
	mtOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex    = "application/vnd.oci.image.index.v1+json"
)

type imageSpec struct {
	os, arch, variant string
	created           time.Time
}

// addImage stores an image manifest with its config and returns the manifest's digest.
func (fi *fakeImages) addImage(repo, tag string, s imageSpec) string {
	cfg, _ := json.Marshal(map[string]any{
		"architecture": s.arch, "os": s.os, "variant": s.variant, "created": s.created.UTC().Format(time.RFC3339),
	})
	cfgDigest := sha(cfg)
	man, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mtOCIManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": len(cfg)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": sha([]byte(s.arch + s.variant)), "size": 1000}},
	})
	d := sha(man)
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.blobs[cfgDigest] = cfg
	fi.manifests[repo+"|"+d] = fakeManifest{body: man, mediaType: mtOCIManifest}
	if tag != "" {
		fi.tags[repo+"|"+tag] = d
	}
	return d
}

type indexEntry struct {
	digest, os, arch, variant string
	attestation               bool
}

// addIndex stores an image index under tag and returns its digest.
func (fi *fakeImages) addIndex(repo, tag string, entries ...indexEntry) string {
	var list []any
	for _, e := range entries {
		m := map[string]any{
			"mediaType": mtOCIManifest, "digest": e.digest, "size": 400,
			"platform": map[string]any{"os": e.os, "architecture": e.arch, "variant": e.variant},
		}
		if e.attestation {
			m["annotations"] = map[string]string{"vnd.docker.reference.type": "attestation-manifest"}
		}
		list = append(list, m)
	}
	body, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mtOCIIndex, "manifests": list})
	d := sha(body)
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.manifests[repo+"|"+d] = fakeManifest{body: body, mediaType: mtOCIIndex}
	fi.tags[repo+"|"+tag] = d
	return d
}

func (fi *fakeImages) handle(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v2/")
	var repo, kind, ref string
	switch {
	case strings.Contains(rest, "/manifests/"):
		i := strings.LastIndex(rest, "/manifests/")
		repo, kind, ref = rest[:i], "manifests", rest[i+len("/manifests/"):]
	case strings.Contains(rest, "/blobs/"):
		i := strings.LastIndex(rest, "/blobs/")
		repo, kind, ref = rest[:i], "blobs", rest[i+len("/blobs/"):]
	default:
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer good" {
		fi.f.challenge(w, "repository:"+repo+":pull")
		return
	}
	if fi.refuse {
		fi.f.challenge(w, "repository:"+repo+":pull")
		return
	}
	fi.mu.Lock()
	fi.requests = append(fi.requests, r.Method+" "+kind+" "+ref)
	fi.inflight++
	fi.maxInflight = max(fi.maxInflight, fi.inflight)
	delay := fi.delay
	fi.mu.Unlock()
	defer func() {
		fi.mu.Lock()
		fi.inflight--
		fi.mu.Unlock()
	}()
	if delay > 0 {
		time.Sleep(delay)
	}
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if kind == "blobs" {
		b, ok := fi.blobs[ref]
		if !ok || fi.failBlobs {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Docker-Content-Digest", ref)
		if r.Method != http.MethodHead {
			_, _ = w.Write(b)
		}
		return
	}
	d := ref
	if !strings.HasPrefix(ref, "sha256:") {
		d = fi.tags[repo+"|"+ref]
	}
	m, ok := fi.manifests[repo+"|"+d]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	w.Header().Set("Docker-Content-Digest", d)
	if r.Method != http.MethodHead {
		_, _ = w.Write(m.body)
	}
}

// count is how many requests of a method and kind ("GET manifests") were made.
func (fi *fakeImages) count(method, kind string) int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	n := 0
	for _, r := range fi.requests {
		if strings.HasPrefix(r, method+" "+kind+" ") {
			n++
		}
	}
	return n
}

func (fi *fakeImages) total() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return len(fi.requests)
}

var imgT0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// multiArch stores an index with amd64, arm64 and an attestation entry, and returns the
// index digest and the platform digests.
func multiArch(fi *fakeImages, repo, tag string) (index, amd, arm string) {
	amd = fi.addImage(repo, "", imageSpec{"linux", "amd64", "", imgT0})
	arm = fi.addImage(repo, "", imageSpec{"linux", "arm64", "", imgT0.Add(-time.Hour)})
	att := sha([]byte("attestation"))
	index = fi.addIndex(repo, tag,
		indexEntry{digest: amd, os: "linux", arch: "amd64"},
		indexEntry{digest: arm, os: "linux", arch: "arm64"},
		indexEntry{digest: att, os: "unknown", arch: "unknown", attestation: true},
	)
	return index, amd, arm
}

func TestDetailsForAnIndexTagGivesTheIndexDigestRealPlatformsAndTheAmd64Date(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	index, amd, arm := multiArch(fi, "acme/app", "1.0")

	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"1.0"}, "arm64")
	if len(got) != 1 {
		t.Fatalf("details = %+v", got)
	}
	d := got[0]
	if d.Tag != "1.0" || d.Digest != index || d.Code != "" {
		t.Errorf("detail = %+v, want the index digest %s and no code", d, index)
	}
	if d.Created == nil || !d.Created.Equal(imgT0) {
		t.Errorf("created = %v, want %v from the linux/amd64 config, not the preferred arm64 one", d.Created, imgT0)
	}
	if len(d.Platforms) != 2 {
		t.Fatalf("platforms = %+v, want amd64 and arm64 with the attestation entry dropped", d.Platforms)
	}
	if p := d.Platforms[0]; p.OS != "linux" || p.Arch != "amd64" || p.Digest != amd || p.Size != 1000 {
		t.Errorf("platform 0 = %+v, want linux/amd64 %s with the 1000 byte size of the manifest that was read", p, amd)
	}
	if p := d.Platforms[1]; p.Arch != "arm64" || p.Digest != arm || p.Size != 0 {
		t.Errorf("platform 1 = %+v, want arm64 %s with an unknown size", p, arm)
	}
	if n := fi.count("HEAD", "manifests"); n != 1 {
		t.Errorf("HEAD manifests = %d, want 1", n)
	}
}

func TestDetailsReadsTheDateFromThePreferredArchitectureWhenThereIsNoAmd64(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	armV7 := fi.addImage("acme/app", "", imageSpec{"linux", "arm", "v7", imgT0.Add(-2 * time.Hour)})
	arm64 := fi.addImage("acme/app", "", imageSpec{"linux", "arm64", "", imgT0.Add(-3 * time.Hour)})
	fi.addIndex("acme/app", "2.0",
		indexEntry{digest: armV7, os: "linux", arch: "arm", variant: "v7"},
		indexEntry{digest: arm64, os: "linux", arch: "arm64"},
	)
	s := tagServiceFor(f)
	got := s.Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"2.0"}, "arm64")[0]
	if got.Created == nil || !got.Created.Equal(imgT0.Add(-3*time.Hour)) {
		t.Errorf("created = %v, want the arm64 config's date: no amd64, so the node architecture", got.Created)
	}
	// With neither, the first real platform.
	got = s.Details(context.Background(), f.cred("c2", f.pass), f.ref(t, "acme/app"), []string{"2.0"}, "riscv64")[0]
	if got.Created == nil || !got.Created.Equal(imgT0.Add(-2*time.Hour)) {
		t.Errorf("created = %v, want the first platform's date (arm/v7)", got.Created)
	}
}

func TestDetailsForASinglePlatformImageHasOnePlatformAndItsDate(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	d := fi.addImage("acme/app", "single", imageSpec{"linux", "arm", "v7", imgT0})
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"single"}, "amd64")[0]
	if got.Digest != d || got.Code != "" || got.Created == nil || !got.Created.Equal(imgT0) {
		t.Fatalf("detail = %+v", got)
	}
	if len(got.Platforms) != 1 || got.Platforms[0].Arch != "arm" || got.Platforms[0].Variant != "v7" || got.Platforms[0].Digest != d || got.Platforms[0].Size != 1000 {
		t.Errorf("platforms = %+v, want the one platform of the image", got.Platforms)
	}
}

func TestDetailsAreServedFromTheCachesOnASecondCall(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	multiArch(fi, "acme/app", "1.0")
	s := tagServiceFor(f)
	cred, ref := f.cred("c1", f.pass), f.ref(t, "acme/app")
	first := s.Details(context.Background(), cred, ref, []string{"1.0"}, "amd64")
	before := fi.total()
	second := s.Details(context.Background(), cred, ref, []string{"1.0"}, "amd64")
	if fi.total() != before {
		t.Errorf("a second call made %d more requests, want none", fi.total()-before)
	}
	if first[0].Digest != second[0].Digest || len(second[0].Platforms) != 2 || second[0].Created == nil {
		t.Errorf("second = %+v, first = %+v", second, first)
	}
}

func TestDetailsReadTheDigestOfEachTagAndShareTheDetailsOfTagsWithOneDigest(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	index, _, _ := multiArch(fi, "acme/app", "1.0")
	fi.mu.Lock()
	fi.tags["acme/app|latest"] = index
	fi.mu.Unlock()
	s := tagServiceFor(f)
	got := s.Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"1.0", "latest"}, "amd64")
	if got[0].Digest != index || got[1].Digest != index || got[0].Tag != "1.0" || got[1].Tag != "latest" {
		t.Fatalf("details = %+v, want both tags on %s", got, index)
	}
	if n := fi.count("HEAD", "manifests"); n != 2 {
		t.Errorf("HEAD manifests = %d, want one per tag", n)
	}
}

func TestDetailsCachesAreKeyedByCredentialSoAnotherCallerIsNotServedWhatItMayNotSee(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	multiArch(fi, "acme/app", "1.0")
	s := tagServiceFor(f)
	ref := f.ref(t, "acme/app")
	s.Details(context.Background(), f.cred("c1", f.pass), ref, []string{"1.0"}, "amd64")
	before := fi.total()
	// Anonymous: the registry refuses it, and the cache of the credentialed call is not used.
	got := s.Details(context.Background(), nil, ref, []string{"1.0"}, "amd64")[0]
	if got.Code != CodeNeedsLogin || got.Digest != "" || got.Platforms != nil || got.Created != nil {
		t.Errorf("anonymous detail = %+v, want needs_login and nothing else", got)
	}
	// Another credential is another cache.
	s.Details(context.Background(), f.cred("c2", f.pass), ref, []string{"1.0"}, "amd64")
	if fi.total() == before {
		t.Error("a second credential was served the first one's cached details")
	}
}

func TestDetailsGiveNoDateButKeepThePlatformsWhenTheConfigCannotBeRead(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	index, _, _ := multiArch(fi, "acme/app", "1.0")
	fi.failBlobs = true
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"1.0"}, "amd64")[0]
	if got.Created != nil || got.Code != CodeDateUnavailable || got.Digest != index || len(got.Platforms) != 2 {
		t.Errorf("detail = %+v, want the digest and platforms, no date and date_unavailable", got)
	}
}

func TestDetailsNameATagTheRegistryDoesNotHave(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	multiArch(fi, "acme/app", "1.0")
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), []string{"1.0", "gone"}, "amd64")
	if got[0].Code != "" || got[1].Code != CodeTagNotFound || got[1].Tag != "gone" || got[1].Digest != "" {
		t.Errorf("details = %+v, want 1.0 fine and gone tag_not_found", got)
	}
}

// A refusal after a working sign-in is the repository's, decided with one sign-in check for the
// whole request; a credential the token endpoint refuses is still login_refused (G-05-17a).
func TestDetailsOfARefusedRepositoryAreUnavailableWithOneSignInCheck(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	fi.refuse = true
	var checks atomic.Int32
	f.signedInV2 = func(w http.ResponseWriter, _ *http.Request) {
		checks.Add(1)
		w.WriteHeader(http.StatusOK)
	}
	tags := []string{"1.0", "2.0", "3.0"}
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), tags, "amd64")
	for _, d := range got {
		if d.Code != CodeRepositoryUnavailable || d.Digest != "" {
			t.Errorf("detail = %+v, want repository_unavailable", d)
		}
	}
	if n := checks.Load(); n != 1 {
		t.Errorf("sign-in checks = %d, want exactly one for the request", n)
	}
}

func TestDetailsOfARefusedRepositoryWithAWrongCredentialAreLoginRefused(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	fi.refuse = true
	multiArch(fi, "acme/app", "1.0")
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", "wrong"), f.ref(t, "acme/app"), []string{"1.0", "2.0", "gone"}, "amd64")
	for _, d := range got {
		if d.Code != CodeLoginRefused {
			t.Errorf("detail = %+v, want login_refused", d)
		}
	}
}

func TestDetailsWithAWrongCredentialOnARegistryWhoseV2IsOpenAreLoginRefused(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	f.open = true
	fi := newFakeImages(f)
	multiArch(fi, "acme/app", "1.0")
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", "wrong"), f.ref(t, "acme/app"), []string{"1.0"}, "amd64")
	if got[0].Code != CodeLoginRefused {
		t.Errorf("detail = %+v, want login_refused", got[0])
	}
}

func TestDetailsOfARefusedRepositoryWithNoCredentialNeedALogin(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	fi.refuse = true
	got := tagServiceFor(f).Details(context.Background(), nil, f.ref(t, "acme/app"), []string{"1.0"}, "amd64")
	if got[0].Code != CodeNeedsLogin {
		t.Errorf("detail = %+v, want needs_login", got[0])
	}
}

func TestDetailsOfManyTagsNeverHaveMoreThanThreeRequestsInFlight(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	fi.delay = 15 * time.Millisecond
	var tags []string
	for i := range 25 {
		tag := fmt.Sprintf("v%d", i)
		tags = append(tags, tag)
		img := fi.addImage("acme/app", tag, imageSpec{"linux", "amd64", "", imgT0.Add(time.Duration(i) * time.Minute)})
		_ = img
	}
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), tags, "amd64")
	for i, d := range got {
		if d.Tag != tags[i] || d.Code != "" || d.Created == nil {
			t.Fatalf("detail %d = %+v", i, d)
		}
	}
	if fi.maxInflight > reqConcurrent || fi.maxInflight == 0 {
		t.Errorf("most requests in flight = %d, want 1 to %d", fi.maxInflight, reqConcurrent)
	}
}

func TestChoosePlatformPrefersLinuxAmd64ThenTheNodeArchitectureThenTheFirst(t *testing.T) {
	ps := []Platform{{OS: "windows", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}}
	if got := choosePlatform(ps, "arm64"); got != 2 {
		t.Errorf("with amd64 present = %d, want 2", got)
	}
	ps = ps[:2]
	if got := choosePlatform(ps, "arm64"); got != 1 {
		t.Errorf("node arm64 = %d, want 1", got)
	}
	if got := choosePlatform(ps, "riscv64"); got != 0 {
		t.Errorf("nothing matches = %d, want the first", got)
	}
	if got := choosePlatform(nil, "amd64"); got != -1 {
		t.Errorf("no platforms = %d, want -1", got)
	}
}

// A node's 32-bit ARM architecture is spelled arm/v7 and a platform's is Arch arm with a variant
// (WR-A05), so the two must be compared as the same thing.
func TestChoosePlatformMatchesA32BitArmNodeToTheArmPlatformOfThatVariant(t *testing.T) {
	ps := []Platform{{OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "arm", Variant: "v6"}, {OS: "linux", Arch: "arm", Variant: "v7"}}
	if got := choosePlatform(ps, "arm/v7"); got != 2 {
		t.Errorf("node arm/v7 = %d, want 2", got)
	}
	if got := choosePlatform(ps, "arm/v6"); got != 1 {
		t.Errorf("node arm/v6 = %d, want 1", got)
	}
	if got := choosePlatform([]Platform{{OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "arm"}}, "arm/v7"); got != 1 {
		t.Errorf("an arm platform with no variant = %d, want 1 (read as v7)", got)
	}
}

// A tag becomes the last segment of a registry URL sent with the credential, so a name that is
// not a tag is never sent anywhere and reads as a tag the registry does not have (WR-A02).
func TestDetailsOfAnInvalidTagNameSendsNothingAndKeepsTheOrderOfTheRest(t *testing.T) {
	f := newFakeRegistry(t, http.StatusUnauthorized)
	fi := newFakeImages(f)
	index, _, _ := multiArch(fi, "acme/app", "1.0")

	bad := []string{"../../other/blobs/uploads/", "a/b", "x?y=1", "x#z", "a%2fb", "-lead", ".dot", strings.Repeat("a", 129), ""}
	tags := append([]string{"1.0"}, bad...)
	got := tagServiceFor(f).Details(context.Background(), f.cred("c1", f.pass), f.ref(t, "acme/app"), tags, "amd64")
	if len(got) != len(tags) {
		t.Fatalf("details = %d items, want %d", len(got), len(tags))
	}
	if got[0].Tag != "1.0" || got[0].Digest != index || got[0].Code != "" {
		t.Errorf("the valid tag = %+v, want it read", got[0])
	}
	for i, tag := range bad {
		if d := got[i+1]; d.Tag != tag || d.Code != CodeTagNotFound || d.Digest != "" {
			t.Errorf("tag %q = %+v, want tag_not_found and no read", tag, d)
		}
	}
	for _, r := range fi.requests {
		if strings.Contains(r, "..") || strings.Contains(r, "?") || strings.Contains(r, "uploads") || strings.Contains(r, "%") {
			t.Errorf("the registry was sent %q for an invalid tag", r)
		}
	}
	if !ValidTag("1.27.0-rc_1") || ValidTag("") {
		t.Error("ValidTag is wrong for an ordinary tag or an empty one")
	}
}
