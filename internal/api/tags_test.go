// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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
