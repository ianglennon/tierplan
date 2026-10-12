// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Browsing a repository's tags (plan 05-18, IMG-03, D-19, D-24a, T-05-60). The router,
// policy, rbac checker, vault and resolver are the real ones; the registry is a real TLS
// server in the test.

type tagsResp struct {
	Repo       string `json:"repo"`
	Host       string `json:"host"`
	Credential *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Scope struct {
			Kind    string  `json:"kind"`
			EnvName *string `json:"envName"`
		} `json:"scope"`
	} `json:"credential"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Hidden int  `json:"hidden"`
	Total  int  `json:"total"`
	More   bool `json:"more"`
	Page   int  `json:"page"`
	Capped bool `json:"capped"`
}

func (r tagsResp) names() []string {
	out := make([]string, len(r.Tags))
	for i, t := range r.Tags {
		out[i] = t.Name
	}
	slices.Sort(out)
	return out
}

// registryCredentialBody is a credential for the fake registry covering the acme namespace.
func (f *fakeRegistry) credentialBody(name string) string {
	b, _ := json.Marshal(map[string]string{
		"name": name, "type": "generic", "host": f.host(), "prefix": "acme", "username": f.user, "secret": f.pass,
	})
	return string(b)
}

func (f *authFixture) browse(t *testing.T, u credentialUser, reg *fakeRegistry, repo, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/registries/tags?ref=" + url.QueryEscape(reg.host()+"/"+repo)
	if query != "" {
		path += "&" + query
	}
	return f.credCall(t, u, http.MethodGet, path, "")
}

func problemErrors(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var p struct {
		Errors []struct {
			Location string `json:"location"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	out := map[string]string{}
	for _, e := range p.Errors {
		out[e.Location] = e.Message
	}
	return out
}

func TestBrowseTagsListsTheNamesWithTheInstanceCredentialThatMatches(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	reg.tags = []string{"3", "1", "2"}
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	cred := f.storeCredential(t, admin, reg.credentialBody("fake-inst"))
	f.addEnvironmentRow(t, "env-a")
	viewer := f.asRoleOn(t, "viewera", "env-a", "viewer")

	rec := f.browse(t, viewer, reg, "acme/app", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("browse: %d %s", rec.Code, rec.Body)
	}
	got := decodeRec[tagsResp](t, rec)
	if got.Repo != "acme/app" || got.Host != reg.host() || !slices.Equal(got.names(), []string{"1", "2", "3"}) {
		t.Errorf("answer = %+v", got)
	}
	if got.Credential == nil || got.Credential.ID != cred.ID || got.Credential.Name != "fake-inst" || got.Credential.Scope.Kind != "instance" {
		t.Errorf("credential = %+v, want the instance one, named", got.Credential)
	}
	if strings.Contains(rec.Body.String(), reg.pass) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the answer carries the secret or is cacheable: %s %v", rec.Body, rec.Header())
	}
	if actions, _ := f.registryAudit(t); !slices.Equal(actions, []string{"registry.credential_created"}) {
		t.Errorf("registry audit actions = %v; browsing is not audited", actions)
	}
}

func TestBrowseTagsIsAnonymousWhenNoCredentialMatchesAndTheRegistryWantsALogin(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, reg.credentialBody("fake-inst"))

	rec := f.browse(t, admin, reg, "other/app", "")
	if rec.Code != http.StatusBadGateway || problemType(t, rec) != "needs_login" {
		t.Fatalf("browse: %d %s, want 502 needs_login", rec.Code, rec.Body)
	}
	errs := problemErrors(t, rec)
	if errs["registry.host"] != reg.host() || errs["registry.repo"] != "other/app" {
		t.Errorf("errors = %v, want the registry and repository named", errs)
	}
	if _, named := errs["credential.id"]; named {
		t.Errorf("errors = %v, want no credential named for an anonymous request", errs)
	}
}

func TestBrowseTagsRefusesACallerWhoCannotReadImagesAnywhereOrOnTheChosenEnvironment(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")

	for name, tc := range map[string]struct {
		u     credentialUser
		query string
		want  int
	}{
		"no grant at all":          {credentialUser{nobodyCookie, nobodyCSRF}, "", http.StatusForbidden},
		"no grant, with envId":     {credentialUser{nobodyCookie, nobodyCSRF}, "envId=" + a.ID, http.StatusForbidden},
		"viewer of A on B":         {viewerA, "envId=" + b.ID, http.StatusForbidden},
		"viewer of A on nowhere":   {viewerA, "envId=no-such-environment", http.StatusForbidden},
		"viewer of A on A":         {viewerA, "envId=" + a.ID, http.StatusBadGateway}, // allowed; the registry wants a login
		"viewer of A, no envId":    {viewerA, "", http.StatusBadGateway},
		"instance admin on B":      {credentialUser{f.cookie, f.csrf}, "envId=" + b.ID, http.StatusBadGateway},
		"instance admin, no envId": {credentialUser{f.cookie, f.csrf}, "", http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.browse(t, tc.u, reg, "acme/app", tc.query)
			if rec.Code != tc.want {
				t.Errorf("browse: %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
		})
	}
	if reg.tagLists.Load() != 0 {
		t.Errorf("the registry served %d tag lists; a refused caller must reach nothing", reg.tagLists.Load())
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'permission.denied' AND target_id = 'GET /api/registries/tags'`).Scan(&n); err != nil || n != 4 {
		t.Errorf("refusals audited = %d, %v, want 4", n, err)
	}
}

func TestBrowseTagsNeverServesAListFetchedWithACredentialTheCallerCannotUse(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	reg.tags = []string{"secret-release-1"}
	f.trusting(t, reg)
	admin := credentialUser{f.cookie, f.csrf}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+b.ID+"/registries", reg.credentialBody("fake-in-b")); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")
	viewerB := f.asRoleOn(t, "viewerb", b.ID, "viewer")

	// The admin browses with B's credential, which fills the cache for it.
	rec := f.browse(t, admin, reg, "acme/app", "envId="+b.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	got := decodeRec[tagsResp](t, rec)
	if got.Credential == nil || got.Credential.Scope.Kind != "environment" || !slices.Equal(got.names(), []string{"secret-release-1"}) {
		t.Fatalf("admin answer = %+v", got)
	}
	served := reg.tagLists.Load()

	// A viewer of A has no credential that applies: the registry is asked anonymously and
	// refuses, and the names fetched with B's credential are nowhere in the answer.
	rec = f.browse(t, viewerA, reg, "acme/app", "")
	if rec.Code != http.StatusBadGateway || problemType(t, rec) != "needs_login" || strings.Contains(rec.Body.String(), "secret-release-1") {
		t.Errorf("viewer of A: %d %s, want 502 needs_login without the names", rec.Code, rec.Body)
	}
	// So does a viewer of B who does not choose B: the environment's credentials apply only
	// when the environment is chosen.
	rec = f.browse(t, viewerB, reg, "acme/app", "")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "secret-release-1") {
		t.Errorf("viewer of B without envId: %d %s, want 502 and no names", rec.Code, rec.Body)
	}
	if reg.tagLists.Load() != served {
		t.Errorf("tag lists served = %d, want %d: the anonymous calls reached the registry and were refused", reg.tagLists.Load(), served)
	}
	// A viewer of B who chooses B may use it, and is served the cached list.
	rec = f.browse(t, viewerB, reg, "acme/app", "envId="+b.ID)
	if rec.Code != http.StatusOK || !slices.Equal(decodeRec[tagsResp](t, rec).names(), []string{"secret-release-1"}) {
		t.Errorf("viewer of B with envId: %d %s, want the names", rec.Code, rec.Body)
	}
	if reg.tagLists.Load() != served {
		t.Errorf("tag lists served = %d, want the cached list reused (%d)", reg.tagLists.Load(), served)
	}
}

func TestBrowseTagsNamesTheCredentialTheRegistryRefusedAndRecordsTheFailure(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	cred := f.storeCredential(t, admin, reg.credentialBody("fake-inst"))
	reg.pass = "changed-at-the-registry-after-saving"

	rec := f.browse(t, admin, reg, "acme/app", "")
	if rec.Code != http.StatusBadGateway || problemType(t, rec) != "login_refused" {
		t.Fatalf("browse: %d %s, want 502 login_refused", rec.Code, rec.Body)
	}
	errs := problemErrors(t, rec)
	if errs["credential.id"] != cred.ID || errs["credential.name"] != "fake-inst" || errs["credential.scopeKind"] != "instance" {
		t.Errorf("errors = %v, want the credential named", errs)
	}
	if strings.Contains(rec.Body.String(), reg.pass) || strings.Contains(rec.Body.String(), credToken) {
		t.Errorf("the problem carries a secret: %s", rec.Body)
	}
	var code string
	if err := f.st.Read.QueryRow(`SELECT last_failure_code FROM registry_credentials WHERE id = ?`, cred.ID).Scan(&code); err != nil || code == "" {
		t.Errorf("last failure code = %q, %v, want the refusal recorded (D-07)", code, err)
	}
}

func TestBrowseTagsMapsAMissingRepositoryAndAnUnparsableReference(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	reg.tagsStatus = http.StatusNotFound
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, reg.credentialBody("fake-inst"))

	rec := f.browse(t, admin, reg, "acme/missing", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "repository_not_found" {
		t.Errorf("missing: %d %s, want 404 repository_not_found", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodGet, "/api/registries/tags?ref="+url.QueryEscape("NOT A REF!!"), "")
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ref_invalid" {
		t.Errorf("invalid: %d %s, want 422 ref_invalid", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodGet, "/api/registries/tags", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no ref: %d %s, want 422", rec.Code, rec.Body)
	}
}

func TestBrowseTagsSortsFiltersHidesAndPages(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	reg.tags = []string{"latest", "app.sig"}
	for i := range 120 {
		reg.tags = append(reg.tags, fmt.Sprintf("1.%d.0", i))
	}
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, reg.credentialBody("fake-inst"))
	get := func(query string) tagsResp {
		t.Helper()
		rec := f.browse(t, admin, reg, "acme/app", query)
		if rec.Code != http.StatusOK {
			t.Fatalf("browse %q: %d %s", query, rec.Code, rec.Body)
		}
		return decodeRec[tagsResp](t, rec)
	}

	first := get("")
	if len(first.Tags) != 100 || first.Total != 121 || first.Hidden != 1 || !first.More || first.Page != 0 || first.Tags[0].Name != "1.119.0" {
		t.Errorf("first page = %d tags, total %d, hidden %d, more %v, page %d, first %q; want 100, 121, 1, true, 0, 1.119.0",
			len(first.Tags), first.Total, first.Hidden, first.More, first.Page, first.Tags[0].Name)
	}
	second := get("page=1")
	if len(second.Tags) != 21 || second.More || second.Page != 1 || second.Tags[20].Name != "latest" {
		t.Errorf("second page = %d tags, more %v, page %d, last %q; want 21, false, 1, latest", len(second.Tags), second.More, second.Page, second.Tags[len(second.Tags)-1].Name)
	}
	if shown := get("hide=false"); shown.Total != 122 || shown.Hidden != 1 {
		t.Errorf("hide=false: total %d, hidden %d; want 122 and 1", shown.Total, shown.Hidden)
	}
	if hit := get("q=LATE"); hit.Total != 1 || hit.Tags[0].Name != "latest" {
		t.Errorf("q=LATE = %d tags, total %d, want latest alone", len(hit.Tags), hit.Total)
	}
	if onlyHidden := get("q=.sig"); onlyHidden.Total != 0 || onlyHidden.Hidden != 1 || len(onlyHidden.Tags) != 0 {
		t.Errorf("q=.sig = %d tags, total %d, hidden %d, want nothing shown and one hidden", len(onlyHidden.Tags), onlyHidden.Total, onlyHidden.Hidden)
	}
}

func TestBrowseTagsRefusesAParameterOutOfRange(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	for name, query := range map[string]string{
		"negative page": "page=-1",
		"huge page":     "page=100000",
		"long filter":   "q=" + strings.Repeat("a", 129),
		"hide not bool": "hide=maybe",
	} {
		if rec := f.browse(t, admin, reg, "acme/app", query); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422", name, rec.Code, rec.Body)
		}
	}
}

// The details of the tags on screen (plan 05-19, IMG-03, D-20, D-24a).

type tagDetailsResp struct {
	Items []struct {
		Tag       string  `json:"tag"`
		Digest    string  `json:"digest"`
		Created   *string `json:"created"`
		Code      string  `json:"code"`
		Platforms []struct {
			OS      string `json:"os"`
			Arch    string `json:"arch"`
			Variant string `json:"variant"`
			Digest  string `json:"digest"`
			Size    int64  `json:"size"`
		} `json:"platforms"`
		RunningIn []struct {
			EnvID   string `json:"envId"`
			EnvName string `json:"envName"`
		} `json:"runningIn"`
	} `json:"items"`
}

// imageServer makes the registry serve one single-platform image, acme/app:1.0, and count the
// manifest requests it gets.
type imageServer struct {
	manifestRequests atomic.Int32
	digest           string
	configDigest     string
}

func serveImage(reg *fakeRegistry, created time.Time) *imageServer {
	cfg := []byte(`{"architecture":"arm64","os":"linux","created":"` + created.UTC().Format(time.RFC3339) + `"}`)
	sum := func(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
	is := &imageServer{configDigest: sum(cfg)}
	man := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` +
		is.configDigest + `","size":` + strconv.Itoa(len(cfg)) + `},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"` + sum([]byte("layer")) + `","size":2048}]}`)
	is.digest = sum(man)
	reg.extra = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+reg.srv.URL+`/token",service="fake",scope="repository:acme/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/1.0"), strings.HasSuffix(r.URL.Path, "/manifests/"+is.digest):
			is.manifestRequests.Add(1)
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", is.digest)
			w.Header().Set("Content-Length", strconv.Itoa(len(man)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(man)
			}
		case strings.HasSuffix(r.URL.Path, "/blobs/"+is.configDigest):
			w.Header().Set("Content-Length", strconv.Itoa(len(cfg)))
			_, _ = w.Write(cfg)
		default:
			http.NotFound(w, r)
		}
	}
	return is
}

func (f *authFixture) details(t *testing.T, u credentialUser, reg *fakeRegistry, repo, envID string, tags ...string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"ref": reg.host() + "/" + repo, "tags": tags}
	if envID != "" {
		body["envId"] = envID
	}
	b, _ := json.Marshal(body)
	return f.credCall(t, u, http.MethodPost, "/api/registries/tags/details", string(b))
}

func TestBrowseTagDetailsGiveTheDigestDateAndPlatformsOfTheTagsAsked(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	img := serveImage(reg, created)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, reg.credentialBody("fake-inst"))

	rec := f.details(t, admin, reg, "acme/app", "", "1.0", "gone")
	if rec.Code != http.StatusOK {
		t.Fatalf("details: %d %s", rec.Code, rec.Body)
	}
	got := decodeRec[tagDetailsResp](t, rec)
	if len(got.Items) != 2 {
		t.Fatalf("items = %+v", got.Items)
	}
	one := got.Items[0]
	if one.Tag != "1.0" || one.Digest != img.digest || one.Code != "" || one.Created == nil || *one.Created != "2026-02-03T04:05:06Z" {
		t.Errorf("1.0 = %+v, want digest %s and the date of the config", one, img.digest)
	}
	if len(one.Platforms) != 1 || one.Platforms[0].Arch != "arm64" || one.Platforms[0].OS != "linux" || one.Platforms[0].Size != 2048 || one.Platforms[0].Digest != img.digest {
		t.Errorf("platforms = %+v", one.Platforms)
	}
	if got.Items[1].Tag != "gone" || got.Items[1].Code != "tag_not_found" || got.Items[1].Created != nil {
		t.Errorf("gone = %+v, want tag_not_found", got.Items[1])
	}
	if strings.Contains(rec.Body.String(), reg.pass) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the answer carries the secret or is cacheable: %s %v", rec.Body, rec.Header())
	}
	if actions, _ := f.registryAudit(t); !slices.Equal(actions, []string{"registry.credential_created"}) {
		t.Errorf("registry audit actions = %v; reading details is not audited", actions)
	}
}

func TestBrowseTagDetailsRefusesZeroTagsMoreThan25AndAnUnparsableReference(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	serveImage(reg, time.Now())
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	many := make([]string, 26)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	if rec := f.details(t, admin, reg, "acme/app", "", many...); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("26 tags: %d %s, want 422", rec.Code, rec.Body)
	}
	if rec := f.details(t, admin, reg, "acme/app", ""); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("no tags: %d %s, want 422", rec.Code, rec.Body)
	}
	if rec := f.details(t, admin, reg, "acme/app", "", many[:25]...); rec.Code != http.StatusOK {
		t.Errorf("25 tags: %d %s, want 200", rec.Code, rec.Body)
	}
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/tags/details", `{"ref":"NOT A REF!!","tags":["1.0"]}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ref_invalid" {
		t.Errorf("invalid: %d %s, want 422 ref_invalid", rec.Code, rec.Body)
	}
}

func TestBrowseTagDetailsAuthoriseExactlyAsBrowseTagsBeforeAnythingIsRead(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	img := serveImage(reg, time.Now())
	f.trusting(t, reg)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")
	f.storeCredential(t, credentialUser{f.cookie, f.csrf}, reg.credentialBody("fake-inst"))

	for name, tc := range map[string]struct {
		u     credentialUser
		envID string
		want  int
	}{
		"no grant at all":      {credentialUser{nobodyCookie, nobodyCSRF}, "", http.StatusForbidden},
		"no grant, with envId": {credentialUser{nobodyCookie, nobodyCSRF}, a.ID, http.StatusForbidden},
		"viewer of A on B":     {viewerA, b.ID, http.StatusForbidden},
		"viewer of A on A":     {viewerA, a.ID, http.StatusOK},
		"viewer of A":          {viewerA, "", http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			before := img.manifestRequests.Load()
			rec := f.details(t, tc.u, reg, "acme/app", tc.envID, "1.0")
			if rec.Code != tc.want {
				t.Errorf("details: %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if tc.want == http.StatusForbidden && img.manifestRequests.Load() != before {
				t.Error("a refused caller reached the registry")
			}
		})
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'permission.denied' AND target_id = 'POST /api/registries/tags/details'`).Scan(&n); err != nil || n != 3 {
		t.Errorf("refusals audited = %d, %v, want 3", n, err)
	}
}

func TestBrowseTagDetailsNeverServeWhatACredentialSawToACallerWhoCannotUseIt(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	img := serveImage(reg, time.Now())
	f.trusting(t, reg)
	admin := credentialUser{f.cookie, f.csrf}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+b.ID+"/registries", reg.credentialBody("fake-in-b")); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")

	rec := f.details(t, admin, reg, "acme/app", b.ID, "1.0")
	if rec.Code != http.StatusOK || decodeRec[tagDetailsResp](t, rec).Items[0].Digest != img.digest {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body)
	}
	// The viewer of A has no credential that applies: the registry is asked anonymously and
	// refuses, and nothing the admin's credential saw is in the answer.
	rec = f.details(t, viewerA, reg, "acme/app", "", "1.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer: %d %s", rec.Code, rec.Body)
	}
	item := decodeRec[tagDetailsResp](t, rec).Items[0]
	if item.Code != "needs_login" || item.Digest != "" || item.Created != nil || len(item.Platforms) != 0 {
		t.Errorf("viewer item = %+v, want needs_login and nothing the credential saw", item)
	}
}

// Running in (plan 05-19 task 2, IMG-03, D-22, T-05-64).

func TestBrowseTagDetailsSayWhichReadableEnvironmentsRunTheDigestAndNoOthers(t *testing.T) {
	reg := newFakeRegistry(t)
	img := serveImage(reg, time.Now())
	image := reg.host() + "/acme/app"
	f := newEnvFix(t, dockertest.FakeConfig{
		Hostname: "node-1",
		Images:   []dockertest.FakeImage{{ID: hexID("a"), RepoTags: []string{image + ":1.0"}, RepoDigests: []string{image + "@" + img.digest}}},
		Containers: []dockertest.FakeContainer{
			{ID: "c-web", Names: []string{"/web"}, Image: image + ":1.0", ImageID: hexID("a"), State: "running"},
		},
	})
	f.trusting(t, reg)
	a := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(a.ID).State == envs.StateOnline })
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	bEnv, rec := f.addAgent(t)
	if rec.code != http.StatusCreated {
		t.Fatalf("add the agent environment: %d %s", rec.code, rec.body)
	}
	b := bEnv.ID
	admin := credentialUser{f.cookie, f.csrf}
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")
	viewerB := f.asRoleOn(t, "viewerb", b, "viewer")

	running := func(u credentialUser, envID string) []string {
		t.Helper()
		rec := f.details(t, u, reg, "acme/app", envID, "1.0")
		if rec.Code != http.StatusOK {
			t.Fatalf("details: %d %s", rec.Code, rec.Body)
		}
		var names []string
		for _, e := range decodeRec[tagDetailsResp](t, rec).Items[0].RunningIn {
			if e.EnvID == "" {
				t.Errorf("running in %+v has no environment ID", e)
			}
			names = append(names, e.EnvName)
		}
		return names
	}
	if got := running(admin, ""); !slices.Equal(got, []string{"node-1"}) {
		t.Errorf("admin: running in %v, want node-1; the offline environment is skipped", got)
	}
	if got := running(viewerA, ""); !slices.Equal(got, []string{"node-1"}) {
		t.Errorf("viewer of A: running in %v, want node-1", got)
	}
	if got := running(viewerB, ""); len(got) != 0 {
		t.Errorf("viewer of B: running in %v, want none: A is not theirs to read", got)
	}
	// With an environment chosen the page is that environment's, not the registries page.
	if got := running(admin, a.ID); len(got) != 0 {
		t.Errorf("with envId: running in %v, want none", got)
	}
}

func TestBrowseTagDetailsMatchByDigestSoAMovedTagStillShowsWhereItsImageRuns(t *testing.T) {
	reg := newFakeRegistry(t)
	img := serveImage(reg, time.Now())
	image := reg.host() + "/acme/app"
	f := newEnvFix(t, dockertest.FakeConfig{
		Hostname: "node-1",
		Images:   []dockertest.FakeImage{{ID: hexID("a"), RepoDigests: []string{image + "@" + img.digest}}},
		// The container was created from 0.9, which has since been re-pushed as something else:
		// only its digest still says what it runs.
		Containers: []dockertest.FakeContainer{
			{ID: "c-web", Names: []string{"/web"}, Image: image + ":0.9", ImageID: hexID("a"), State: "running"},
		},
	})
	f.trusting(t, reg)
	a := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(a.ID).State == envs.StateOnline })
	rec := f.details(t, credentialUser{f.cookie, f.csrf}, reg, "acme/app", "", "1.0")
	if rec.Code != http.StatusOK {
		t.Fatalf("details: %d %s", rec.Code, rec.Body)
	}
	if got := decodeRec[tagDetailsResp](t, rec).Items[0].RunningIn; len(got) != 1 || got[0].EnvID != a.ID {
		t.Errorf("running in %+v, want environment A", got)
	}
}
