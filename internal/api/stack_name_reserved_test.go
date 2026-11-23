package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/gitx/gittest"
	"github.com/ianglennon/tierplan/internal/stacks"
)

const reservedName = "tierplan-ingress"

func TestCreateStackNameReservedOnStandalone(t *testing.T) {
	t.Parallel()
	t.Run("a standalone host refuses it and writes nothing", func(t *testing.T) {
		t.Parallel()
		f, e, _ := standaloneEnv(t, nil)
		admin := f.caller(t, e, "envadmin-1", "admin")
		rec := admin.createStack(`{"name":"` + reservedName + `"}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_name_reserved" {
			t.Fatalf("create: %d %s, want 422 stack_name_reserved", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), stacks.NameReservedText) {
			t.Errorf("the refusal does not carry the one sentence: %s", rec.Body)
		}
		if n := f.count(t, "SELECT COUNT(*) FROM stacks"); n != 0 {
			t.Errorf("a refused create left %d stack rows", n)
		}
		// A near name is a plain valid name.
		if rec := admin.createStack(`{"name":"tierplan-ingress-2"}`); rec.Code != http.StatusCreated {
			t.Errorf("tierplan-ingress-2: %d %s, want 201", rec.Code, rec.Body)
		}
	})
	t.Run("a swarm is not held to it", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		admin := f.caller(t, e, "envadmin-1", "admin")
		if rec := admin.createStack(`{"name":"` + reservedName + `"}`); rec.Code != http.StatusCreated {
			t.Errorf("create on a swarm: %d %s, want 201", rec.Code, rec.Body)
		}
	})
}

func TestStackNameCheckReserved(t *testing.T) {
	t.Parallel()
	t.Run("a standalone host answers invalid with the one sentence", func(t *testing.T) {
		t.Parallel()
		f, e, _ := standaloneEnv(t, nil)
		admin := f.caller(t, e, "envadmin-1", "admin")
		code, got, rec := nameCheck(t, admin, reservedName)
		if code != http.StatusOK || got.Status != "invalid" || got.Message != stacks.NameReservedText {
			t.Errorf("name-check %s = %d %s, want invalid with NameReservedText", reservedName, code, rec.Body)
		}
		code, got, rec = nameCheck(t, admin, "tierplan-ingress-2")
		if code != http.StatusOK || got.Status != "available" {
			t.Errorf("name-check tierplan-ingress-2 = %d %s, want available", code, rec.Body)
		}
	})
	t.Run("a swarm answers available", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnv(t, nil)
		admin := f.caller(t, e, "envadmin-1", "admin")
		code, got, rec := nameCheck(t, admin, reservedName)
		if code != http.StatusOK || got.Status != "available" {
			t.Errorf("name-check %s on a swarm = %d %s, want available", reservedName, code, rec.Body)
		}
	})
}

func TestGitImportNameReserved(t *testing.T) {
	t.Parallel()
	t.Run("a standalone host refuses the create", func(t *testing.T) {
		t.Parallel()
		g := newStandaloneGitFix(t)
		rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["name"] = reservedName }))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_name_reserved" {
			t.Fatalf("git create: %d %s, want 422 stack_name_reserved", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), stacks.NameReservedText) {
			t.Errorf("the refusal does not carry the one sentence: %s", rec.Body)
		}
		var n int
		if err := g.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stacks) + (SELECT count(*) FROM stack_git)`).Scan(&n); err != nil || n != 0 {
			t.Errorf("a refused create left %d rows, %v", n, err)
		}
	})
	t.Run("the source check does not use the reserved name as its working name", func(t *testing.T) {
		t.Parallel()
		g := newStandaloneGitFix(t)
		rec := g.f.credCall(t, g.admin, http.MethodPost, "/api/env/"+g.env+"/git-stacks/check",
			g.body(func(b map[string]any) { b["name"] = reservedName }))
		if rec.Code != http.StatusOK {
			t.Errorf("the source check: %d %s, want 200 (the reserved name is the create's to refuse)", rec.Code, rec.Body)
		}
	})
	t.Run("a swarm is not held to it", func(t *testing.T) {
		t.Parallel()
		g := newGitStackFix(t)
		rec := g.create(t, g.admin, g.body(func(b map[string]any) { b["name"] = reservedName }))
		if rec.Code != http.StatusCreated {
			t.Errorf("git create on a swarm: %d %s, want 201", rec.Code, rec.Body)
		}
	})
}

// newStandaloneGitFix is newGitStackFix on an environment that is a standalone host.
func newStandaloneGitFix(t *testing.T) *gitStackFix {
	t.Helper()
	f, e, _ := standaloneEnv(t, nil)
	admin := f.asAdmin(t)
	repo := gittest.NewRepo(t)
	sha := repo.Commit(t, map[string]string{
		"stacks/web/compose.yaml": gitWebCompose,
		"stacks/web/.env":         "TAG=1\n",
		"stacks/web/web.env":      "A=b\n",
	}, "first")
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	g := &gitStackFix{f: f, e: e, env: e.ID, admin: admin, srv: srv, repo: repo, sha: sha}
	rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+e.ID+"/git/repos", gitJSON(map[string]any{"name": "acme", "url": srv.URL, "caPem": srv.CAPEM}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("save the repository: %d %s", rec.Code, rec.Body)
	}
	g.id = decodeGitRepo(t, rec).ID
	return g
}
