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
	// Overlays are the files Bayplan holds for paths the repository does not have (D-09). They are stored sealed, as
	// secret files with origin overlay. An overlay for a path the repository holds is not stored: it would shadow the
	// repository's file, so it is reported as the blocking problem overlay_clash and the repository's file stands.
	Overlays []Overlay
}

// Overlay is a file of Bayplan's own for a path the repository lacks.
type Overlay struct {
	Path    string
	Content []byte
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
	// Variables are the rows of the variables pane: every ${VAR} the compose files use and every variable of the table,
	// with the value a deploy would use and where it comes from. A secret or secret-like value is hidden.
	Variables []stacks.ResolvedVar
	// Services are the names of the services the compose files declare, sorted; none when they did not load.
	Services []string
	// Missing are the paths the compose files need that the repository lacks and no overlay fills, sorted. A path that
	// may be absent (the folder's .env, an env_file with required: false) is not one.
	Missing []string
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
	return s.read(ctx, req, "")
}

// read is Import. held, when it is the full ID of a commit the mirror already holds, is used without any fetch;
// otherwise the ref is fetched.
func (s *Service) read(ctx context.Context, req ImportRequest, held string) (Import, error) {
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
	overlays, err := cleanOverlays(req.Overlays)
	if err != nil {
		return Import{}, err
	}
	rem, err := s.conns.Remote(ctx, repo)
	if err != nil {
		return Import{}, err
	}
	mirror := gitx.MirrorID(repo.ID)
	commit, ok := s.heldCommit(ctx, mirror, rem, held, folder)
	if !ok {
		commit, err = s.git.FetchRef(ctx, mirror, rem, req.RefKind, req.Ref)
		s.recordUse(ctx, repo, err)
		if err != nil {
			return Import{}, err
		}
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
	wanted := make(map[string]bool, len(needed.Paths)+len(overlays))
	for _, p := range needed.Paths {
		wanted[p] = true
	}
	// A path an overlay fills is read too, so that a repository file at that path is found and the overlay refused.
	for _, o := range overlays {
		wanted[o.Path] = true
	}
	all, err := s.git.ReadFolder(ctx, mirror, commit.SHA, folder, func(rel string) bool { return wanted[rel] }, gitx.ReadLimits{})
	if err != nil {
		return Import{}, err
	}
	repoFiles := map[string]bool{}
	var read []stacks.File
	for _, f := range all.Files {
		if f.Kind != gitx.EntryFile {
			continue
		}
		repoFiles[f.Path] = true
		read = append(read, stacks.File{Path: f.Path, Content: f.Content, Origin: stacks.OriginRepo})
	}
	problems = append(problems, linkProblems(needed.Paths, all)...)

	// The check sees every overlay, so one that clashes with a repository file is named. What is stored is the files
	// the compose files need and the overlays that fill a path the repository lacks.
	checkFiles := slices.Clone(read)
	var stored []stacks.File
	for _, f := range read {
		if slices.Contains(needed.Paths, f.Path) {
			stored = append(stored, f)
		}
	}
	filled := map[string]bool{}
	for _, o := range overlays {
		f := stacks.File{Path: o.Path, Content: o.Content, Secret: true, Origin: stacks.OriginOverlay}
		checkFiles = append(checkFiles, f)
		if repoFiles[o.Path] {
			continue
		}
		filled[o.Path] = true
		stored = append(stored, f)
	}
	// The server decides which files are secret: the ones the compose files use under secrets: (UI-SPEC).
	stored = stacks.MarkSecretFilesFor(stored, nil, composeFiles)
	if err := stacks.CheckFilesFor(stored, composeFiles); err != nil {
		return Import{}, err
	}
	copy.Files = stacks.MarkSecretFilesFor(checkFiles, nil, composeFiles)
	res := stacks.Check(ctx, stacks.CheckInput{Name: req.Name, Target: req.Target, Copy: copy})
	problems = append(problems, res.Problems...)

	var missing []string
	for _, p := range needed.Paths {
		if !repoFiles[p] && !filled[p] && !slices.Contains(needed.Optional, p) {
			missing = append(missing, p)
		}
	}
	var services []string
	if res.Loaded != nil && res.Loaded.Project != nil {
		for name := range res.Loaded.Project.Services {
			services = append(services, name)
		}
		slices.Sort(services)
	}

	ref := req.Ref
	if req.RefKind == gitx.RefCommit {
		ref = commit.SHA
	}
	return Import{
		Commit: commit, Tree: tree, Files: stored, NotImported: all.Others, NotImportedTotal: all.OthersTotal,
		Problems: problems, Variables: res.Variables, Services: services, Missing: missing,
		Source: stackops.GitSource{
			RepoID: repo.ID, RepoName: repo.Name, RefKind: string(req.RefKind), Ref: ref, Folder: folder, ComposeFiles: composeFiles,
		},
	}, nil
}

// heldCommit answers the commit with the full ID sha when the mirror already holds it and has the folder, reading
// the object database only. It is not held for anything else, and then the ref is fetched. It never reaches the
// network: a full ID the mirror holds is looked up locally before any fetch is thought of.
func (s *Service) heldCommit(ctx context.Context, mirror gitx.MirrorID, rem gitx.Remote, sha, folder string) (gitx.Commit, bool) {
	if !fullSHA(sha) {
		return gitx.Commit{}, false
	}
	if _, err := s.git.FolderTree(ctx, mirror, sha, folder); err != nil {
		return gitx.Commit{}, false
	}
	c, err := s.git.FetchRef(ctx, mirror, rem, gitx.RefCommit, sha)
	if err != nil || c.SHA != sha {
		return gitx.Commit{}, false
	}
	return c, true
}

// fullSHA reports whether s is a full commit ID: 40 lower-case hex digits.
func fullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// cleanOverlays validates the overlay files before any network call: each path is a path inside the stack, none is
// named twice, and none is over the size of a file.
func cleanOverlays(in []Overlay) ([]Overlay, error) {
	seen := make(map[string]bool, len(in))
	for _, o := range in {
		if err := stacks.ValidPath(o.Path); err != nil {
			return nil, err
		}
		if seen[o.Path] {
			return nil, &stacks.FileError{Path: o.Path, Err: stacks.ErrPathTaken}
		}
		seen[o.Path] = true
		if len(o.Content) > stacks.MaxFileBytes {
			return nil, &stacks.FileError{Path: o.Path, Err: stacks.ErrFileTooLarge}
		}
	}
	return in, nil
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
