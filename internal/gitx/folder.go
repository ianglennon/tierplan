// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitx

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The limits ReadFolder applies when it is given none: the file and stack sizes of a stack of any source.
const (
	DefaultMaxFileBytes  = 1 << 20
	DefaultMaxTotalBytes = 8 << 20
	// DefaultMaxEntries is how many entries of a folder ReadFolder looks at before it gives up: a folder is a
	// stack, and one with this many files is not.
	DefaultMaxEntries = 100000
	// DefaultMaxOthers is how many names of files that were not read are returned (the total is always counted).
	DefaultMaxOthers = 50
)

// ReadLimits bound what ReadFolder reads. A zero field takes its default.
type ReadLimits struct {
	// MaxFileBytes is the most one file may hold.
	MaxFileBytes int64
	// MaxTotalBytes is the most all the files read may hold together.
	MaxTotalBytes int64
	// MaxEntries is how many entries of the folder are looked at.
	MaxEntries int
	// MaxOthers is how many names of unread files are returned.
	MaxOthers int
}

func (l ReadLimits) withDefaults() ReadLimits {
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultMaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxOthers <= 0 {
		l.MaxOthers = DefaultMaxOthers
	}
	return l
}

// LimitError is a folder that breaks a ReadFolder limit: one file over MaxFileBytes (Path names it) or the files
// together over MaxTotalBytes or more entries than MaxEntries (Path is empty).
type LimitError struct {
	Path string
}

func (e *LimitError) Error() string {
	if e.Path != "" {
		return "gitx: " + e.Path + " is over the file size limit"
	}
	return "gitx: the folder is over the size limit"
}

// FolderFile is one entry of a folder that was asked for. A regular file has its content. A symlink has its target
// as Content and is never followed; a submodule has no content.
type FolderFile struct {
	// Path is relative to the folder, with forward slashes.
	Path    string
	Content []byte
	Kind    EntryKind
}

// Folder is what ReadFolder found in a folder of a commit.
type Folder struct {
	// Tree is the hash of the folder's tree object (of the commit's root tree for the root folder).
	Tree string
	// Files are the entries want accepted that the folder holds, sorted by path.
	Files []FolderFile
	// Others are the paths of the files that want did not accept, sorted, at most MaxOthers of them; OthersTotal is
	// how many there are.
	Others      []string
	OthersTotal int
	// Links are the symlinks and submodules anywhere in the folder, whether or not want accepted them, by path. A
	// link is never followed, so a file behind one is not in Files.
	Links map[string]Link
}

// Link is a symlink or a submodule found in a folder.
type Link struct {
	Kind EntryKind
	// Target is where a symlink points, as the repository records it; empty for a submodule and for a target over
	// maxLinkTarget bytes.
	Target string
}

// maxLinkTarget is the longest symlink target ReadFolder reports.
const maxLinkTarget = 4096

// FolderTree answers the hash of the tree object of the folder dir ("" is the root) of the commit sha in the mirror
// m: the commit's root tree for the root. It changes when any file under the folder changes and not otherwise, so
// two commits with the same answer hold the same folder. A folder the commit does not hold is folder_missing and a
// commit the mirror does not hold is commit_missing.
func (g *GoGit) FolderTree(ctx context.Context, m MirrorID, sha, dir string) (string, error) {
	var tree string
	err := g.withCommitTree(ctx, m, sha, dir, func(t *object.Tree) error {
		tree = t.Hash.String()
		return nil
	})
	return tree, err
}

// ReadFolder reads the files of the folder dir of the commit sha in the mirror m, from the object database by the
// commit's ID. want is asked about every file with its path relative to the folder; the files it accepts are read
// into Folder.Files and the others are only counted and named. A symlink or a submodule is reported in
// Folder.Links and, when want accepts it, in Files without being followed. Nothing is written to disk.
func (g *GoGit) ReadFolder(ctx context.Context, m MirrorID, sha, dir string, want func(rel string) bool, limits ReadLimits) (Folder, error) {
	limits = limits.withDefaults()
	out := Folder{Links: map[string]Link{}}
	var repo *git.Repository
	err := g.withCommitTreeRepo(ctx, m, sha, dir, func(r *git.Repository, t *object.Tree) error {
		repo = r
		out.Tree = t.Hash.String()
		walker := object.NewTreeWalker(t, true, nil)
		defer walker.Close()
		var seen int
		var total int64
		for {
			if err := ctx.Err(); err != nil {
				return Classify(err, "")
			}
			name, entry, err := walker.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return &Error{Code: CodeUnreachable}
			}
			kind := kindOf(entry.Mode)
			if kind == EntryDir {
				continue
			}
			if seen++; seen > limits.MaxEntries {
				return &LimitError{}
			}
			if kind != EntryFile {
				link := Link{Kind: kind}
				if kind == EntrySymlink {
					link.Target = readLinkTarget(repo, entry.Hash)
				}
				out.Links[name] = link
			}
			if !want(name) {
				out.OthersTotal++
				if len(out.Others) < limits.MaxOthers {
					out.Others = append(out.Others, name)
				}
				continue
			}
			file := FolderFile{Path: name, Kind: kind}
			switch kind {
			case EntrySubmodule:
				// Nothing to read: the commit the submodule names is not in this repository.
			default:
				blob, err := repo.BlobObject(entry.Hash)
				if err != nil {
					return &Error{Code: CodeFileMissing}
				}
				if blob.Size > limits.MaxFileBytes {
					return &LimitError{Path: name}
				}
				if total += blob.Size; total > limits.MaxTotalBytes {
					return &LimitError{}
				}
				content, err := readBlob(blob, limits.MaxFileBytes)
				if err != nil {
					return err
				}
				file.Content = content
			}
			out.Files = append(out.Files, file)
		}
	})
	if err != nil {
		return Folder{}, err
	}
	sortFolder(&out)
	return out, nil
}

// readLinkTarget reads the target of the symlink whose blob is h, or "" when it cannot be read or is too long.
func readLinkTarget(repo *git.Repository, h plumbing.Hash) string {
	blob, err := repo.BlobObject(h)
	if err != nil || blob.Size > maxLinkTarget {
		return ""
	}
	data, err := readBlob(blob, maxLinkTarget)
	if err != nil {
		return ""
	}
	return string(data)
}

// readBlob reads a blob of at most max bytes.
func readBlob(blob *object.Blob, max int64) ([]byte, error) {
	r, err := blob.Reader()
	if err != nil {
		return nil, &Error{Code: CodeFileMissing}
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, &Error{Code: CodeFileMissing}
	}
	if int64(len(data)) > max {
		return nil, &LimitError{}
	}
	return data, nil
}

func sortFolder(f *Folder) {
	slices.Sort(f.Others)
	slices.SortFunc(f.Files, func(a, b FolderFile) int { return strings.Compare(a.Path, b.Path) })
}

// withCommitTree runs fn with the tree of the folder dir of the commit sha, holding the mirror's read lock.
func (g *GoGit) withCommitTree(ctx context.Context, m MirrorID, sha, dir string, fn func(*object.Tree) error) error {
	return g.withCommitTreeRepo(ctx, m, sha, dir, func(_ *git.Repository, t *object.Tree) error { return fn(t) })
}

func (g *GoGit) withCommitTreeRepo(ctx context.Context, m MirrorID, sha, dir string, fn func(*git.Repository, *object.Tree) error) error {
	if err := ctx.Err(); err != nil {
		return Classify(err, "")
	}
	path, err := g.mirrorPath(m)
	if err != nil {
		return err
	}
	mu := g.lockFor(m)
	mu.RLock()
	defer mu.RUnlock()

	repo, err := openExisting(path)
	if errors.Is(err, errMirrorMissing) {
		return &Error{Code: CodeCommitMissing}
	}
	if err != nil {
		g.log.Error("could not open a git mirror", "error", err)
		return &Error{Code: CodeUnreachable}
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return &Error{Code: CodeCommitMissing}
	}
	tree, err := commit.Tree()
	if err != nil {
		return &Error{Code: CodeCommitMissing}
	}
	dir = strings.Trim(dir, "/")
	if dir != "" {
		e, err := tree.FindEntry(dir)
		if err != nil || e.Mode != filemode.Dir {
			return &Error{Code: CodeFolderMissing}
		}
		if tree, err = tree.Tree(dir); err != nil {
			return &Error{Code: CodeFolderMissing}
		}
	}
	return fn(repo, tree)
}
