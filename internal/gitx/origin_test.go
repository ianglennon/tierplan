// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/gitx/gittest"
)

// A mirror belongs to the origin it was filled from (WR-A05): a repository pointed at another address, or at another
// certificate authority or sign-in, starts again, so a commit pinned from the old one cannot be read through the new.

func TestFetchFromAnotherOriginStartsTheMirrorAgain(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()
	if _, err := f.g.FetchRef(ctx, f.m, f.remote(), gitx.RefBranch, "main"); err != nil {
		t.Fatalf("FetchRef: %v", err)
	}
	if _, err := f.g.ReadDir(ctx, f.m, f.sha, "", 0); err != nil {
		t.Fatalf("ReadDir of the commit just fetched: %v", err)
	}

	// The same repository ID now points at another server, which has never heard of that commit.
	other := gittest.NewRepo(t)
	other.Commit(t, map[string]string{"compose.yaml": "services: {other: {image: y}}\n"}, "other")
	srv := gittest.NewHTTPServer(t, other, gittest.HTTPOptions{TLS: true})
	_, err := f.g.FetchRef(ctx, f.m, gitx.Remote{URL: srv.URL, CAPEM: srv.CAPEM}, gitx.RefCommit, f.sha)
	if ge := asError(t, err); ge.Code != gitx.CodeCommitMissing {
		t.Errorf("asking the new address for the old repository's commit = %v, want commit_missing", err)
	}
	if _, err := f.g.ReadDir(ctx, f.m, f.sha, "", 0); err == nil {
		t.Error("the old repository's commit can still be read through the mirror of the new address")
	}
}

func TestMirrorIsKeptForTheSameOriginAndResetForAnotherIdentity(t *testing.T) {
	t.Parallel()
	f := newBrowseFixture(t, gittest.HTTPOptions{})
	ctx := context.Background()
	marker := filepath.Join(f.mirrorDir(), "marker")
	fetch := func(r gitx.Remote) {
		t.Helper()
		if _, err := f.g.FetchRef(ctx, f.m, r, gitx.RefBranch, "main"); err != nil {
			t.Fatalf("FetchRef: %v", err)
		}
	}
	mark := func() {
		t.Helper()
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	marked := func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}
	fetch(f.remote())

	mark()
	fetch(f.remote())
	if !marked() {
		t.Error("a second fetch from the same origin started the mirror again")
	}
	// A new token for the same user is the same origin: the mirror is a cache of the same repository.
	asUser := f.remote()
	asUser.Auth = gitx.TokenAuth{Username: "ci", Token: "one"}
	fetch(asUser)
	mark()
	asUser.Auth = gitx.TokenAuth{Username: "ci", Token: "two"}
	fetch(asUser)
	if !marked() {
		t.Error("a rotated token of the same user started the mirror again")
	}

	for name, r := range map[string]gitx.Remote{
		"another user":     {URL: f.srv.URL, CAPEM: f.srv.CAPEM, Auth: gitx.TokenAuth{Username: "someone-else", Token: "two"}},
		"another CA":       {URL: f.srv.URL, CAPEM: f.srv.CAPEM + "\n"},
		"no longer signed": {URL: f.srv.URL, CAPEM: f.srv.CAPEM},
	} {
		mark()
		fetch(r)
		if marked() {
			t.Errorf("%s: the mirror was kept", name)
		}
		fetch(asUser) // back to a known identity for the next case
	}
}
