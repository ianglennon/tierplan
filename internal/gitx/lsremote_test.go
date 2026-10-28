// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"os"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/internal/registry"
)

// TestMain installs the HTTPS transport once for the package, as the server does at startup.
func TestMain(m *testing.M) {
	gitx.InstallTransport(registry.NewEgressPolicy(nil))
	os.Exit(m.Run())
}

// fixture is a repository with main and dev, a lightweight tag v1 and an annotated tag v2, served
// over TLS.
type fixture struct {
	repo       *gittest.Repo
	srv        *gittest.HTTPServer
	first, dev string
}

func newFixture(t *testing.T, o gittest.HTTPOptions) *fixture {
	t.Helper()
	repo := gittest.NewRepo(t)
	first := repo.Commit(t, map[string]string{"compose.yaml": "services: {}\n"}, "first")
	dev := repo.Commit(t, map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, "second")
	repo.Branch(t, "dev", dev)
	repo.Branch(t, "main", dev)
	repo.Tag(t, "v1", first, false)
	repo.Tag(t, "v2", dev, true)
	// A forge advertises these too; they are not branches or tags.
	repo.Ref(t, "refs/pull/1/head", first)
	return &fixture{repo: repo, srv: gittest.NewHTTPServer(t, repo, o), first: first, dev: dev}
}

func TestLsRemotePeeledAndDefault(t *testing.T) {
	t.Parallel()
	f := newFixture(t, gittest.HTTPOptions{TLS: true})
	g := gitx.NewGoGit(gitx.Options{})

	got, err := g.LsRemote(context.Background(), gitx.Remote{URL: f.srv.URL, CAPEM: f.srv.CAPEM})
	if err != nil {
		t.Fatalf("LsRemote: %v", err)
	}
	want := gitx.RefList{
		Branches: []gitx.Ref{
			{Name: "dev", Kind: gitx.KindBranch, Commit: f.dev},
			{Name: "main", Kind: gitx.KindBranch, Commit: f.dev},
		},
		Tags: []gitx.Ref{
			{Name: "v1", Kind: gitx.KindTag, Commit: f.first},
			{Name: "v2", Kind: gitx.KindTag, Commit: f.dev},
		},
		Default: "main",
	}
	if len(got.Branches) != 2 || len(got.Tags) != 2 {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want.Branches {
		if got.Branches[i] != want.Branches[i] {
			t.Errorf("branch %d = %+v, want %+v", i, got.Branches[i], want.Branches[i])
		}
	}
	for i := range want.Tags {
		if got.Tags[i] != want.Tags[i] {
			t.Errorf("tag %d = %+v, want %+v (an annotated tag is listed at its commit, never its tag object)", i, got.Tags[i], want.Tags[i])
		}
	}
	if got.Default != "main" {
		t.Errorf("Default = %q, want main (the branch HEAD points at)", got.Default)
	}
	if f.srv.Requests() != 1 {
		t.Errorf("the server saw %d ref advertisements, want 1", f.srv.Requests())
	}
}
