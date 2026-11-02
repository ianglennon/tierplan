// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync

import (
	"context"
	"errors"
	"slices"

	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// ErrOverlayClash means a file the person chose to keep as an overlay file is also a file of the repository, so keeping it
// would shadow the repository's. An *OverlayClashError names the path.
var ErrOverlayClash = errors.New("gitsync: a file to keep exists in the repository")

// OverlayClashError is the refusal to keep a file at a path the repository holds.
type OverlayClashError struct{ Path string }

func (e *OverlayClashError) Error() string { return ErrOverlayClash.Error() + ": " + e.Path }

// Is lets errors.Is match ErrOverlayClash.
func (e *OverlayClashError) Is(target error) bool { return target == ErrOverlayClash }

// FileChoice is what becomes of one file of the stack as it is today: kept as an overlay file (Keep), or dropped.
type FileChoice struct {
	Path string
	Keep bool
}

// ConvertRequest is an inline or adopted stack to point at a repository (D-24).
type ConvertRequest struct {
	// SourceChange is the repository, ref, folder and compose files the stack is to follow.
	SourceChange
	// Version is the version of the draft the person read; a draft that moved since is refused.
	Version int64
	// Files are the choices for the files of the stack's draft. A file that is not named is dropped.
	Files []FileChoice
	// Vars are values to set over the stack's variables, by name. A variable the stack holds and this does not name stays
	// as it is, a secret one included.
	Vars []stacks.Var
	// Overlays are new overlay files, for paths the repository lacks (D-09), beside the files kept from the draft. A path
	// named both here and in Files as kept is stacks.ErrPathTaken.
	Overlays []Overlay
}

// ConvertResult is what a Convert did to the stack's draft.
type ConvertResult struct {
	PullResult
	// Kept is how many files became overlay files and Dropped how many files of the stack as it was were left behind.
	Kept, Dropped int
}

// Convert points an inline or adopted stack at a repository, through review (D-24). It reads the repository at the head of
// the ref and writes the draft: the repository's files, the files the person kept as overlay files (sealed, secret), and the
// stack's variables with Vars set over them. It applies nothing: the stack stays inline or adopted, with its revisions, until
// a deploy of the draft ends good, which makes it a git stack (Deployed) and keeps its history. Discarding the draft leaves
// the stack exactly as it was.
//
// The repository must be one of the environment or an instance repository, else it is gitconn.ErrNotFound. A stack that is a
// git stack is stackops.ErrAlreadyGit. The draft must still be at Version, or the answer is a *stackops.DraftConflictError
// and nothing is stored. A file to keep that the repository holds is an *OverlayClashError, and a file that is not one of
// the draft's is a *stacks.FileError; the import's own failures are returned as the importer returns them. In every refusal
// the draft is untouched.
//
// The result names the commit the draft was made from; there is no deployed commit and no count of commits.
func (s *Service) Convert(ctx context.Context, envID, stackID, actor string, r ConvertRequest) (ConvertResult, error) {
	st, err := s.repo.Stack(ctx, envID, stackID)
	if err != nil {
		return ConvertResult{}, err
	}
	if st.Source.Kind == stacks.SourceGit {
		return ConvertResult{}, stackops.ErrAlreadyGit
	}
	draft, err := s.repo.Draft(ctx, stackID)
	if err != nil {
		return ConvertResult{}, err
	}
	if draft.Version != r.Version {
		return ConvertResult{}, &stackops.DraftConflictError{SavedBy: draft.CreatedBy, SavedAt: draft.CreatedAt, Version: draft.Version}
	}

	var overlays []Overlay
	seen := make(map[string]bool, len(r.Files))
	for _, c := range r.Files {
		if seen[c.Path] {
			return ConvertResult{}, &stacks.FileError{Path: c.Path, Err: stacks.ErrPathTaken}
		}
		seen[c.Path] = true
		i := slices.IndexFunc(draft.Copy.Files, func(f stacks.File) bool { return f.Path == c.Path })
		if i < 0 {
			return ConvertResult{}, &stacks.FileError{Path: c.Path, Err: stacks.ErrPathInvalid}
		}
		if c.Keep {
			overlays = append(overlays, Overlay{Path: c.Path, Content: draft.Copy.Files[i].Content})
		}
	}
	kept := len(overlays)
	overlays = append(overlays, r.Overlays...)
	vars := mergeVars(draft.Copy.Vars, r.Vars)

	held := ""
	if r.RefKind == gitx.RefCommit {
		held = r.Ref
	}
	imp, err := s.read(ctx, ImportRequest{
		EnvID: st.EnvID, RepoID: r.RepoID, RefKind: r.RefKind, Ref: r.Ref, Folder: r.Folder, ComposeFiles: r.ComposeFiles,
		Name: st.Name, Target: st.Target, Vars: vars, Overlays: overlays,
	}, held)
	if err != nil {
		return ConvertResult{}, err
	}
	for _, p := range imp.Problems {
		if p.Code == stacks.CodeOverlayClash {
			return ConvertResult{}, &OverlayClashError{Path: p.File}
		}
	}

	saved, err := s.repo.CreatePendingGitSource(ctx, stackID, r.Version, actor, imp.Copy(vars), imp.Revision())
	if err != nil {
		return ConvertResult{}, err
	}
	return ConvertResult{PullResult: PullResult{DraftVersion: saved.Version, Commit: imp.Commit, Ref: imp.Source.Ref}, Kept: kept, Dropped: len(draft.Copy.Files) - kept}, nil
}

// mergeVars is the stack's variables with the set ones over them: a name in set takes its value and secrecy, a name only in
// current stays, and a name only in set is added.
func mergeVars(current, set []stacks.Var) []stacks.Var {
	out := slices.Clone(current)
	for _, v := range set {
		if i := slices.IndexFunc(out, func(c stacks.Var) bool { return c.Name == v.Name }); i >= 0 {
			out[i] = v
		} else {
			out = append(out, v)
		}
	}
	return out
}
