// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package gitsync reads a stack's files from a git repository and, in the plans that follow, keeps the stack in
// step with it. It imports gitx (the one place go-git is used), gitconn (repositories and their credentials),
// stacks (the stack model) and stackops (where a stack is stored); stackops never imports gitx.
package gitsync

import (
	"log/slog"
	"time"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// Deps is what a Service needs.
type Deps struct {
	// Git reads the repository and its mirror.
	Git gitx.Client
	// Conns holds the repositories and their credentials.
	Conns *gitconn.Store
	// Repo stores the stacks.
	Repo stackops.Repo
	Log  *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Service imports the files of a stack from a repository folder.
type Service struct {
	git   gitx.Client
	conns *gitconn.Store
	repo  stackops.Repo
	log   *slog.Logger
	now   func() time.Time
}

// New returns a Service.
func New(d Deps) *Service {
	s := &Service{git: d.Git, conns: d.Conns, repo: d.Repo, log: d.Log, now: d.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}
