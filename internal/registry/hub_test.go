// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package registry

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// Docker Hub is read through its tags API, which costs no pulls (plan 05-19, IMG-03, D-20,
// T-05-61). The Hub is a real TLS server in the test; the registry path of the same service
// dials through an egress policy that refuses every address, so a call that succeeds did not
// use it.

type hubFixtureTag struct {
	name    string
	digest  string
	updated time.Time
	images  []hubFixtureImage
}

type hubFixtureImage struct {
	arch, os, variant, digest string
	size                      int64
}

type fakeHub struct {
	srv *httptest.Server

	mu     sync.Mutex
	paths  []string
	tags   []hubFixtureTag
	bodies []string

	// private makes the API answer 404 to a call without the bearer token.
	private bool
	// tokenStatus is the answer to POST /v2/auth/token: 0 or 200 give a token.
	tokenStatus int
	// limited429 is how many of the first API calls are answered 429, or every call when
	// negative.
	limited429 int
	retryAfter string
	calls      int
}

const hubJWT = "hub-jwt-value"

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHub) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths = append(h.paths, r.Method+" "+r.URL.Path)
}

func (h *fakeHub) apiCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.paths)
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	h.record(r)
	if r.URL.Path == "/v2/auth/token" {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, string(body))
		status := h.tokenStatus
		h.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": hubJWT})
		return
	}
	h.mu.Lock()
	h.calls++
	limited := h.limited429 < 0 || h.calls <= h.limited429
	retry := h.retryAfter
	private := h.private
	h.mu.Unlock()
	if limited {
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if private && r.Header.Get("Authorization") != "Bearer "+hubJWT {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	const base = "/v2/namespaces/acme/repositories/app/tags"
	switch {
	case r.URL.Path == base:
		h.serveList(w, r)
	case strings.HasPrefix(r.URL.Path, base+"/"):
		name := strings.TrimPrefix(r.URL.Path, base+"/")
		for _, tg := range h.tags {
			if tg.name == name {
				_ = json.NewEncoder(w).Encode(hubJSON(tg))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func hubJSON(tg hubFixtureTag) map[string]any {
	var images []any
	for _, im := range tg.images {
		images = append(images, map[string]any{"architecture": im.arch, "os": im.os, "variant": im.variant, "digest": im.digest, "size": im.size})
	}
	return map[string]any{"name": tg.name, "digest": tg.digest, "last_updated": tg.updated.UTC().Format(time.RFC3339Nano), "images": images}
}

func (h *fakeHub) serveList(w http.ResponseWriter, r *http.Request) {
	size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if size <= 0 || size > 100 {
		size = 100
	}
	page = max(page, 1)
	start := (page - 1) * size
	var results []any
	for i := start; i < min(start+size, len(h.tags)); i++ {
		results = append(results, hubJSON(h.tags[i]))
	}
	var next any
	if start+size < len(h.tags) {
		next = fmt.Sprintf("%s%s?page=%d&page_size=%d", h.srv.URL, r.URL.Path, page+1, size)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(h.tags), "next": next, "previous": nil, "results": results})
}

// blockEverything is an egress policy no address passes.
func blockEverything() EgressPolicy {
	return NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")})
}

func hubService(t *testing.T, h *fakeHub) *TagService {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool := x509.NewCertPool()
	pool.AddCert(h.srv.Certificate())
	egress := NewEgressPolicy(nil).WithRootCAs(pool)
	s := NewTagService(NewClientCache(blockEverything(), "test", log), egress, "test", log)
	s.hub.base = h.srv.URL
	s.hub.lim = rate.NewLimiter(rate.Inf, 1)
	return s
}

func hubRef(t *testing.T) Ref {
	t.Helper()
	r, err := ParseRef("docker.io/acme/app")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var hubT0 = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// hubTags makes n tags t0 .. t{n-1}; a larger number is older, and the slice is in an order
// the API would not use (oldest first) so the service has to sort them.
func hubTags(n int) []hubFixtureTag {
	var out []hubFixtureTag
	for i := n - 1; i >= 0; i-- {
		d := sha([]byte(fmt.Sprintf("index-%d", i)))
		out = append(out, hubFixtureTag{
			name: fmt.Sprintf("t%d", i), digest: d, updated: hubT0.Add(-time.Duration(i) * time.Hour),
			images: []hubFixtureImage{
				{arch: "amd64", os: "linux", digest: sha([]byte("a" + d)), size: 111},
				{arch: "arm", os: "linux", variant: "v7", digest: sha([]byte("v" + d)), size: 222},
				{arch: "unknown", os: "unknown", digest: sha([]byte("u" + d)), size: 3},
			},
		})
	}
	return out
}

func TestHubListReadsNamesDigestsDatesAndPlatformsFromTheTagsAPINewestFirst(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(250)
	s := hubService(t, h)
	page, err := s.List(context.Background(), nil, hubRef(t), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tags) != TagPageSize || page.Total != 250 || !page.More || page.Capped {
		t.Fatalf("page = total %d, tags %d, more %v, capped %v", page.Total, len(page.Tags), page.More, page.Capped)
	}
	for i, tg := range page.Tags {
		if tg.Name != fmt.Sprintf("t%d", i) {
			t.Fatalf("tag %d = %s, want t%d: newest first", i, tg.Name, i)
		}
	}
	first := page.Tags[0]
	if first.Digest != sha([]byte("index-0")) || first.Created == nil || !first.Created.Equal(hubT0) {
		t.Errorf("first = %+v, want the index digest and the date of the Hub", first)
	}
	if len(first.Platforms) != 2 || first.Platforms[0].Arch != "amd64" || first.Platforms[0].Size != 111 || first.Platforms[1].Variant != "v7" {
		t.Errorf("platforms = %+v, want amd64 and arm/v7 with unknown/unknown dropped", first.Platforms)
	}
	last, err := s.List(context.Background(), nil, hubRef(t), "", true, 2)
	if err != nil || len(last.Tags) != 50 || last.More || last.Tags[49].Name != "t249" {
		t.Errorf("last page = %d tags, more %v, err %v", len(last.Tags), last.More, err)
	}
}

func TestHubListNeverSendsAManifestRequestAndPagesTheTagsAPIOnly(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(250)
	s := hubService(t, h)
	if _, err := s.List(context.Background(), nil, hubRef(t), "", true, 0); err != nil {
		t.Fatal(err)
	}
	for _, p := range h.paths {
		if !strings.HasPrefix(p, "GET /v2/namespaces/acme/repositories/app/tags") {
			t.Errorf("request %q is not a call of the tags API", p)
		}
	}
	if len(h.paths) != 3 {
		t.Errorf("requests = %v, want the 3 pages of 100", h.paths)
	}
}

func TestHubListStopsAtTheCapAndSaysSo(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(hubListMax + 150)
	page, err := hubService(t, h).List(context.Background(), nil, hubRef(t), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Capped || page.Total != hubListMax {
		t.Errorf("total = %d, capped = %v, want %d and capped", page.Total, page.Capped, hubListMax)
	}
}

func TestHubDetailsComeFromTheListWithoutAnotherRequest(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(5)
	s := hubService(t, h)
	if _, err := s.List(context.Background(), nil, hubRef(t), "", true, 0); err != nil {
		t.Fatal(err)
	}
	before := h.apiCalls()
	got := s.Details(context.Background(), nil, hubRef(t), []string{"t1", "t3"}, "amd64")
	if h.apiCalls() != before {
		t.Errorf("details made %d requests, want none: the list holds them", h.apiCalls()-before)
	}
	if got[0].Tag != "t1" || got[0].Digest != sha([]byte("index-1")) || got[0].Created == nil || !got[0].Created.Equal(hubT0.Add(-time.Hour)) || len(got[0].Platforms) != 2 || got[0].Code != "" {
		t.Errorf("detail = %+v", got[0])
	}
}

func TestHubDetailsOfATagThatIsNotListedAskTheTagEndpointOnce(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(5)
	s := hubService(t, h)
	got := s.Details(context.Background(), nil, hubRef(t), []string{"t2", "missing"}, "amd64")
	if got[0].Digest != sha([]byte("index-2")) || got[0].Created == nil || got[0].Code != "" {
		t.Errorf("detail = %+v", got[0])
	}
	if got[1].Code != CodeTagNotFound || got[1].Tag != "missing" {
		t.Errorf("missing = %+v, want tag_not_found", got[1])
	}
	again := s.Details(context.Background(), nil, hubRef(t), []string{"t2"}, "amd64")
	if again[0].Digest != got[0].Digest {
		t.Errorf("again = %+v", again[0])
	}
	if n := len(h.paths); n != 2 {
		t.Errorf("requests = %v, want one per tag asked for the first time", h.paths)
	}
	for _, p := range h.paths {
		if strings.Contains(p, "manifests") {
			t.Errorf("request %q is a manifest request: Docker Hub pull quota (T-05-61)", p)
		}
	}
}

func TestHubWaitsForRetryAfterOnceThenAnswersRateLimited(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(3)
	h.limited429, h.retryAfter = 1, "1"
	s := hubService(t, h)
	start := time.Now()
	page, err := s.List(context.Background(), nil, hubRef(t), "", true, 0)
	if err != nil || len(page.Tags) != 3 {
		t.Fatalf("list after one 429: %v %v", page, err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("retried after %v, want the 1 second Retry-After waited", time.Since(start))
	}

	h2 := newFakeHub(t)
	h2.tags = hubTags(3)
	h2.limited429, h2.retryAfter = -1, "0"
	_, err = hubService(t, h2).List(context.Background(), nil, hubRef(t), "", true, 0)
	if got := TagErrorCode(err); got != CodeRateLimited {
		t.Errorf("error = %v (%q), want rate_limited after the one retry", err, got)
	}
	if n := h2.apiCalls(); n != 2 {
		t.Errorf("requests = %d, want the call and one retry", n)
	}
}

func TestHubIsPacedAtTwoRequestsASecondWithABurstOfTen(t *testing.T) {
	s := NewTagService(nil, NewEgressPolicy(nil), "test", nil)
	if got := s.hub.lim.Limit(); got != 2 {
		t.Errorf("limit = %v, want 2 per second", got)
	}
	if got := s.hub.lim.Burst(); got != 10 {
		t.Errorf("burst = %d, want 10", got)
	}
	if s.hub.base != "https://hub.docker.com" {
		t.Errorf("base = %q", s.hub.base)
	}
}

func TestHubPrivateRepositoryListsWithATokenFromTheCredential(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(3)
	h.private = true
	s := hubService(t, h)
	cred := &ResolvedCredential{
		Candidate:   Candidate{ID: "c1", Layer: LayerInstance},
		HostOptions: HostOptions{Host: "docker.io", Username: "ana", Password: "pat"},
		UpdatedAt:   tagsT0,
	}
	page, err := s.List(context.Background(), cred, hubRef(t), "", true, 0)
	if err != nil || len(page.Tags) != 3 {
		t.Fatalf("list: %v %v", page, err)
	}
	if len(h.bodies) != 1 || !strings.Contains(h.bodies[0], `"identifier":"ana"`) || !strings.Contains(h.bodies[0], `"secret":"pat"`) {
		t.Errorf("token request bodies = %v", h.bodies)
	}
	// The token is kept for the next call.
	s.Details(context.Background(), cred, hubRef(t), []string{"t0"}, "amd64")
	s.Details(context.Background(), cred, hubRef(t), []string{"nolist"}, "amd64")
	if len(h.bodies) != 1 {
		t.Errorf("token requests = %d, want the one token reused", len(h.bodies))
	}
}

func TestHubFallsBackToTheRegistryPathWhenTheTokenIsRefusedOrTheRepositoryIsNotThere(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(3)
	h.private = true
	h.tokenStatus = http.StatusUnauthorized
	s := hubService(t, h)
	cred := &ResolvedCredential{
		Candidate:   Candidate{ID: "c1", Layer: LayerInstance},
		HostOptions: HostOptions{Host: "docker.io", Username: "ana", Password: "bad"},
		UpdatedAt:   tagsT0,
	}
	// The fallback is the registry path, which the test's egress policy refuses.
	_, err := s.List(context.Background(), cred, hubRef(t), "", true, 0)
	if got := TagErrorCode(err); got != CodeRegistryUnreachable {
		t.Errorf("refused token: %v (%q), want the registry path to have been tried", err, got)
	}
	_, err = s.List(context.Background(), nil, hubRef(t), "", true, 0)
	if got := TagErrorCode(err); got != CodeRegistryUnreachable {
		t.Errorf("anonymous private: %v (%q), want the registry path to have been tried", err, got)
	}
}

func TestHubKeepsWhatACredentialSawAwayFromAnonymousCalls(t *testing.T) {
	h := newFakeHub(t)
	h.tags = hubTags(3)
	h.private = true
	s := hubService(t, h)
	cred := &ResolvedCredential{
		Candidate:   Candidate{ID: "c1", Layer: LayerInstance},
		HostOptions: HostOptions{Host: "docker.io", Username: "ana", Password: "pat"},
		UpdatedAt:   tagsT0,
	}
	if _, err := s.List(context.Background(), cred, hubRef(t), "", true, 0); err != nil {
		t.Fatal(err)
	}
	got := s.Details(context.Background(), nil, hubRef(t), []string{"t0"}, "amd64")[0]
	if got.Digest != "" || got.Created != nil || got.Code == "" {
		t.Errorf("anonymous details = %+v, want nothing from the credential's cache", got)
	}
}
