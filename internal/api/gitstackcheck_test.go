// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Checking a repository folder before a stack exists (plan 09-12, GIT-08, D-09). The router, policy, CSRF guard,
// vault and store are the real ones; the repository is a real git server and the mirror a real bare repository.

const gitCheckCompose = `services:
  web:
    image: nginx:1.27
    environment:
      FROM_TABLE: ${FROM_TABLE}
      FROM_ENV: ${FROM_ENV}
      WITH_DEFAULT: ${WITH_DEFAULT:-dflt}
      NEEDED: ${NEEDED}
      DB_PASSWORD: ${DB_PASSWORD}
  worker:
    image: busybox:1.36
`

const gitCheckSecret = "committed-secret-9f3a"

type gitCheckVar struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Source     string `json:"source"`
	Required   bool   `json:"required"`
	Secret     bool   `json:"secret"`
	SecretLike bool   `json:"secretLike"`
	Unused     bool   `json:"unused"`
}

type gitCheckAnswer struct {
	Commit struct {
		SHA  string `json:"sha"`
		When string `json:"when"`
	} `json:"commit"`
	Services []string `json:"services"`
	Problems []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		File     string `json:"file"`
		Line     int    `json:"line"`
	} `json:"problems"`
	Vars             []gitCheckVar `json:"vars"`
	Missing          []string      `json:"missing"`
	NotImported      []string      `json:"notImported"`
	NotImportedTotal int           `json:"notImportedTotal"`
}

func (a gitCheckAnswer) variable(t *testing.T, name string) gitCheckVar {
	t.Helper()
	for _, v := range a.Vars {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %s in %+v", name, a.Vars)
	return gitCheckVar{}
}

// checkBody is a check of the fixture's stacks/web folder; mut changes it.
func (g *gitStackFix) checkBody(mut func(map[string]any)) string {
	b := map[string]any{
		"repoId": g.id, "refKind": "branch", "ref": "main", "folder": "stacks/web", "composeFiles": []string{"compose.yaml"},
		"vars": []map[string]any{{"name": "FROM_TABLE", "value": "from-table", "secret": false}},
	}
	if mut != nil {
		mut(b)
	}
	return gitJSON(b)
}

func (g *gitStackFix) check(t *testing.T, u credentialUser, body string) *httptest.ResponseRecorder {
	t.Helper()
	return g.f.credCall(t, u, http.MethodPost, "/api/env/"+g.env+"/git-stacks/check", body)
}

func (g *gitStackFix) checkOK(t *testing.T, body string) (gitCheckAnswer, string) {
	t.Helper()
	rec := g.check(t, g.admin, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var got gitCheckAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got, rec.Body.String()
}

func (g *gitStackFix) rows(t *testing.T) (n int) {
	t.Helper()
	if err := g.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) + (SELECT count(*) FROM stack_snapshots) +
		(SELECT count(*) FROM stack_snapshot_files) + (SELECT count(*) FROM stack_git)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGitStackCheckRoute(t *testing.T) {
	t.Parallel()
	g := newGitStackFix(t)
	g.repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml": gitCheckCompose,
		"stacks/web/.env":         "FROM_TABLE=from-env-file\nFROM_ENV=from-the-env\nDB_PASSWORD=" + gitCheckSecret + "\n",
	}, "vars")

	got, raw := g.checkOK(t, g.checkBody(nil))
	if len(got.Commit.SHA) != 40 || got.Commit.When == "" {
		t.Errorf("commit = %+v, want a full ID with its time", got.Commit)
	}
	if !slices.Equal(got.Services, []string{"web", "worker"}) {
		t.Errorf("services = %v", got.Services)
	}
	for name, want := range map[string]struct {
		source, value string
		required      bool
	}{
		"FROM_TABLE":   {"table", "from-table", false},
		"FROM_ENV":     {"repo-env", "from-the-env", false},
		"WITH_DEFAULT": {"default", "dflt", false},
		"NEEDED":       {"unset", "", true},
	} {
		if v := got.variable(t, name); v.Source != want.source || v.Value != want.value || v.Required != want.required {
			t.Errorf("%s = %+v, want %+v", name, v, want)
		}
	}
	if v := got.variable(t, "DB_PASSWORD"); v.Value != "<hidden>" || !v.SecretLike || v.Source != "repo-env" {
		t.Errorf("DB_PASSWORD = %+v, want a hidden value from the repository's .env", v)
	}
	if strings.Contains(raw, gitCheckSecret) {
		t.Errorf("the secret committed in .env is in the answer: %s", raw)
	}
	var blocked, warned bool
	for _, p := range got.Problems {
		blocked = blocked || (p.Code == "unset_variable" && p.Severity == "error" && strings.HasPrefix(p.Message, "NEEDED "))
		warned = warned || (p.Code == "env_secret_like" && p.Severity == "warning")
	}
	if !blocked || !warned {
		t.Errorf("problems = %+v, want NEEDED blocking and DB_PASSWORD warned", got.Problems)
	}
	if got.Missing == nil || got.NotImported == nil {
		t.Errorf("a list is null: %s", raw)
	}

	// Nothing was saved: no stack, draft, revision or file, and the engine saw no write.
	if n := g.rows(t); n != 0 {
		t.Errorf("a check left %d rows", n)
	}
	if n := len(g.f.fake.ServiceCreates()); n != 0 {
		t.Errorf("the engine was sent %d service creates", n)
	}

	// Only stacks.create may check.
	for role, want := range map[string]int{"viewer": http.StatusForbidden, "operator": http.StatusForbidden, "admin": http.StatusOK} {
		u := g.f.asRoleOn(t, "u-"+role, g.env, role)
		if rec := g.check(t, u, g.checkBody(nil)); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", role, rec.Code, rec.Body, want)
		}
	}
	// The repository layer rule holds here as for a create.
	if rec := g.check(t, g.admin, g.checkBody(func(b map[string]any) { b["repoId"] = "nope" })); rec.Code != http.StatusNotFound || problemType(t, rec) != "repository_not_found" {
		t.Errorf("an unknown repository: %d %s", rec.Code, rec.Body)
	}
}
