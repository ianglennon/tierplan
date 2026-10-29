// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/gitx"
)

// Repositories no git client would make, written straight into a mirror: a server can send any objects, and go-git
// checks neither that the trees of a commit are all there nor that they are sane (WR-A02, WR-A03).

type hostileMirror struct {
	g   *gitx.GoGit
	m   gitx.MirrorID
	sha string
	st  *git.Repository
}

func newHostileMirror(t *testing.T, build func(h *hostileMirror) plumbing.Hash) *hostileMirror {
	t.Helper()
	root := t.TempDir()
	h := &hostileMirror{g: gitx.NewGoGit(gitx.Options{Root: root}), m: gitx.MirrorID(uuid.NewString())}
	repo, err := git.PlainInit(filepath.Join(root, string(h.m)+".git"), true)
	if err != nil {
		t.Fatal(err)
	}
	h.st = repo
	tree := build(h)
	commit := &object.Commit{
		Author:    object.Signature{Name: "a", Email: "a@example.invalid", When: time.Unix(1700000000, 0).UTC()},
		Committer: object.Signature{Name: "a", Email: "a@example.invalid", When: time.Unix(1700000000, 0).UTC()},
		Message:   "hostile", TreeHash: tree,
	}
	h.sha = h.store(t, commit).String()
	return h
}

func (h *hostileMirror) store(t *testing.T, o interface {
	Encode(plumbing.EncodedObject) error
}) plumbing.Hash {
	t.Helper()
	obj := h.st.Storer.NewEncodedObject()
	if err := o.Encode(obj); err != nil {
		t.Fatal(err)
	}
	hash, err := h.st.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func (h *hostileMirror) blob(t *testing.T, content string) plumbing.Hash {
	t.Helper()
	obj := h.st.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := h.st.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func all(string) bool { return true }

// A tree that names a subtree the mirror does not hold: go-git's walker ends the walk there without an error, so
// the files after it would silently be missing.
func TestReadFolderAMissingSubtreeIsAnErrorNotASmallerFolder(t *testing.T) {
	t.Parallel()
	h := newHostileMirror(t, func(h *hostileMirror) plumbing.Hash {
		return h.store(t, &object.Tree{Entries: []object.TreeEntry{
			{Name: "a-dir", Mode: filemode.Dir, Hash: plumbing.NewHash("1111111111111111111111111111111111111111")},
			{Name: "z.txt", Mode: filemode.Regular, Hash: h.blob(t, "z\n")},
		}})
	})
	_, err := h.g.ReadFolder(context.Background(), h.m, h.sha, "", all, gitx.ReadLimits{})
	if ge := asError(t, err); ge.Code != gitx.CodeCommitMissing {
		t.Errorf("error = %v, want commit_missing so the mirror is fetched again", err)
	}
}

// k trees, each with ten entries that are all the tree below it, and an empty tree at the bottom: 10^k folders and no
// file. Counting files alone never stops this walk.
func TestReadFolderATreeThatFansOutWithoutFilesHitsTheEntryLimit(t *testing.T) {
	t.Parallel()
	h := newHostileMirror(t, func(h *hostileMirror) plumbing.Hash {
		below := h.store(t, &object.Tree{})
		for range 30 {
			var entries []object.TreeEntry
			for _, n := range []string{"d0", "d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9"} {
				entries = append(entries, object.TreeEntry{Name: n, Mode: filemode.Dir, Hash: below})
			}
			below = h.store(t, &object.Tree{Entries: entries})
		}
		return below
	})
	_, err := h.g.ReadFolder(context.Background(), h.m, h.sha, "", all, gitx.ReadLimits{MaxEntries: 1000})
	var le *gitx.LimitError
	if !errors.As(err, &le) {
		t.Errorf("error = %v, want the entry limit", err)
	}
}

// A folder shared in two places is two folders to the reader: the files under both are named.
func TestReadFolderASharedSubtreeIsReadWhereverItIs(t *testing.T) {
	t.Parallel()
	h := newHostileMirror(t, func(h *hostileMirror) plumbing.Hash {
		shared := h.store(t, &object.Tree{Entries: []object.TreeEntry{{Name: "x.env", Mode: filemode.Regular, Hash: h.blob(t, "A=1\n")}}})
		return h.store(t, &object.Tree{Entries: []object.TreeEntry{
			{Name: "one", Mode: filemode.Dir, Hash: shared},
			{Name: "two", Mode: filemode.Dir, Hash: shared},
		}})
	})
	got, err := h.g.ReadFolder(context.Background(), h.m, h.sha, "", all, gitx.ReadLimits{})
	if err != nil {
		t.Fatalf("ReadFolder: %v", err)
	}
	if len(got.Files) != 2 || got.Files[0].Path != "one/x.env" || got.Files[1].Path != "two/x.env" {
		t.Errorf("files = %+v, want one/x.env and two/x.env", got.Files)
	}
}
