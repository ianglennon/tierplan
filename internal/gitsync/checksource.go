// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync

import (
	"context"

	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// CheckSourceRequest is a repository folder and compose files to judge before any stack is made from them.
type CheckSourceRequest struct {
	// ImportRequest says where the stack would come from and which variables it would have. Its Overlays are
	// ignored: a check never receives the contents of an overlay file, only their paths (OverlayPaths).
	ImportRequest
	// Commit is the full ID of the commit an earlier check answered for this ref. When the repository's mirror holds
	// it the check reads the mirror and fetches nothing; otherwise the ref is fetched as for an import.
	Commit string
	// OverlayPaths are the paths of the overlay files the person means to add. They count as present, so a path the
	// compose files need that an overlay will fill is not missing, and one the repository holds is overlay_clash.
	OverlayPaths []string
}

// SourceCheck is what a repository folder and its compose files would give, found without saving anything.
type SourceCheck struct {
	// Commit is the commit the check read.
	Commit gitx.Commit
	// Services are the names of the services the compose files declare, sorted.
	Services []string
	// Problems are everything the check found, blocking or not, with file and line.
	Problems []stacks.Problem
	// Vars are the variables with the value a deploy would use and where it comes from. A secret or secret-like value
	// is hidden.
	Vars []stacks.ResolvedVar
	// Missing are the paths the compose files need that the repository lacks and no overlay path fills.
	Missing []string
	// NotImported are the first names of the folder's other files and NotImportedTotal how many there are.
	NotImported      []string
	NotImportedTotal int
}

// CheckSource reads a repository folder as an import would, and answers what it would give: the services, the
// problems, every variable with where its value comes from (the table, the repository's .env, a default or nowhere,
// GIT-08) and the files the compose files need that neither the repository nor an overlay path supplies. It stores
// nothing and handles no overlay content. It answers the errors of Import.
func (s *Service) CheckSource(ctx context.Context, req CheckSourceRequest) (SourceCheck, error) {
	in := req.ImportRequest
	in.Overlays = make([]Overlay, 0, len(req.OverlayPaths))
	for _, p := range req.OverlayPaths {
		in.Overlays = append(in.Overlays, Overlay{Path: p})
	}
	got, err := s.read(ctx, in, req.Commit)
	if err != nil {
		return SourceCheck{}, err
	}
	return SourceCheck{
		Commit: got.Commit, Services: got.Services, Problems: stacks.Problems(got.Problems).Sorted(), Vars: got.Variables, Missing: got.Missing,
		NotImported: got.NotImported, NotImportedTotal: got.NotImportedTotal,
	}, nil
}
