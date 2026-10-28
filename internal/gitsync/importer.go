// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitsync

import (
	"context"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

var (
	// ErrFolderInvalid means the folder is not a path inside the repository.
	ErrFolderInvalid = errors.New("gitsync: the folder is not a path inside the repository")
	// ErrNoComposeFiles means no compose file was named.
	ErrNoComposeFiles = errors.New("gitsync: a stack needs at least one compose file")
	// ErrComposePathInvalid means a compose file is not a path inside the folder, or is named twice.
	ErrComposePathInvalid = errors.New("gitsync: a compose file is not a path inside the folder")
	// ErrComposeFileMissing means a compose file is not a regular file in the folder at the commit.
	ErrComposeFileMissing = errors.New("gitsync: the compose file is not in the folder")
)

// PathError names the path an ErrComposePathInvalid or ErrComposeFileMissing is about.
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string { return e.Err.Error() + ": " + e.Path }

// Unwrap lets errors.Is match the sentinel.
func (e *PathError) Unwrap() error { return e.Err }

// ImportRequest is a stack to read from a repository folder.
type ImportRequest struct {
	// EnvID is the environment the stack is for; it decides which repositories may be used.
	EnvID, RepoID string
	RefKind       gitx.RefKind
	Ref           string
	// Folder is the folder of the repository the stack lives in; empty is the root.
	Folder string
	// ComposeFiles are the compose files, relative to the folder, in merge order.
	ComposeFiles []string
	// Name, Target and Vars are the stack's, which the check of the imported files needs.
	Name   string
	Target stacks.Target
	Vars   []stacks.Var
}

// Import is the files of a stack read from a repository folder at one commit.
type Import struct {
	// Commit is the commit the ref resolved to and Tree the hash of the folder's tree at it.
	Commit gitx.Commit
	Tree   string
	// Files are the stack's files, each OriginRepo: the compose files, the folder's .env when it has one and every
	// env_file, secrets and configs file the compose files name that the folder holds.
	Files []stacks.File
	// NotImported are the first names of the folder's other files, which are never stored, and NotImportedTotal how
	// many there are.
	NotImported      []string
	NotImportedTotal int
	// Problems are what the check of the files found, and the links and submodules the stack needs.
	Problems []stacks.Problem
	// Source says where the files came from.
	Source stackops.GitSource
}

// Copy is the working copy of the import with vars as its variables.
func (i Import) Copy(vars []stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{Files: slices.Clone(i.Files), Vars: slices.Clone(vars), ComposeFiles: slices.Clone(i.Source.ComposeFiles)}
}

// Revision is what a snapshot made from the import records.
func (i Import) Revision() stackops.GitRevision {
	return stackops.GitRevision{Commit: i.Commit.SHA, Tree: i.Tree, Source: i.Source}
}

// Import fetches the ref into the repository's mirror and reads the stack out of the commit it resolved to: the
// compose files, the folder's .env and the files the compose files name, and nothing else of the folder (D-27). It
// reads the object database by the commit's ID, so a branch that moves meanwhile changes nothing. A repository of
// another environment is gitconn.ErrNotFound.
func (s *Service) Import(ctx context.Context, req ImportRequest) (Import, error) {
	repo, err := s.conns.GetRepo(ctx, req.RepoID)
	if err != nil {
		return Import{}, err
	}
	if repo.EnvID != "" && repo.EnvID != req.EnvID {
		return Import{}, gitconn.ErrNotFound
	}
	folder, composeFiles, err := cleanLocation(req.Folder, req.ComposeFiles)
	if err != nil {
		return Import{}, err
	}
	rem, err := s.conns.Remote(ctx, repo)
	if err != nil {
		return Import{}, err
	}
	mirror := gitx.MirrorID(repo.ID)
	commit, err := s.git.FetchRef(ctx, mirror, rem, req.RefKind, req.Ref)
	s.recordUse(ctx, repo, err)
	if err != nil {
		return Import{}, err
	}
	tree, err := s.git.FolderTree(ctx, mirror, commit.SHA, folder)
	if err != nil {
		return Import{}, err
	}

	// The compose files and the .env first: they say which other files the stack needs.
	first := map[string]bool{stacks.DotEnvFile: true}
	for _, p := range composeFiles {
		first[p] = true
	}
	got, err := s.git.ReadFolder(ctx, mirror, commit.SHA, folder, func(rel string) bool { return first[rel] }, gitx.ReadLimits{})
	if err != nil {
		return Import{}, err
	}
	var problems stacks.Problems
	copy := stacks.WorkingCopy{Vars: slices.Clone(req.Vars), ComposeFiles: composeFiles}
	for _, f := range got.Files {
		if f.Kind != gitx.EntryFile {
			continue
		}
		copy.Files = append(copy.Files, stacks.File{Path: f.Path, Content: f.Content, Origin: stacks.OriginRepo})
	}
	for _, p := range composeFiles {
		if !slices.ContainsFunc(copy.Files, func(f stacks.File) bool { return f.Path == p }) {
			return Import{}, &PathError{Path: p, Err: ErrComposeFileMissing}
		}
	}

	// The load reads the paths as compose-go interpolates them; a file that does not load names what the raw YAML
	// names, which is all the check needs.
	loaded, _ := stacks.Load(ctx, stacks.LoadInput{Name: req.Name, Target: req.Target, Copy: copy})
	needed := stacks.ReferencedPaths(loaded, copy)
	wanted := make(map[string]bool, len(needed.Paths))
	for _, p := range needed.Paths {
		wanted[p] = true
	}
	all, err := s.git.ReadFolder(ctx, mirror, commit.SHA, folder, func(rel string) bool { return wanted[rel] }, gitx.ReadLimits{})
	if err != nil {
		return Import{}, err
	}
	copy.Files = nil
	for _, f := range all.Files {
		if f.Kind != gitx.EntryFile {
			continue
		}
		copy.Files = append(copy.Files, stacks.File{Path: f.Path, Content: f.Content, Origin: stacks.OriginRepo})
	}
	problems = append(problems, linkProblems(needed.Paths, all)...)
	// The server decides which files are secret: the ones the compose files use under secrets: (UI-SPEC).
	copy.Files = stacks.MarkSecretFilesFor(copy.Files, nil, composeFiles)
	if err := stacks.CheckFilesFor(copy.Files, composeFiles); err != nil {
		return Import{}, err
	}
	res := stacks.Check(ctx, stacks.CheckInput{Name: req.Name, Target: req.Target, Copy: copy})
	problems = append(problems, res.Problems...)

	ref := req.Ref
	if req.RefKind == gitx.RefCommit {
		ref = commit.SHA
	}
	return Import{
		Commit: commit, Tree: tree, Files: copy.Files, NotImported: all.Others, NotImportedTotal: all.OthersTotal,
		Problems: problems,
		Source: stackops.GitSource{
			RepoID: repo.ID, RepoName: repo.Name, RefKind: string(req.RefKind), Ref: ref, Folder: folder, ComposeFiles: composeFiles,
		},
	}, nil
}

// recordUse remembers what the fetch showed of the repository's credential. A failure to write it is logged, never
// the import's problem.
func (s *Service) recordUse(ctx context.Context, repo gitconn.Repo, err error) {
	var code gitx.Code
	var ge *gitx.Error
	if errors.As(err, &ge) {
		code = ge.Code
	}
	if rerr := s.conns.RecordUse(ctx, repo.CredentialID, code); rerr != nil {
		s.log.Error("could not store the use of a git credential", "repository", repo.ID, "error", rerr)
	}
}

// cleanLocation validates the folder and the compose file paths before any network call and returns them clean:
// the folder without surrounding slashes, each compose path without a leading "./".
func cleanLocation(folder string, compose []string) (string, []string, error) {
	folder = strings.Trim(folder, "/")
	if folder != "" && stacks.ValidPath(folder) != nil {
		return "", nil, ErrFolderInvalid
	}
	if len(compose) == 0 {
		return "", nil, ErrNoComposeFiles
	}
	if len(compose) > stacks.MaxFiles {
		return "", nil, &stacks.FileError{Err: stacks.ErrStackTooLarge}
	}
	out := make([]string, 0, len(compose))
	for _, p := range compose {
		clean := strings.TrimPrefix(p, "./")
		if stacks.ValidPath(clean) != nil || slices.Contains(out, clean) {
			return "", nil, &PathError{Path: p, Err: ErrComposePathInvalid}
		}
		out = append(out, clean)
	}
	return folder, out, nil
}

// linkProblems are the blocking problems of a stack file that is a submodule or a link that was not followed, or
// sits behind one. A link is followed only when it names a regular file inside the folder (D-10), and then it is
// stored under its own path and raises nothing. What a link outside the folder names is never read.
func linkProblems(needed []string, f gitx.Folder) stacks.Problems {
	var out stacks.Problems
	seen := map[string]bool{}
	add := func(p stacks.Problem) {
		if !seen[p.Code+p.File] {
			seen[p.Code+p.File] = true
			out = append(out, p)
		}
	}
	for _, p := range needed {
		for at := p; at != "." && at != "/" && at != ""; at = path.Dir(at) {
			link, ok := f.Links[at]
			if !ok || link.Followed {
				continue
			}
			switch {
			case link.Kind == gitx.EntrySubmodule:
				add(stacks.SubmoduleProblem(at))
			case link.Outside:
				add(stacks.SymlinkOutsideProblem(at))
			default:
				add(stacks.SymlinkUnsupportedProblem(at))
			}
		}
	}
	return out
}
