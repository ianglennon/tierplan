// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/gitx/gittest"
)

// Git credentials and repositories (plan 09-04, GIT-01, D-01, D-02, D-04). The router, policy, CSRF
// guard, vault, audit writer and rate limit are the real ones; the repository is a real git server.

// gitToken is a made-up token of 12 or more characters, so its last 4 are kept.
const gitToken = "glpat-SECRETtoken0123456789WXYZ" //nolint:gosec // G101: a made-up token for the test

type gitCredResp struct {
	ID    string `json:"id"`
	Scope struct {
		Kind    string  `json:"kind"`
		EnvID   *string `json:"envId"`
		EnvName *string `json:"envName"`
	} `json:"scope"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Username    string  `json:"username"`
	Last4       string  `json:"last4"`
	PublicKey   string  `json:"publicKey"`
	Fingerprint string  `json:"fingerprint"`
	UsedBy      int     `json:"usedBy"`
	ReadOnly    bool    `json:"readOnly"`
	LastUseAt   *string `json:"lastUseAt"`
}

type gitRepoResp struct {
	ID    string `json:"id"`
	Scope struct {
		Kind  string  `json:"kind"`
		EnvID *string `json:"envId"`
	} `json:"scope"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Host       string `json:"host"`
	Credential *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Scope struct {
			Kind string `json:"kind"`
		} `json:"scope"`
	} `json:"credential"`
	CAPEM    string `json:"caPem"`
	HasCA    bool   `json:"hasCa"`
	ReadOnly bool   `json:"readOnly"`
	UsedBy   int    `json:"usedBy"`
	LastTest *struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	} `json:"lastTest"`
}

func decodeGitCred(t *testing.T, rec *httptest.ResponseRecorder) gitCredResp {
	t.Helper()
	var c gitCredResp
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return c
}

func decodeGitRepo(t *testing.T, rec *httptest.ResponseRecorder) gitRepoResp {
	t.Helper()
	var r gitRepoResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

func gitJSON(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// securedRepo is a repository served over TLS that wants the basic-auth user bot and gitToken.
func securedRepo(t *testing.T) *gittest.HTTPServer {
	t.Helper()
	return publicRepo(t, gittest.HTTPOptions{BasicAuth: &[2]string{"bot", gitToken}})
}

func TestGitRepoSaveAndTest(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	srv := securedRepo(t)
	admin := f.asAdmin(t)

	credRec := f.credCall(t, admin, http.MethodPost, "/api/git/credentials",
		gitJSON(map[string]any{"name": "gitea-homelab", "type": "https", "username": "bot", "secret": gitToken}))
	if credRec.Code != http.StatusCreated {
		t.Fatalf("create credential: %d %s", credRec.Code, credRec.Body)
	}
	cred := decodeGitCred(t, credRec)
	if cred.Scope.Kind != "instance" || cred.Type != "https" || cred.Username != "bot" || cred.Last4 != "WXYZ" || cred.UsedBy != 0 {
		t.Errorf("credential = %+v", cred)
	}

	// Saving never runs or waits for a test (D-04): the repository is saved and untested.
	repoRec := f.credCall(t, admin, http.MethodPost, "/api/git/repos",
		gitJSON(map[string]any{"name": "app", "url": srv.URL, "credentialId": cred.ID, "caPem": srv.CAPEM}))
	if repoRec.Code != http.StatusCreated {
		t.Fatalf("create repository: %d %s", repoRec.Code, repoRec.Body)
	}
	repo := decodeGitRepo(t, repoRec)
	if repo.Credential == nil || repo.Credential.ID != cred.ID || repo.Credential.Name != "gitea-homelab" || repo.LastTest != nil || !repo.HasCA {
		t.Errorf("repository = %+v, want it to name its credential and have no test yet", repo)
	}
	if srv.Requests() != 0 {
		t.Errorf("saving a repository made %d requests to the git server, want none", srv.Requests())
	}

	// A repository with no credential picked does not get one by address: it is refused.
	anon := decodeGitRepo(t, f.credCall(t, admin, http.MethodPost, "/api/git/repos",
		gitJSON(map[string]any{"name": "anon", "url": srv.URL, "caPem": srv.CAPEM})))
	rec := f.credCall(t, admin, http.MethodPost, "/api/git/repos/"+anon.ID+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test of the repository with no credential: %d %s", rec.Code, rec.Body)
	}
	if got := decodeGitTest(t, rec); got.Failure == nil || got.Failure.Code != "auth_refused" {
		t.Errorf("a repository with no credential picked was tested as %+v, want auth_refused (nothing is inferred from the address)", got)
	}

	// Testing the saved repository signs in with its credential and records the outcome.
	rec = f.credCall(t, admin, http.MethodPost, "/api/git/repos/"+repo.ID+"/test", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	got := decodeGitTest(t, rec)
	if got.Failure != nil || got.Steps.Reachable.State != "passed" || got.Steps.SignedIn.State != "passed" || got.Steps.Refs.State != "passed" {
		t.Errorf("test = %+v, want all three steps passed", got)
	}
	if got.BranchCount != 2 || got.TagCount != 2 || got.DefaultBranch != "main" {
		t.Errorf("counts %d/%d default %q, want 2/2 and main", got.BranchCount, got.TagCount, got.DefaultBranch)
	}

	// The saved test shows on the repository, and the credential knows it signed in.
	envID := f.add(t).ID
	one := f.credCall(t, admin, http.MethodGet, "/api/env/"+envID+"/git/repos/"+repo.ID, "")
	if one.Code != http.StatusOK {
		t.Fatalf("get: %d %s", one.Code, one.Body)
	}
	read := decodeGitRepo(t, one)
	if read.LastTest == nil || !read.LastTest.OK || read.LastTest.Code != "" {
		t.Errorf("repository after its test = %+v, want a passing last test", read)
	}

	// No body ever carried the token, and no audit row does.
	for i, body := range []string{one.Body.String(), credRec.Body.String(), repoRec.Body.String(), rec.Body.String()} {
		if strings.Contains(body, gitToken) || strings.Contains(body, "SECRETtoken") {
			t.Errorf("response %d carries the token: %s", i, body)
		}
	}
	for _, action := range []string{"git.credential_created", "git.repo_created", "git.repo_tested"} {
		rows := f.auditRows(t, action)
		if len(rows) == 0 {
			t.Errorf("%s was not audited", action)
		}
		for _, r := range rows {
			for k, v := range r.Details {
				if strings.Contains(v, "SECRETtoken") || strings.Contains(k, "secret") {
					t.Errorf("%s audit detail %s=%s carries the secret", action, k, v)
				}
			}
		}
	}
}

type gitProblem struct {
	Type   string `json:"type"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func decodeGitProblem(t *testing.T, rec *httptest.ResponseRecorder) gitProblem {
	t.Helper()
	var p gitProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p
}

// wantFieldProblem fails unless rec is a 422 naming field with code.
func wantFieldProblem(t *testing.T, what string, rec *httptest.ResponseRecorder, field, code string) {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("%s: %d %s, want 422", what, rec.Code, rec.Body)
		return
	}
	for _, e := range decodeGitProblem(t, rec).Errors {
		if e.Location == "body."+field && e.Message == code {
			return
		}
	}
	t.Errorf("%s: %s, want body.%s: %s", what, rec.Body, field, code)
}

func (f *envFix) mustCreate(t *testing.T, u credentialUser, path, body string) (id string) {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST %s %s: %d %s", path, body, rec.Code, rec.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.ID
}

func httpsCred(name string) string {
	return gitJSON(map[string]any{"name": name, "type": "https", "username": "bot", "secret": gitToken})
}

func gitRepoJSON(name, credID string) string {
	m := map[string]any{"name": name, "url": "https://git.example.org/acme/" + name + ".git"}
	if credID != "" {
		m["credentialId"] = credID
	}
	return gitJSON(m)
}

func listGitCreds(t *testing.T, f *envFix, u credentialUser, path string) map[string]gitCredResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var list struct {
		Items []gitCredResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]gitCredResp{}
	for _, c := range list.Items {
		out[c.Name] = c
	}
	return out
}

func listGitRepos(t *testing.T, f *envFix, u credentialUser, path string) map[string]gitRepoResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var list struct {
		Items []gitRepoResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]gitRepoResp{}
	for _, r := range list.Items {
		out[r.Name] = r
	}
	return out
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestGitConnLayers(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	admin := f.asAdmin(t)

	inst := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("inst-cred"))
	inA := f.mustCreate(t, admin, "/api/env/"+a.ID+"/git/credentials", httpsCred("a-cred"))
	inB := f.mustCreate(t, admin, "/api/env/"+b.ID+"/git/credentials", httpsCred("b-cred"))
	f.mustCreate(t, admin, "/api/git/repos", gitRepoJSON("inst-repo", inst))
	f.mustCreate(t, admin, "/api/env/"+a.ID+"/git/repos", gitRepoJSON("a-repo", inA))
	f.mustCreate(t, admin, "/api/env/"+a.ID+"/git/repos", gitRepoJSON("a-with-inst", inst))
	bRepo := f.mustCreate(t, admin, "/api/env/"+b.ID+"/git/repos", gitRepoJSON("b-repo", inB))

	// D-02: an environment repository uses its own environment's credential or an instance one; an
	// instance repository only an instance one; nothing else.
	wantFieldProblem(t, "environment A repository with B's credential",
		f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/git/repos", gitRepoJSON("steal", inB)), "credentialId", "credential_scope")
	wantFieldProblem(t, "instance repository with an environment credential",
		f.credCall(t, admin, http.MethodPost, "/api/git/repos", gitRepoJSON("steal", inA)), "credentialId", "credential_scope")
	wantFieldProblem(t, "a credential that does not exist",
		f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/git/repos", gitRepoJSON("steal", "no-such-credential")), "credentialId", "credential_scope")
	wantFieldProblem(t, "a test of A's address with B's credential",
		f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/git/repos/test", gitJSON(map[string]any{"url": "https://git.example.org/x.git", "credentialId": inB})),
		"credentialId", "credential_scope")

	envAdminA := f.asRoleOn(t, "admina", a.ID, "admin")
	viewerA := f.asRoleOn(t, "viewera", a.ID, "viewer")
	envAdminB := f.asRoleOn(t, "adminb", b.ID, "admin")
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	nobody := credentialUser{nobodyCookie, nobodyCSRF}

	type want struct {
		credentials map[string]bool // name -> readOnly
		repos       map[string]bool
	}
	for name, tc := range map[string]struct {
		u    credentialUser
		want want
	}{
		"instance admin": {admin, want{
			map[string]bool{"inst-cred": false, "a-cred": false, "b-cred": false},
			map[string]bool{"inst-repo": false, "a-repo": false, "a-with-inst": false, "b-repo": false}}},
		"admin of A": {envAdminA, want{
			map[string]bool{"inst-cred": true, "a-cred": false},
			map[string]bool{"inst-repo": true, "a-repo": false, "a-with-inst": false}}},
		"viewer of A": {viewerA, want{
			map[string]bool{"inst-cred": true, "a-cred": true},
			map[string]bool{"inst-repo": true, "a-repo": true, "a-with-inst": true}}},
		"admin of B": {envAdminB, want{
			map[string]bool{"inst-cred": true, "b-cred": false},
			map[string]bool{"inst-repo": true, "b-repo": false}}},
		"no role anywhere": {nobody, want{map[string]bool{}, map[string]bool{}}},
	} {
		creds := listGitCreds(t, f, tc.u, "/api/git/credentials")
		if got := keysOf(creds); !slices.Equal(got, keysOf(tc.want.credentials)) {
			t.Errorf("%s sees credentials %v, want %v", name, got, keysOf(tc.want.credentials))
		}
		for n, ro := range tc.want.credentials {
			if creds[n].ReadOnly != ro {
				t.Errorf("%s: credential %s readOnly = %v, want %v", name, n, creds[n].ReadOnly, ro)
			}
		}
		repos := listGitRepos(t, f, tc.u, "/api/git/repos")
		if got := keysOf(repos); !slices.Equal(got, keysOf(tc.want.repos)) {
			t.Errorf("%s sees repositories %v, want %v", name, got, keysOf(tc.want.repos))
		}
		for n, ro := range tc.want.repos {
			if repos[n].ReadOnly != ro {
				t.Errorf("%s: repository %s readOnly = %v, want %v", name, n, repos[n].ReadOnly, ro)
			}
		}
	}

	// usedBy counts the repositories that picked the credential.
	creds := listGitCreds(t, f, admin, "/api/git/credentials")
	if creds["inst-cred"].UsedBy != 2 || creds["a-cred"].UsedBy != 1 || creds["b-cred"].UsedBy != 1 {
		t.Errorf("usedBy = %d/%d/%d, want 2/1/1", creds["inst-cred"].UsedBy, creds["a-cred"].UsedBy, creds["b-cred"].UsedBy)
	}

	// An environment's route lists its own rows and the instance's, never another environment's.
	onA := listGitCreds(t, f, envAdminA, "/api/env/"+a.ID+"/git/credentials")
	if got := keysOf(onA); !slices.Equal(got, []string{"a-cred", "inst-cred"}) || !onA["inst-cred"].ReadOnly || onA["a-cred"].ReadOnly {
		t.Errorf("environment A's credentials = %v (inst readOnly %v)", got, onA["inst-cred"].ReadOnly)
	}
	reposOnA := listGitRepos(t, f, envAdminA, "/api/env/"+a.ID+"/git/repos")
	if got := keysOf(reposOnA); !slices.Equal(got, []string{"a-repo", "a-with-inst", "inst-repo"}) || !reposOnA["inst-repo"].ReadOnly {
		t.Errorf("environment A's repositories = %v (inst-repo readOnly %v)", got, reposOnA["inst-repo"].ReadOnly)
	}
	if rec := f.credCall(t, envAdminB, http.MethodGet, "/api/env/"+a.ID+"/git/credentials", ""); rec.Code != http.StatusNotFound {
		t.Errorf("B's admin reading A's credentials: %d %s, want 404", rec.Code, rec.Body)
	}

	// An environment's Admin manages only that environment: not the instance layer, not another one.
	if rec := f.credCall(t, envAdminA, http.MethodPost, "/api/git/credentials", httpsCred("nope")); rec.Code != http.StatusForbidden {
		t.Errorf("env admin adding an instance credential: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, viewerA, http.MethodPost, "/api/env/"+a.ID+"/git/credentials", httpsCred("nope")); rec.Code != http.StatusForbidden {
		t.Errorf("viewer adding a credential: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdminA, http.MethodPut, "/api/env/"+a.ID+"/git/credentials/"+inst, httpsCred("hijack")); rec.Code != http.StatusNotFound {
		t.Errorf("env admin changing the instance credential through its route: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdminA, http.MethodDelete, "/api/env/"+a.ID+"/git/credentials/"+inB, ""); rec.Code != http.StatusNotFound {
		t.Errorf("env admin deleting B's credential through its own route: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdminA, http.MethodGet, "/api/env/"+a.ID+"/git/repos/"+bRepo, ""); rec.Code != http.StatusNotFound {
		t.Errorf("A's admin reading B's repository through A's route: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestGitConnUpdateRules(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := f.asAdmin(t)
	cred := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("gitea"))
	other := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("other"))
	repo := f.mustCreate(t, admin, "/api/git/repos", gitRepoJSON("app", cred))

	// PUT with no secret keeps the saved one: the answer still shows its last 4, and the name changed.
	rec := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+cred, gitJSON(map[string]any{"name": "gitea-2", "type": "https", "username": "bot"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT without a secret: %d %s", rec.Code, rec.Body)
	}
	if got := decodeGitCred(t, rec); got.Name != "gitea-2" || got.Last4 != "WXYZ" {
		t.Errorf("credential = %+v, want the new name and the saved token's last 4", got)
	}
	// A clash within the layer is name_taken.
	if rec := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+other, gitJSON(map[string]any{"name": "gitea-2", "type": "https", "username": "bot"})); rec.Code != http.StatusConflict ||
		decodeGitProblem(t, rec).Type != "name_taken" {
		t.Errorf("rename onto another credential: %d %s, want 409 name_taken", rec.Code, rec.Body)
	}
	// The instance route cannot reach an environment's credential, nor the environment route the instance's.
	envCred := f.mustCreate(t, admin, "/api/env/"+a.ID+"/git/credentials", httpsCred("a-cred"))
	if rec := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+envCred, httpsCred("x")); rec.Code != http.StatusNotFound {
		t.Errorf("instance route to an environment credential: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPut, "/api/env/"+a.ID+"/git/credentials/"+cred, httpsCred("x")); rec.Code != http.StatusNotFound {
		t.Errorf("environment route to an instance credential: %d %s, want 404", rec.Code, rec.Body)
	}

	// A repository whose address moves to another host keeps its credential only when the request
	// names the credential again.
	moved := func(credID *string) string {
		m := map[string]any{"name": "app", "url": "https://elsewhere.example.net/acme/app.git"}
		if credID != nil {
			m["credentialId"] = *credID
		}
		return gitJSON(m)
	}
	wantFieldProblem(t, "a new host without naming the credential",
		f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, moved(nil)), "credentialId", "credential_reselect")
	rec = f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, moved(&cred))
	if rec.Code != http.StatusOK {
		t.Fatalf("a new host naming the credential: %d %s", rec.Code, rec.Body)
	}
	if got := decodeGitRepo(t, rec); got.Host != "elsewhere.example.net" || got.Credential == nil || got.Credential.ID != cred {
		t.Errorf("repository = %+v, want the new host and the same credential", got)
	}
	// The same host keeps the credential without naming it.
	rec = f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, gitJSON(map[string]any{"name": "app-2", "url": "https://elsewhere.example.net/acme/two.git"}))
	if rec.Code != http.StatusOK || decodeGitRepo(t, rec).Credential == nil {
		t.Errorf("same host, no credentialId: %d %s, want the credential kept", rec.Code, rec.Body)
	}
	// An empty credentialId picks none; a credential outside the layer is refused.
	rec = f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, gitJSON(map[string]any{"name": "app-2", "url": "https://elsewhere.example.net/acme/two.git", "credentialId": ""}))
	if rec.Code != http.StatusOK || decodeGitRepo(t, rec).Credential != nil {
		t.Errorf("empty credentialId: %d %s, want no credential", rec.Code, rec.Body)
	}
	wantFieldProblem(t, "an instance repository picking an environment credential",
		f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, gitJSON(map[string]any{"name": "app-2", "url": "https://elsewhere.example.net/acme/two.git", "credentialId": envCred})),
		"credentialId", "credential_scope")
	wantFieldProblem(t, "an address with a password in it",
		f.credCall(t, admin, http.MethodPut, "/api/git/repos/"+repo, gitJSON(map[string]any{"name": "app-2", "url": "https://u:p@elsewhere.example.net/x.git"})), "url", "url_userinfo") //nolint:gosec // G101: an address with a made-up password, to be refused
}

func TestGitCredentialInUse(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := f.asAdmin(t)
	cred := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("shared"))
	one := f.mustCreate(t, admin, "/api/git/repos", gitRepoJSON("one", cred))
	f.mustCreate(t, admin, "/api/env/"+a.ID+"/git/repos", gitRepoJSON("two", cred))

	rec := f.credCall(t, admin, http.MethodDelete, "/api/git/credentials/"+cred, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE of a credential used by 2 repositories: %d %s, want 409", rec.Code, rec.Body)
	}
	p := decodeGitProblem(t, rec)
	if p.Type != "credential_in_use" || len(p.Errors) != 1 || p.Errors[0].Location != "conflict.count" || p.Errors[0].Message != "2" {
		t.Errorf("problem = %s, want credential_in_use with count 2", rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodDelete, "/api/git/repos/"+one, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE repository: %d %s", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodDelete, "/api/git/credentials/"+cred, "")
	if rec.Code != http.StatusConflict || decodeGitProblem(t, rec).Errors[0].Message != "1" {
		t.Errorf("DELETE with one repository left: %d %s, want 409 with count 1", rec.Code, rec.Body)
	}
	// Deleting the environment's repository through the wrong route finds nothing.
	if rec := f.credCall(t, admin, http.MethodDelete, "/api/env/"+a.ID+"/git/repos/"+one, ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE of an instance repository through an environment route: %d %s, want 404", rec.Code, rec.Body)
	}
}

func TestGitConnAudit(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	srv := securedRepo(t)
	admin := f.asAdmin(t)
	envAdmin := f.asRoleOn(t, "admina", a.ID, "admin")

	cred := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("gitea"))
	const rotated = "glpat-ROTATEDtoken9876543210" //nolint:gosec // G101: a made-up token for the test
	if rec := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+cred,
		gitJSON(map[string]any{"name": "gitea", "type": "https", "username": "bot", "secret": rotated})); rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	// An environment repository that uses an instance credential: the log shows it.
	repo := f.mustCreate(t, envAdmin, "/api/env/"+a.ID+"/git/repos",
		gitJSON(map[string]any{"name": "app", "url": srv.URL, "credentialId": cred, "caPem": srv.CAPEM}))
	if rec := f.credCall(t, envAdmin, http.MethodPut, "/api/env/"+a.ID+"/git/repos/"+repo,
		gitJSON(map[string]any{"name": "app", "url": "https://elsewhere.example.net/acme/app.git", "credentialId": cred, "caPem": srv.CAPEM})); rec.Code != http.StatusOK {
		t.Fatalf("move: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdmin, http.MethodDelete, "/api/env/"+a.ID+"/git/repos/"+repo, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete repository: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodDelete, "/api/git/credentials/"+cred, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete credential: %d %s", rec.Code, rec.Body)
	}

	for action, want := range map[string]struct {
		env     string
		details map[string]string
	}{
		"git.credential_created": {"", map[string]string{"name": "gitea", "layer": "instance", "type": "https", "username": "bot"}},
		"git.credential_updated": {"", map[string]string{"name": "gitea", "layer": "instance", "changed": "secret"}},
		"git.credential_deleted": {"", map[string]string{"name": "gitea", "layer": "instance", "type": "https"}},
		"git.repo_created":       {a.ID, map[string]string{"name": "app", "layer": "environment", "host": "127.0.0.1", "credential": "gitea", "credential_layer": "instance", "custom_ca": "true"}},
		"git.repo_updated":       {a.ID, map[string]string{"name": "app", "layer": "environment", "host": "elsewhere.example.net", "host_from": "127.0.0.1", "changed": "url", "credential_layer": "instance"}},
		"git.repo_deleted":       {a.ID, map[string]string{"name": "app", "layer": "environment", "host": "elsewhere.example.net", "credential": "gitea"}},
	} {
		rows := f.auditRows(t, action)
		if len(rows) != 1 {
			t.Errorf("%s: %d audit rows, want exactly 1", action, len(rows))
			continue
		}
		if rows[0].EnvID != want.env || rows[0].Result != "success" {
			t.Errorf("%s: env %q result %q, want env %q and success", action, rows[0].EnvID, rows[0].Result, want.env)
		}
		for k, v := range want.details {
			if rows[0].Details[k] != v {
				t.Errorf("%s: detail %s = %q, want %q (all: %v)", action, k, rows[0].Details[k], v, rows[0].Details)
			}
		}
	}

	// No row of the whole log carries a token, a key or a part of one.
	rows, err := f.st.Read.Query(`SELECT COALESCE(details, '{}') || action || COALESCE(reason, '') || COALESCE(target_id, '') FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"SECRETtoken", "ROTATEDtoken", "3210", "WXYZ"} {
			if strings.Contains(text, secret) {
				t.Errorf("an audit row carries %q: %s", secret, text)
			}
		}
	}
}

// deployKey is an unencrypted OpenSSH ed25519 private key, its authorized_keys line and fingerprint,
// and the base64 body of the key, which must never come back.
func deployKey(t *testing.T) (pemText, authorized, fingerprint, body string) {
	t.Helper()
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), ssh.FingerprintSHA256(sshPub),
		strings.Join(strings.Split(strings.TrimSpace(string(pem.EncodeToMemory(block))), "\n")[1:3], "")
}

func TestSSHCredentialBodyHasNoKey(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := f.asAdmin(t)
	key, authorized, fingerprint, body := deployKey(t)

	rec := f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "deploy", "type": "ssh", "secret": key}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c := decodeGitCred(t, rec)
	if c.Type != "ssh" || c.Username != "git" || c.PublicKey != authorized || c.Fingerprint != fingerprint || c.Last4 != "" {
		t.Errorf("credential = %+v, want user git, the public key %q and fingerprint %q", c, authorized, fingerprint)
	}
	repo := f.mustCreate(t, admin, "/api/git/repos", gitJSON(map[string]any{"name": "app", "url": "git@git.example.org:acme/app.git", "credentialId": c.ID}))

	// Nowhere the key is read back: not in the answer to the create, the list, the get, the
	// environment's list, the repository that uses it, nor the audit log.
	var bodies []string
	bodies = append(bodies, rec.Body.String())
	for _, path := range []string{
		"/api/git/credentials", "/api/git/credentials/" + c.ID, "/api/env/" + a.ID + "/git/credentials",
		"/api/env/" + a.ID + "/git/credentials/" + c.ID, "/api/git/repos", "/api/git/repos/" + repo,
	} {
		got := f.credCall(t, admin, http.MethodGet, path, "")
		if got.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, got.Code, got.Body)
		}
		bodies = append(bodies, got.Body.String())
	}
	put := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+c.ID, gitJSON(map[string]any{"name": "deploy-2", "type": "ssh"}))
	if put.Code != http.StatusOK {
		t.Fatalf("PUT without a key: %d %s", put.Code, put.Body)
	}
	if got := decodeGitCred(t, put); got.PublicKey != authorized || got.Fingerprint != fingerprint {
		t.Errorf("after a rename without a key = %+v, want the same public key", got)
	}
	bodies = append(bodies, put.Body.String())
	for i, text := range bodies {
		if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, body) {
			t.Errorf("response %d carries the private key: %s", i, text)
		}
	}
	for _, action := range []string{"git.credential_created", "git.credential_updated"} {
		for _, r := range f.auditRows(t, action) {
			for k, v := range r.Details {
				if strings.Contains(v, "PRIVATE KEY") || strings.Contains(v, body) {
					t.Errorf("%s detail %s carries the private key", action, k)
				}
			}
			if r.Details["fingerprint"] != fingerprint {
				t.Errorf("%s detail fingerprint = %q, want %q", action, r.Details["fingerprint"], fingerprint)
			}
		}
	}

	// The refusals the sheet words, each on body.secret.
	protected := ed25519PEMWithPassphrase(t)
	wantFieldProblem(t, "a key with a passphrase",
		f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "p", "type": "ssh", "secret": protected})), "secret", "key_passphrase")
	wantFieldProblem(t, "garbage",
		f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "g", "type": "ssh", "secret": "not a key"})), "secret", "key_invalid")
	wantFieldProblem(t, "a key over 16 KiB",
		f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "big", "type": "ssh", "secret": key + strings.Repeat("\n#", 17<<10)})), "secret", "key_too_large")
	wantFieldProblem(t, "a missing key",
		f.credCall(t, admin, http.MethodPost, "/api/git/credentials", gitJSON(map[string]any{"name": "none", "type": "ssh"})), "secret", "secret_required")
	// Switching an https credential to a key needs the key typed now.
	h := f.mustCreate(t, admin, "/api/git/credentials", httpsCred("token"))
	wantFieldProblem(t, "https to ssh without a key",
		f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+h, gitJSON(map[string]any{"name": "token", "type": "ssh"})), "secret", "secret_required")
	if rec := f.credCall(t, admin, http.MethodPut, "/api/git/credentials/"+h, gitJSON(map[string]any{"name": "token", "type": "ssh", "secret": key})); rec.Code != http.StatusOK ||
		decodeGitCred(t, rec).Fingerprint != fingerprint {
		t.Errorf("https to ssh with a key: %d %s", rec.Code, rec.Body)
	}
}

func ed25519PEMWithPassphrase(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

// A mirror is only a cache of its repository (plan 09-32, D-08): deleting the repository removes it at once, and nothing
// else is touched.
func TestRepoDeleteRemovesMirror(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	env := f.add(t).ID
	mirrors := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	f.gitClient = mirrors
	f.h = f.build(t)
	admin := f.asAdmin(t)
	b := newBrowseRepoFix(t)
	doomed := b.save(t, f, admin, "/api/git/repos", "doomed")
	kept := b.save(t, f, admin, "/api/git/repos", "kept")
	for _, id := range []string{doomed, kept} {
		if rec := f.credCall(t, admin, http.MethodGet, treeURL(env, id, "branch", "main", ""), ""); rec.Code != http.StatusOK {
			t.Fatalf("browse %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if ids, err := mirrors.ListMirrors(); err != nil || len(ids) != 2 {
		t.Fatalf("mirrors after browsing two repositories = %v, %v, want two", ids, err)
	}

	if rec := f.credCall(t, admin, http.MethodDelete, "/api/git/repos/"+doomed, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	ids, err := mirrors.ListMirrors()
	if err != nil || len(ids) != 1 || string(ids[0]) != kept {
		t.Fatalf("mirrors after the delete = %v, %v, want only the other repository's", ids, err)
	}
}
