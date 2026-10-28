// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Importing a stack from a repository folder (plan 09-10, GIT-02, D-27): the files the compose files need and
// nothing else, read from a real git server through a real mirror.

func TestMain(m *testing.M) {
	gitx.InstallTransport(registry.NewEgressPolicy(nil))
	os.Exit(m.Run())
}

const webCompose = `services:
  web:
    image: nginx:1.27
    env_file: web.env
`

type fixture struct {
	svc   *gitsync.Service
	conns *gitconn.Store
	st    *store.Store
	repo  *gittest.Repo
	srv   *gittest.HTTPServer
	sha   string
	id    string
}

// newFixture serves a repository whose folder stacks/web is a stack, with files around it that are not.
func newFixture(t *testing.T, files map[string]string, prepare func(*gittest.Repo)) *fixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	if prepare != nil {
		prepare(repo)
	}
	sha := repo.Commit(t, files, "first")
	srv := gittest.NewHTTPServer(t, repo, gittest.HTTPOptions{TLS: true})
	st := storetest.New(t)
	v, err := vault.New(bytes.Repeat([]byte{5}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	conns := gitconn.NewStore(st, v, nil)
	client := gitx.NewGoGit(gitx.Options{Root: t.TempDir()})
	f := &fixture{
		svc:   gitsync.New(gitsync.Deps{Git: client, Conns: conns, Repo: stackops.Repo{Store: st, Vault: v}}),
		conns: conns, st: st, repo: repo, srv: srv, sha: sha,
	}
	f.id = f.addRepo(t, nil, "acme")
	return f
}

func (f *fixture) addRepo(t *testing.T, env *string, name string) string {
	t.Helper()
	r, err := f.conns.CreateRepo(context.Background(), gitconn.RepoInput{EnvID: env, Name: name, URL: f.srv.URL, CAPEM: f.srv.CAPEM}, "admin", nil)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return r.ID
}

func (f *fixture) request() gitsync.ImportRequest {
	return gitsync.ImportRequest{
		EnvID: "env-1", RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "stacks/web",
		ComposeFiles: []string{"compose.yaml"}, Name: "web", Target: stacks.TargetStandalone,
	}
}

func webFiles() map[string]string {
	return map[string]string{
		"stacks/web/compose.yaml":  webCompose,
		"stacks/web/.env":          "TAG=1\n",
		"stacks/web/web.env":       "A=b\n",
		"stacks/web/README.md":     "# web\n",
		"stacks/web/docs/a.md":     "a\n",
		"stacks/other/compose.yml": "services: {}\n",
	}
}

func paths(files []stacks.File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

func errorsOf(ps []stacks.Problem) []stacks.Problem {
	var out []stacks.Problem
	for _, p := range ps {
		if p.Severity == stacks.SeverityError {
			out = append(out, p)
		}
	}
	return out
}

func TestImportReferencedOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)

	imp, err := f.svc.Import(context.Background(), f.request())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := paths(imp.Files); !slices.Equal(got, []string{".env", "compose.yaml", "web.env"}) {
		t.Errorf("files = %v, want the compose file, the .env and the env_file, and no other", got)
	}
	for _, file := range imp.Files {
		if file.Origin != stacks.OriginRepo || file.Secret {
			t.Errorf("file %+v, want a plain repository file", file)
		}
	}
	if !slices.Equal(imp.NotImported, []string{"README.md", "docs/a.md"}) || imp.NotImportedTotal != 2 {
		t.Errorf("not imported = %v (%d), want README.md and docs/a.md", imp.NotImported, imp.NotImportedTotal)
	}
	if imp.Commit.SHA != f.sha || imp.Tree != f.repo.Tree(t, f.sha, "stacks/web") {
		t.Errorf("commit %s tree %s, want %s and the folder's tree", imp.Commit.SHA, imp.Tree, f.sha)
	}
	want := stackops.GitSource{RepoID: f.id, RepoName: "acme", RefKind: "branch", Ref: "main", Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}}
	if imp.Source.RepoID != want.RepoID || imp.Source.RepoName != want.RepoName || imp.Source.RefKind != want.RefKind || imp.Source.Ref != want.Ref ||
		imp.Source.Folder != want.Folder || !slices.Equal(imp.Source.ComposeFiles, want.ComposeFiles) {
		t.Errorf("source = %+v, want %+v", imp.Source, want)
	}
	if errs := errorsOf(imp.Problems); len(errs) != 0 {
		t.Errorf("problems = %+v, want none that block", errs)
	}
	copy := imp.Copy([]stacks.Var{{Name: "X", Value: "1"}})
	if !slices.Equal(copy.ComposeFiles, []string{"compose.yaml"}) || len(copy.Vars) != 1 || len(copy.Files) != 3 {
		t.Errorf("copy = %+v", copy)
	}
	if rev := imp.Revision(); rev.Commit != f.sha || rev.Tree != imp.Tree {
		t.Errorf("revision = %+v", rev)
	}
}

func TestImportWithoutADotEnvStoresNone(t *testing.T) {
	t.Parallel()
	files := webFiles()
	delete(files, "stacks/web/.env")
	f := newFixture(t, files, nil)
	imp, err := f.svc.Import(context.Background(), f.request())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := paths(imp.Files); !slices.Equal(got, []string{"compose.yaml", "web.env"}) {
		t.Errorf("files = %v, want no .env", got)
	}
	if errs := errorsOf(imp.Problems); len(errs) != 0 {
		t.Errorf("problems = %+v, want none that block: a missing .env is fine", errs)
	}
}

func TestImportRootFolderAndSeveralComposeFilesInOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"compose.yaml":      "services:\n  web:\n    image: nginx:1.27\n    secrets: [token]\nsecrets:\n  token:\n    file: ./token.txt\n",
		"compose.prod.yaml": "services:\n  web:\n    image: nginx:1.28\n",
		"token.txt":         "s3cr3t-value\n",
		"unrelated/x":       "x\n",
	}, nil)
	req := f.request()
	req.Folder, req.ComposeFiles = "", []string{"./compose.yaml", "compose.prod.yaml"}
	imp, err := f.svc.Import(context.Background(), req)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !slices.Equal(imp.Source.ComposeFiles, []string{"compose.yaml", "compose.prod.yaml"}) || imp.Source.Folder != "" {
		t.Errorf("source = %+v, want the two compose files in order at the root", imp.Source)
	}
	if imp.Tree != f.repo.Tree(t, f.sha, "") {
		t.Errorf("tree = %s, want the commit's root tree", imp.Tree)
	}
	if got := paths(imp.Files); !slices.Equal(got, []string{"compose.prod.yaml", "compose.yaml", "token.txt"}) {
		t.Errorf("files = %v", got)
	}
	for _, file := range imp.Files {
		if want := file.Path == "token.txt"; file.Secret != want {
			t.Errorf("file %s secret = %v, want %v: a secrets: file is stored sealed", file.Path, file.Secret, want)
		}
	}
	if !slices.Equal(imp.NotImported, []string{"unrelated/x"}) {
		t.Errorf("not imported = %v", imp.NotImported)
	}
}

func TestImportRefusesAComposeFileTheFolderLacks(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	req := f.request()
	req.ComposeFiles = []string{"compose.yaml", "compose.prod.yaml"}
	_, err := f.svc.Import(context.Background(), req)
	var pe *gitsync.PathError
	if !errors.Is(err, gitsync.ErrComposeFileMissing) || !errors.As(err, &pe) || pe.Path != "compose.prod.yaml" {
		t.Errorf("err = %v, want ErrComposeFileMissing naming compose.prod.yaml", err)
	}
}

func TestImportRefusesBadLocationsBeforeAnyNetworkCall(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	before := f.srv.Requests()
	cases := map[string]struct {
		mut  func(*gitsync.ImportRequest)
		want error
	}{
		"folder with ..":          {func(r *gitsync.ImportRequest) { r.Folder = "stacks/../.." }, gitsync.ErrFolderInvalid},
		"folder with a backslash": {func(r *gitsync.ImportRequest) { r.Folder = `stacks\web` }, gitsync.ErrFolderInvalid},
		"no compose file":         {func(r *gitsync.ImportRequest) { r.ComposeFiles = nil }, gitsync.ErrNoComposeFiles},
		"compose path with ..":    {func(r *gitsync.ImportRequest) { r.ComposeFiles = []string{"../compose.yaml"} }, gitsync.ErrComposePathInvalid},
		"absolute compose path":   {func(r *gitsync.ImportRequest) { r.ComposeFiles = []string{"/etc/passwd"} }, gitsync.ErrComposePathInvalid},
		"compose file twice":      {func(r *gitsync.ImportRequest) { r.ComposeFiles = []string{"compose.yaml", "./compose.yaml"} }, gitsync.ErrComposePathInvalid},
	}
	for name, c := range cases {
		req := f.request()
		c.mut(&req)
		if _, err := f.svc.Import(context.Background(), req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if got := f.srv.Requests(); got != before {
		t.Errorf("the git server saw %d requests for refused input, want none", got-before)
	}
}

func TestImportRepositoryOfAnotherEnvironmentIsNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('env-1', 'agent', 'one', '', 1, 1, 1), ('env-2', 'agent', 'two', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	other := "env-2"
	foreign := f.addRepo(t, &other, "theirs")

	req := f.request()
	req.RepoID = foreign
	if _, err := f.svc.Import(context.Background(), req); !errors.Is(err, gitconn.ErrNotFound) {
		t.Errorf("another environment's repository: err = %v, want ErrNotFound", err)
	}
	req.EnvID = "env-2"
	if _, err := f.svc.Import(context.Background(), req); err != nil {
		t.Errorf("its own environment's repository: %v", err)
	}
	// The instance repository (f.id) is usable from every environment.
	req.RepoID, req.EnvID = f.id, "env-1"
	if _, err := f.svc.Import(context.Background(), req); err != nil {
		t.Errorf("an instance repository: %v", err)
	}
}

func TestImportAFixedCommitRecordsItsFullID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	req := f.request()
	req.RefKind, req.Ref = gitx.RefCommit, f.sha[:10]
	imp, err := f.svc.Import(context.Background(), req)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imp.Source.RefKind != "commit" || imp.Source.Ref != f.sha || imp.Commit.SHA != f.sha {
		t.Errorf("source = %+v, want the full commit ID as the ref", imp.Source)
	}
}

func TestImportGitFailuresKeepTheirCode(t *testing.T) {
	t.Parallel()
	f := newFixture(t, webFiles(), nil)
	var ge *gitx.Error

	req := f.request()
	req.Ref = "no-such-branch"
	if _, err := f.svc.Import(context.Background(), req); !errors.As(err, &ge) || ge.Code != gitx.CodeRefMissing {
		t.Errorf("a branch that is not there: err = %v, want ref_missing", err)
	}
	req = f.request()
	req.Folder = "stacks/nope"
	if _, err := f.svc.Import(context.Background(), req); !errors.As(err, &ge) || ge.Code != gitx.CodeFolderMissing {
		t.Errorf("a folder that is not there: err = %v, want folder_missing", err)
	}
	req = f.request()
	req.Ref = "-x"
	if _, err := f.svc.Import(context.Background(), req); !errors.Is(err, gitx.ErrRefInvalid) {
		t.Errorf("a ref git refuses: err = %v, want ErrRefInvalid", err)
	}
}

func TestImportLinksAndSubmodulesAreBlockingProblemsAndNotStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"stacks/web/compose.yaml": "services:\n  web:\n    image: nginx:1.27\n    env_file: [out.env, lib/x.env, ok.env]\n",
		"stacks/web/ok.env":       "A=b\n",
	}, func(r *gittest.Repo) {
		r.Symlink(t, "stacks/web/out.env", "../../../outside/secret.env")
		r.Submodule(t, "stacks/web/lib")
	})
	imp, err := f.svc.Import(context.Background(), f.request())
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if got := paths(imp.Files); !slices.Equal(got, []string{"compose.yaml", "ok.env"}) {
		t.Errorf("files = %v, want neither the link nor what is behind the submodule", got)
	}
	codes := map[string]string{}
	for _, p := range imp.Problems {
		codes[p.Code] = p.File
	}
	if codes[stacks.CodeSymlinkOutside] != "out.env" || codes[stacks.CodeSubmodule] != "lib" {
		t.Errorf("problems = %+v, want git_symlink_outside on out.env and git_submodule on lib", imp.Problems)
	}
	for _, p := range imp.Problems {
		if (p.Code == stacks.CodeSymlinkOutside || p.Code == stacks.CodeSubmodule) && p.Severity != stacks.SeverityError {
			t.Errorf("problem %+v does not block", p)
		}
	}
}

func TestImportRefusesAFileOverTheLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"stacks/web/compose.yaml": webCompose,
		"stacks/web/web.env":      strings.Repeat("A=b\n", 300000),
	}, nil)
	_, err := f.svc.Import(context.Background(), f.request())
	var le *gitx.LimitError
	if !errors.As(err, &le) || le.Path != "web.env" {
		t.Errorf("err = %v, want a LimitError naming web.env", err)
	}
}
