// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
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
