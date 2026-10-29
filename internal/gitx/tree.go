// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// defaultDirLimit is how many entries ReadDir returns when it is not given a limit.
const defaultDirLimit = 500

// composeNames are the file names that make a folder look like a stack.
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// ReadDir implements Client. It reads the object database by commit ID under the read lock and
// touches no working tree, so a file whose name or link target is hostile is only ever reported.
func (g *GoGit) ReadDir(ctx context.Context, m MirrorID, sha, dir string, limit int) (Dir, error) {
	if err := ctx.Err(); err != nil {
		return Dir{}, Classify(err, "")
	}
	path, err := g.mirrorPath(m)
	if err != nil {
		return Dir{}, err
	}
	if limit <= 0 {
		limit = defaultDirLimit
	}
	mu := g.lockFor(m)
	mu.RLock()
	defer mu.RUnlock()

	repo, err := openExisting(path)
	if errors.Is(err, errMirrorMissing) {
		return Dir{}, &Error{Code: CodeCommitMissing}
	}
	if err != nil {
		g.log.Error("could not open a git mirror", "error", err)
		return Dir{}, &Error{Code: CodeUnreachable}
	}
	g.markUsed(m, path)
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return Dir{}, &Error{Code: CodeCommitMissing}
	}
	tree, err := commit.Tree()
	if err != nil {
		return Dir{}, &Error{Code: CodeCommitMissing}
	}
	dir = strings.Trim(dir, "/")
	if dir != "" {
		e, err := tree.FindEntry(dir)
		if err != nil || e.Mode != filemode.Dir {
			return Dir{}, &Error{Code: CodeFolderMissing}
		}
		if tree, err = tree.Tree(dir); err != nil {
			return Dir{}, &Error{Code: CodeFolderMissing}
		}
	}
	out := Dir{Path: dir, Total: len(tree.Entries)}
	for _, e := range tree.Entries {
		out.Entries = append(out.Entries, Entry{Name: e.Name, Kind: kindOf(e.Mode)})
	}
	slices.SortFunc(out.Entries, func(a, b Entry) int {
		if (a.Kind == EntryDir) != (b.Kind == EntryDir) {
			if a.Kind == EntryDir {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(out.Entries) > limit {
		out.Entries, out.Capped = out.Entries[:limit], true
	}
	for i, e := range out.Entries {
		if e.Kind != EntryDir {
			continue
		}
		sub, err := tree.Tree(e.Name)
		if err != nil {
			continue
		}
		out.Entries[i].HasCompose = holdsCompose(sub)
	}
	return out, nil
}

// kindOf maps a git file mode to an entry kind.
func kindOf(m filemode.FileMode) EntryKind {
	switch m {
	case filemode.Dir:
		return EntryDir
	case filemode.Symlink:
		return EntrySymlink
	case filemode.Submodule:
		return EntrySubmodule
	}
	return EntryFile
}

// holdsCompose reports whether a folder directly holds a compose file (a file, not a folder or a link).
func holdsCompose(t *object.Tree) bool {
	for _, e := range t.Entries {
		if kindOf(e.Mode) == EntryFile && slices.Contains(composeNames, e.Name) {
			return true
		}
	}
	return false
}
